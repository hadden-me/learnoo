package courses

// Course submission, newer-revision notices and amendments (LMS-B04,
// migrations/0015_course_review_publication.sql, docs/LMS_CONTRACT.md §5
// and §6). Review decisions are in review.go, publication and the
// administrator lifecycle in publication.go.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrIncomplete means the revision lacks a required field or an outline;
	// errors.As with *IncompleteError lists what.
	ErrIncomplete = errors.New("course revision is incomplete")
	// ErrIneligible means a step's pinned lesson revision is not eligible;
	// errors.As with *IneligibleError lists the steps.
	ErrIneligible = errors.New("a step is not eligible")
	// ErrDraftExists means an amendment was asked for while a draft is open.
	ErrDraftExists = errors.New("the course already has an open draft")
	// ErrNotPublished means the course has no comparison revision (it was
	// never published), or no published revision to suspend.
	ErrNotPublished = errors.New("course is not published")
	// ErrNothingToAmend means no step has a newer eligible revision.
	ErrNothingToAmend = errors.New("no step has a newer eligible revision")
	// ErrNotAmendable means the replacements cannot form an amendment (for
	// example the cover is not an image the current owner uploaded).
	ErrNotAmendable = errors.New("these replacements cannot form an amendment")
)

// IncompleteError lists the missing parts of a revision: title, summary,
// objectives, modules (none), or module:<key> (a module without a step).
type IncompleteError struct{ Fields []string }

func (e *IncompleteError) Error() string {
	return ErrIncomplete.Error() + ": " + strings.Join(e.Fields, ", ")
}

// Is makes errors.Is(err, ErrIncomplete) hold.
func (e *IncompleteError) Is(target error) bool { return target == ErrIncomplete }

// IneligibleStep is a step whose pinned lesson revision is not eligible.
type IneligibleStep struct {
	ModuleKey  string
	Position   int
	Title      string
	LessonID   int64
	RevisionID int64
}

// IneligibleError lists the ineligible steps.
type IneligibleError struct{ Steps []IneligibleStep }

func (e *IneligibleError) Error() string {
	return fmt.Sprintf("%s: %d step(s)", ErrIneligible.Error(), len(e.Steps))
}

// Is makes errors.Is(err, ErrIneligible) hold.
func (e *IneligibleError) Is(target error) bool { return target == ErrIneligible }

// Decision is a review decision on a course revision.
type Decision struct {
	Action       string
	ReviewerID   int64
	ReviewerName string
	Comment      *string
	DecidedAt    time.Time
}

// Blocked describes a revision in review that no active reviewer of its
// subject may decide (contract §6): every reviewer's disqualifications, or
// none at all when the subject has no active reviewer.
type Blocked struct {
	Reasons []ReviewerProblem
}

// ReviewerProblem is one disqualification of one reviewer (ReviewerID 0
// and Reason "no_subject_reviewer" when the subject has none).
type ReviewerProblem struct {
	ReviewerID       int64
	ReviewerName     string
	Reason           string
	LessonID         *int64
	LessonRevisionID *int64
}

// ---- shared helpers ----

// tx runs fn in a READ COMMITTED transaction after checking that actor
// holds one of perms (share-locking the grant and account rows first, as
// contract §2 requires). With no perms, no global permission is checked.
func (s *Store) tx(ctx context.Context, actor int64, perms []permissions.Permission, fn func(tx pgx.Tx) error) error {
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if len(perms) > 0 {
			if err := permissions.RequireAnyTx(ctx, tx, actor, perms...); err != nil {
				return err
			}
		}
		return fn(tx)
	})
	return mapErr(err)
}

func loadDecision(ctx context.Context, tx pgx.Tx, revisionID int64) (*Decision, error) {
	var d Decision
	err := tx.QueryRow(ctx, `SELECT d.action, d.reviewer_id, u.display_name, d.comment, d.decided_at
		FROM course_review_decisions d JOIN users u ON u.id = d.reviewer_id WHERE d.revision_id = $1`,
		revisionID).Scan(&d.Action, &d.ReviewerID, &d.ReviewerName, &d.Comment, &d.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// reviewBlocked returns nil when an active reviewer of the subject may
// decide on the revision, else every active reviewer's disqualifications
// (course_reviewer_problems).
func reviewBlocked(ctx context.Context, tx pgx.Tx, revisionID, _ int64) (*Blocked, error) {
	m, err := reviewBlockedBatch(ctx, tx, []int64{revisionID})
	return m[revisionID], err
}

// reviewBlockedBatch computes reviewBlocked for several revisions in one
// query: the result has an entry only for blocked revisions.
func reviewBlockedBatch(ctx context.Context, tx pgx.Tx, revisionIDs []int64) (map[int64]*Blocked, error) {
	out := map[int64]*Blocked{}
	if len(revisionIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT cr.id, a.user_id, u.display_name, p.reason, p.lesson_id, p.lesson_revision_id
		  FROM unnest($1::bigint[]) AS want(id)
		  JOIN course_revisions cr ON cr.id = want.id
		  LEFT JOIN (reviewer_assignments a JOIN users u ON u.id = a.user_id AND u.disabled_at IS NULL)
		         ON a.subject_id = cr.subject_id
		  LEFT JOIN LATERAL course_reviewer_problems(a.user_id, cr.id) p ON a.user_id IS NOT NULL
		 ORDER BY cr.id, a.user_id, p.reason, p.lesson_id`, revisionIDs)
	if err != nil {
		return nil, err
	}
	type row struct {
		revision int64
		reviewer *int64
		name     *string
		reason   *string
		lesson   *int64
		lrev     *int64
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.revision, &x.reviewer, &x.name, &x.reason, &x.lesson, &x.lrev)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	eligible := map[int64]bool{}
	for _, x := range list {
		if x.reviewer != nil && x.reason == nil {
			eligible[x.revision] = true
		}
	}
	for _, x := range list {
		if eligible[x.revision] {
			continue
		}
		b := out[x.revision]
		if b == nil {
			b = &Blocked{Reasons: []ReviewerProblem{}}
			out[x.revision] = b
		}
		if x.reviewer == nil {
			b.Reasons = append(b.Reasons, ReviewerProblem{Reason: "no_subject_reviewer"})
			continue
		}
		b.Reasons = append(b.Reasons, ReviewerProblem{ReviewerID: *x.reviewer, ReviewerName: *x.name, Reason: *x.reason,
			LessonID: x.lesson, LessonRevisionID: x.lrev})
	}
	return out, nil
}

// ineligibleSteps lists the steps of a revision whose pinned revision is
// not eligible (course_step_revision_eligible), in outline order.
func ineligibleSteps(ctx context.Context, tx pgx.Tx, revisionID int64) ([]IneligibleStep, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.module_key::text, s.position, s.title, s.lesson_id, s.lesson_revision_id
		  FROM course_steps s JOIN course_modules m ON m.course_revision_id = s.course_revision_id AND m.module_key = s.module_key
		 WHERE s.course_revision_id = $1 AND NOT course_step_revision_eligible(s.lesson_id, s.lesson_revision_id)
		 ORDER BY m.position, s.position`, revisionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IneligibleStep, error) {
		var st IneligibleStep
		err := r.Scan(&st.ModuleKey, &st.Position, &st.Title, &st.LessonID, &st.RevisionID)
		return st, err
	})
}

// revisionDependencies reads, unlocked, what a revision's checks must lock:
// its assignments, step lessons and pinned revisions.
func revisionDependencies(ctx context.Context, tx pgx.Tx, revisionID int64) (assignments, lessonIDs, revisionIDs []int64, err error) {
	err = tx.QueryRow(ctx, `
		SELECT ARRAY(SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = $1),
		       ARRAY(SELECT lesson_id FROM course_steps WHERE course_revision_id = $1),
		       ARRAY(SELECT lesson_revision_id FROM course_steps WHERE course_revision_id = $1)`,
		revisionID).Scan(&assignments, &lessonIDs, &revisionIDs)
	return
}

// lockEditionsAndLessons takes the contract §2 locks a submission,
// publication or restoration needs: editions, lessons, pinned revisions,
// all FOR SHARE in ascending ID. Unlike a save, it does not lock sections,
// levels, subjects or chapters: it selects nothing new.
func lockEditionsAndLessons(ctx context.Context, tx pgx.Tx, assignments, lessonIDs, revisionIDs []int64) error {
	for _, l := range []struct {
		ids []int64
		sql string
	}{
		{assignments, `SELECT id FROM curriculum_editions
			 WHERE id IN (SELECT edition_id FROM curriculum_assignments WHERE id = ANY ($1)) ORDER BY id FOR SHARE`},
		{lessonIDs, `SELECT id FROM lessons WHERE id = ANY ($1) ORDER BY id FOR SHARE`},
		{revisionIDs, `SELECT id FROM lesson_revisions WHERE id = ANY ($1) ORDER BY id FOR SHARE`},
	} {
		if len(l.ids) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, l.sql, l.ids); err != nil {
			return err
		}
	}
	return nil
}

// ---- submission ----

// Submit submits actor's draft at expectedVersion for review. It requires
// a complete revision (title, summary, objectives, at least one module,
// every module with a step: *IncompleteError), every step eligible
// (*IneligibleError), the placement valid in submit mode
// (ErrPlacementInvalid) and a course that is not archived. The submitting
// UPDATE compares expectedVersion and changes only the status, submitted_at,
// updated_at and version; the course_submission_audit row is written in the
// same transaction. If the stored kind no longer matches the derived kind,
// it is corrected first (one more version).
func (s *Store) Submit(ctx context.Context, actor, revisionID int64, expectedVersion int) (Revision, error) {
	var rev Revision
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		courseID, err := ownedCourseOf(ctx, tx, actor, revisionID)
		if err != nil {
			return err
		}
		assignments, lessonIDs, revisionIDs, err := revisionDependencies(ctx, tx, revisionID)
		if err != nil {
			return err
		}
		if err := lockEditionsAndLessons(ctx, tx, assignments, lessonIDs, revisionIDs); err != nil {
			return err
		}
		c, err := lockDraft(ctx, tx, actor, courseID, revisionID, expectedVersion)
		if err != nil {
			return err
		}
		if c.archived {
			return ErrArchived
		}
		// Any change of the children since the unlocked read above bumped
		// the version, which lockDraft compared under the revision lock.
		var title, summary, objectives string
		var empty []string
		if err := tx.QueryRow(ctx, `
			SELECT r.title, r.summary, r.objectives,
			       ARRAY(SELECT m.module_key::text FROM course_modules m WHERE m.course_revision_id = r.id
			              AND NOT EXISTS (SELECT 1 FROM course_steps s WHERE s.course_revision_id = r.id AND s.module_key = m.module_key)
			              ORDER BY m.position)
			  FROM course_revisions r WHERE r.id = $1`, revisionID).Scan(&title, &summary, &objectives, &empty); err != nil {
			return err
		}
		inc := &IncompleteError{}
		for _, f := range []struct{ name, v string }{{"title", title}, {"summary", summary}, {"objectives", objectives}} {
			if f.v == "" {
				inc.Fields = append(inc.Fields, f.name)
			}
		}
		var modules int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM course_modules WHERE course_revision_id = $1`, revisionID).Scan(&modules); err != nil {
			return err
		}
		if modules == 0 {
			inc.Fields = append(inc.Fields, "modules")
		}
		for _, k := range empty {
			inc.Fields = append(inc.Fields, "module:"+k)
		}
		if len(inc.Fields) > 0 {
			return inc
		}
		bad, err := ineligibleSteps(ctx, tx, revisionID)
		if err != nil {
			return err
		}
		if len(bad) > 0 {
			return &IneligibleError{Steps: bad}
		}
		if err := placementProblems(ctx, tx, revisionID, "submit"); err != nil {
			return err
		}
		version := expectedVersion
		tag, err := tx.Exec(ctx, `
			UPDATE course_revisions SET kind = course_revision_derived_kind(id), version = version + 1, updated_at = now()
			 WHERE id = $1 AND version = $2 AND status = 'draft' AND kind <> course_revision_derived_kind(id)`, revisionID, version)
		if err != nil {
			return err
		}
		version += int(tag.RowsAffected())
		if _, err := tx.Exec(ctx, `INSERT INTO course_submission_audit (course_id, revision_id, actor_id) VALUES ($1, $2, $3)`,
			courseID, revisionID, actor); err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `
			UPDATE course_revisions SET status = 'in_review', submitted_at = now(), updated_at = now(), version = version + 1
			 WHERE id = $1 AND version = $2 AND status = 'draft' AND author_id = $3`, revisionID, version, actor)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrVersionConflict
		}
		rev, err = loadRevision(ctx, tx, revisionID)
		return err
	})
	return rev, err
}

// ---- newer revision notice and amendments ----

// NewerRevision is a step of the course's comparison revision whose lesson
// has a newer eligible revision.
type NewerRevision struct {
	ModuleKey        string
	Position         int
	Title            string
	LessonID         int64
	PinnedRevisionID int64
	PinnedNumber     int
	NewerRevisionID  int64
	NewerNumber      int
}

// Notice is the newer revision notice of a course (contract §5).
type Notice struct {
	CourseID           int64
	ComparisonRevision *int64
	Steps              []NewerRevision
}

// canSeeNotice reports whether actor may read the course's notice: its
// owner, an active reviewer of its subject, a publisher or an
// administrator. Never students. Grant rows are share-locked.
func canSeeNotice(ctx context.Context, tx pgx.Tx, actor, courseID int64) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM courses c JOIN users u ON u.id = c.owner_id
		                WHERE c.id = $2 AND c.owner_id = $1 AND u.disabled_at IS NULL
		                  AND EXISTS (SELECT 1 FROM user_permissions p WHERE p.user_id = $1 AND p.permission = 'author'))
		    OR EXISTS (SELECT 1 FROM courses c JOIN reviewer_assignments a ON a.subject_id = c.subject_id AND a.user_id = $1
		                JOIN users u ON u.id = a.user_id AND u.disabled_at IS NULL WHERE c.id = $2)
		    OR EXISTS (SELECT 1 FROM user_permissions p JOIN users u ON u.id = p.user_id AND u.disabled_at IS NULL
		                WHERE p.user_id = $1 AND p.permission IN ('publisher', 'administrator'))`, actor, courseID).Scan(&ok)
	return ok, err
}

// NewerRevisions returns the newer revision notice of a course to its
// owner, a reviewer of its subject, a publisher or an administrator;
// anyone else, and a missing course, gets ErrNotFound.
func (s *Store) NewerRevisions(ctx context.Context, actor, courseID int64) (Notice, error) {
	n := Notice{CourseID: courseID, Steps: []NewerRevision{}}
	err := s.tx(ctx, actor, nil, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM courses WHERE id = $1)`, courseID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		ok, err := canSeeNotice(ctx, tx, actor, courseID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if err := tx.QueryRow(ctx, `SELECT course_comparison_revision($1)`, courseID).Scan(&n.ComparisonRevision); err != nil {
			return err
		}
		n.Steps, err = newerRevisions(ctx, tx, courseID)
		return err
	})
	return n, err
}

func newerRevisions(ctx context.Context, tx pgx.Tx, courseID int64) ([]NewerRevision, error) {
	rows, err := tx.Query(ctx, `
		SELECT n.module_key::text, n.step_position, n.step_title, n.lesson_id, n.pinned_revision_id, n.pinned_number,
		       n.newer_revision_id, n.newer_number
		  FROM course_newer_revisions($1) n
		  JOIN course_modules m ON m.course_revision_id = course_comparison_revision($1) AND m.module_key = n.module_key
		 ORDER BY m.position, n.step_position`, courseID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (NewerRevision, error) {
		var x NewerRevision
		err := r.Scan(&x.ModuleKey, &x.Position, &x.Title, &x.LessonID, &x.PinnedRevisionID, &x.PinnedNumber,
			&x.NewerRevisionID, &x.NewerNumber)
		return x, err
	})
	if list == nil {
		list = []NewerRevision{}
	}
	return list, err
}

// Replacement chooses a newer revision for the step of a lesson.
type Replacement struct {
	LessonID   int64
	RevisionID int64
}

// CreateAmendment creates, in one action, an amendment of actor's course
// (contract §6): a draft copying the comparison revision whose only change
// is that each chosen step pins the chosen newer eligible revision of the
// same lesson. An empty choice takes every newer eligible revision of the
// notice (the newest of each lesson). It returns ErrNotPublished without a
// comparison revision, ErrDraftExists while any draft of the course is
// open, ErrActiveCandidateExists while another revision is in review or
// approved awaiting publication, ErrArchived for an archived course,
// ErrNothingToAmend when nothing would change, and ErrInvalidStep for a
// choice that is not a step's lesson with a newer eligible revision, or for
// an unchanged step of another author's lesson that is no longer eligible
// (an amendment cannot carry it; use an ordinary revision). The placement is
// carried unchanged whatever the state of its editions. The course is locked
// FOR UPDATE, like draft creation.
func (s *Store) CreateAmendment(ctx context.Context, actor, courseID int64, choices []Replacement) (Revision, error) {
	seen := map[int64]bool{}
	for _, c := range choices {
		if c.LessonID <= 0 || c.RevisionID <= 0 || seen[c.LessonID] {
			return Revision{}, invalidStep("each step names a lesson and a revision, once")
		}
		seen[c.LessonID] = true
	}
	var rev Revision
	err := s.write(ctx, actor, func(tx pgx.Tx) error {
		if err := ownedCourse(ctx, tx, actor, courseID); err != nil {
			return err
		}
		var compare *int64
		if err := tx.QueryRow(ctx, `SELECT course_comparison_revision($1)`, courseID).Scan(&compare); err != nil {
			return err
		}
		if compare == nil {
			return ErrNotPublished
		}
		assignments, lessonIDs, revisionIDs, err := revisionDependencies(ctx, tx, *compare)
		if err != nil {
			return err
		}
		chosen := map[int64]int64{}
		for _, c := range choices {
			chosen[c.LessonID] = c.RevisionID
			revisionIDs = append(revisionIDs, c.RevisionID)
		}
		if len(choices) == 0 {
			notice, err := newerRevisions(ctx, tx, courseID)
			if err != nil {
				return err
			}
			for _, n := range notice {
				chosen[n.LessonID] = n.NewerRevisionID
				revisionIDs = append(revisionIDs, n.NewerRevisionID)
			}
		}
		if len(chosen) == 0 {
			return ErrNothingToAmend
		}
		if err := lockEditionsAndLessons(ctx, tx, assignments, lessonIDs, revisionIDs); err != nil {
			return err
		}
		c, err := lockCourse(ctx, tx, actor, courseID, "FOR UPDATE")
		if err != nil {
			return err
		}
		if c.archived {
			return ErrArchived
		}
		var again *int64
		if err := tx.QueryRow(ctx, `SELECT course_comparison_revision($1)`, courseID).Scan(&again); err != nil {
			return err
		}
		if !sameID(compare, again) {
			return ErrRetry
		}
		var draft, active bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM course_revisions WHERE course_id = $1 AND status = 'draft'),
			       EXISTS (SELECT 1 FROM course_revisions WHERE course_id = $1 AND withdrawn_at IS NULL
			                  AND (status = 'in_review' OR (status = 'approved' AND candidate_closed_at IS NULL)))`,
			courseID).Scan(&draft, &active); err != nil {
			return err
		}
		switch {
		case draft:
			return ErrDraftExists
		case active:
			return ErrActiveCandidateExists
		}
		// Every choice under the locks: a step of the comparison revision
		// whose lesson gets a newer eligible revision of itself.
		for lesson, revision := range chosen {
			var ok bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM course_steps s
				                 JOIN lesson_revisions pr ON pr.id = s.lesson_revision_id
				                 JOIN lesson_revisions nr ON nr.id = $3 AND nr.lesson_id = s.lesson_id
				                WHERE s.course_revision_id = $1 AND s.lesson_id = $2
				                  AND nr.revision_number > pr.revision_number
				                  AND course_step_revision_eligible(nr.lesson_id, nr.id))`, *compare, lesson, revision).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return invalidStep("lesson %d is not a step of the published course, or revision %d is not a newer eligible revision of it", lesson, revision)
			}
		}
		var lessons, revisions []int64
		for l, r := range chosen {
			lessons = append(lessons, l)
			revisions = append(revisions, r)
		}
		var stuck *int64
		err = tx.QueryRow(ctx, `
			SELECT s.lesson_id FROM course_steps s JOIN lessons l ON l.id = s.lesson_id
			 WHERE s.course_revision_id = $1 AND NOT s.lesson_id = ANY ($2) AND l.created_by <> $3
			   AND NOT course_step_revision_eligible(s.lesson_id, s.lesson_revision_id)
			 ORDER BY s.lesson_id LIMIT 1`, *compare, lessons, actor).Scan(&stuck)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if stuck != nil {
			return invalidStep("the step of lesson %d pins a revision of another author's lesson that is no longer eligible; replace it too, or start an ordinary revision", *stuck)
		}
		var coverOK bool
		if err := tx.QueryRow(ctx, `
			SELECT r.cover_media_id IS NULL OR EXISTS (SELECT 1 FROM media_uploads u WHERE u.user_id = $2 AND u.media_asset_id = r.cover_media_id)
			  FROM course_revisions r WHERE r.id = $1`, *compare, actor).Scan(&coverOK); err != nil {
			return err
		}
		if !coverOK {
			return fmt.Errorf("%w: the published cover was uploaded by another user; start an ordinary revision", ErrNotAmendable)
		}
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO course_revisions (course_id, subject_id, language, revision_number, kind, author_id,
				base_revision_id, compare_revision_id, title, summary, objectives, prerequisites,
				cover_media_id, cover_alt, created_at, updated_at)
			SELECT $1, subject_id, language, $2, 'amendment', $3, $4, id, title, summary, objectives, prerequisites,
			       cover_media_id, cover_alt, now(), now()
			  FROM course_revisions WHERE id = $5
			RETURNING id`, courseID, c.last+1, actor, c.published, *compare).Scan(&id); err != nil {
			return err
		}
		for _, q := range []string{
			`INSERT INTO course_revision_assignments (course_revision_id, subject_id, assignment_id)
			 SELECT $1, subject_id, assignment_id FROM course_revision_assignments WHERE course_revision_id = $2`,
			`INSERT INTO course_modules (course_revision_id, subject_id, language, module_key, position, title, chapter_id)
			 SELECT $1, subject_id, language, module_key, position, title, chapter_id FROM course_modules WHERE course_revision_id = $2`,
		} {
			if _, err := tx.Exec(ctx, q, id, *compare); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO course_steps (course_revision_id, module_key, position, title, lesson_id, subject_id, language, lesson_revision_id)
			SELECT $1, s.module_key, s.position, s.title, s.lesson_id, s.subject_id, s.language, coalesce(c.revision, s.lesson_revision_id)
			  FROM course_steps s
			  LEFT JOIN unnest($3::bigint[], $4::bigint[]) AS c(lesson, revision) ON c.lesson = s.lesson_id
			 WHERE s.course_revision_id = $2`, id, *compare, lessons, revisions); err != nil {
			return err
		}
		var kind string
		if err := tx.QueryRow(ctx, `SELECT course_revision_derived_kind($1)`, id).Scan(&kind); err != nil {
			return err
		}
		if kind != "amendment" {
			return ErrNotAmendable
		}
		rev, err = loadRevision(ctx, tx, id)
		return err
	})
	return rev, err
}
