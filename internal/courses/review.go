package courses

// Independent course review (LMS-B04, docs/LMS_CONTRACT.md §6): decisions,
// the review queue, the review payload with its change list, and the
// administrators' view of revisions no one may review.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
)

// MaxComment bounds a review comment, in characters (database check too).
const MaxComment = 5000

// MaxQueue bounds the review and publication queues.
const MaxQueue = 200

var (
	// ErrNotInReview means the revision is not (or no longer) in review.
	ErrNotInReview = errors.New("course revision is not in review")
	// ErrNotEligibleReviewer means the caller may not decide on the
	// revision; errors.As with *ReviewerError gives the reasons.
	ErrNotEligibleReviewer = errors.New("not an eligible reviewer of this course revision")
	// ErrCommentRequired means a change request has no comment.
	ErrCommentRequired = errors.New("a comment is required to request changes")
	// ErrInvalidComment means the comment is too long or not valid text.
	ErrInvalidComment = errors.New("invalid comment")
	// ErrWithdrawn means the revision was withdrawn: no decision,
	// publication or restoration can use it.
	ErrWithdrawn = errors.New("course revision is withdrawn")
)

// ReviewerError lists why the caller may not review a revision.
type ReviewerError struct{ Reasons []ReviewerProblem }

func (e *ReviewerError) Error() string {
	var rs []string
	for _, r := range e.Reasons {
		rs = append(rs, r.Reason)
	}
	return ErrNotEligibleReviewer.Error() + ": " + strings.Join(rs, ", ")
}

// Is makes errors.Is(err, ErrNotEligibleReviewer) hold.
func (e *ReviewerError) Is(target error) bool { return target == ErrNotEligibleReviewer }

func normalizeComment(c string) (string, error) {
	c = strings.TrimSpace(c)
	if !utf8.ValidString(c) || strings.ContainsRune(c, 0) || utf8.RuneCountInString(c) > MaxComment {
		return "", fmt.Errorf("%w: at most %d characters of valid text", ErrInvalidComment, MaxComment)
	}
	return c, nil
}

// Approve approves the course revision in review, as actor.
func (s *Store) Approve(ctx context.Context, actor, revisionID int64, comment string) (Revision, error) {
	return s.decide(ctx, actor, revisionID, "approve", comment)
}

// RequestChanges sends the course revision back with a required comment.
// The owner then starts a correction draft (CreateDraft copies it).
func (s *Store) RequestChanges(ctx context.Context, actor, revisionID int64, comment string) (Revision, error) {
	return s.decide(ctx, actor, revisionID, "request_changes", comment)
}

// decide checks, in contract §2 order: the reviewer assignment of the
// revision's subject (share-locked, permissions.RequireReviewerTx), the
// course FOR SHARE, the revision FOR UPDATE; then that the revision is in
// review and not withdrawn and that course_reviewer_eligible accepts actor
// (*ReviewerError otherwise). No role bypasses it. A reviewer of another
// subject gets ErrNotFound, as for a missing revision.
func (s *Store) decide(ctx context.Context, actor, revisionID int64, action, comment string) (Revision, error) {
	comment, err := normalizeComment(comment)
	if err != nil {
		return Revision{}, err
	}
	if action == "request_changes" && comment == "" {
		return Revision{}, ErrCommentRequired
	}
	var rev Revision
	err = s.tx(ctx, actor, nil, func(tx pgx.Tx) error {
		var courseID, subject int64
		err := tx.QueryRow(ctx, `SELECT course_id, subject_id FROM course_revisions WHERE id = $1`, revisionID).Scan(&courseID, &subject)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := permissions.RequireReviewerTx(ctx, tx, actor, subject); err != nil {
			if errors.Is(err, permissions.ErrDenied) && !deniedWithCause(err) {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM courses WHERE id = $1 FOR SHARE`, courseID); err != nil {
			return err
		}
		var status Status
		var withdrawn bool
		if err := tx.QueryRow(ctx, `SELECT status, withdrawn_at IS NOT NULL FROM course_revisions WHERE id = $1 FOR UPDATE`,
			revisionID).Scan(&status, &withdrawn); err != nil {
			return err
		}
		switch {
		case status != StatusInReview:
			return ErrNotInReview
		case withdrawn:
			return ErrWithdrawn
		}
		problems, err := reviewerProblems(ctx, tx, actor, revisionID)
		if err != nil {
			return err
		}
		if len(problems) > 0 {
			return &ReviewerError{Reasons: problems}
		}
		var c *string
		if comment != "" {
			c = &comment
		}
		if _, err := tx.Exec(ctx, `INSERT INTO course_review_decisions (course_id, revision_id, reviewer_id, action, comment)
			VALUES ($1, $2, $3, $4, $5)`, courseID, revisionID, actor, action, c); err != nil {
			return err
		}
		next := StatusApproved
		if action == "request_changes" {
			next = StatusChangesRequested
		}
		if _, err := tx.Exec(ctx, `UPDATE course_revisions SET status = $2, updated_at = now() WHERE id = $1 AND status = 'in_review'`,
			revisionID, string(next)); err != nil {
			return err
		}
		rev, err = loadRevision(ctx, tx, revisionID)
		return err
	})
	return rev, err
}

func reviewerProblems(ctx context.Context, tx pgx.Tx, actor, revisionID int64) ([]ReviewerProblem, error) {
	rows, err := tx.Query(ctx, `SELECT reason, lesson_id, lesson_revision_id FROM course_reviewer_problems($1, $2)
		ORDER BY reason, lesson_id`, actor, revisionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReviewerProblem, error) {
		p := ReviewerProblem{ReviewerID: actor}
		err := r.Scan(&p.Reason, &p.LessonID, &p.LessonRevisionID)
		return p, err
	})
}

// deniedWithCause reports an ErrDenied joined with the database error that
// prevented the check: a failure, not a denial.
func deniedWithCause(err error) bool {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		for _, e := range joined.Unwrap() {
			if !errors.Is(e, permissions.ErrDenied) {
				return true
			}
		}
	}
	return false
}

// ReviewItem is a course revision in a review or administration queue.
type ReviewItem struct {
	RevisionID  int64
	CourseID    int64
	Number      int
	Kind        string
	Title       string
	Language    string
	SubjectID   int64
	AuthorID    int64
	AuthorName  string
	SubmittedAt time.Time
	// Eligible: the caller may decide on it (course_reviewer_eligible).
	Eligible bool
	// Blocked is set when no active reviewer of the subject may.
	Blocked *Blocked
}

// ReviewQueue lists the course revisions in review (not withdrawn) of the
// subjects actor is an active reviewer of, oldest submission first, with
// Eligible for actor and Blocked. Unlike the lesson queue it includes
// revisions actor may not decide on, so that every reviewer sees the gap.
// subjectID, if non-zero, restricts it to one subject (ErrNotFound when
// actor does not review it).
func (s *Store) ReviewQueue(ctx context.Context, actor, subjectID int64) ([]ReviewItem, error) {
	var items []ReviewItem
	err := s.tx(ctx, actor, nil, func(tx pgx.Tx) error {
		if subjectID != 0 {
			if err := permissions.RequireReviewerTx(ctx, tx, actor, subjectID); err != nil {
				if errors.Is(err, permissions.ErrDenied) && !deniedWithCause(err) {
					return ErrNotFound
				}
				return err
			}
		}
		var err error
		items, err = reviewItems(ctx, tx, `
			JOIN reviewer_assignments a ON a.subject_id = r.subject_id AND a.user_id = $1
			JOIN users ru ON ru.id = a.user_id AND ru.disabled_at IS NULL
			WHERE ($2 = 0 OR r.subject_id = $2)`, `r.submitted_at, r.id`, actor, subjectID, MaxQueue)
		return err
	})
	return items, err
}

// BlockedPage is the page size of BlockedReviews (a variable for tests).
var BlockedPage = MaxQueue

// blockedFilter keeps the revisions no active reviewer of their subject may
// decide on (the SQL form of reviewBlocked).
const blockedFilter = `NOT EXISTS (SELECT 1 FROM reviewer_assignments ra JOIN users ru ON ru.id = ra.user_id AND ru.disabled_at IS NULL
		WHERE ra.subject_id = r.subject_id AND course_reviewer_eligible(ra.user_id, r.id))`

// BlockedReviews pages, for administrators, through the course revisions
// in review that no active reviewer may decide on, with the reasons, by
// revision ID: after is the last revision ID of the previous page (0 for
// the first). It returns at most BlockedPage items and the next cursor, or
// nil.
func (s *Store) BlockedReviews(ctx context.Context, actor, after int64) ([]ReviewItem, *int64, error) {
	var out []ReviewItem
	err := s.tx(ctx, actor, []permissions.Permission{permissions.Administrator}, func(tx pgx.Tx) error {
		var err error
		out, err = reviewItems(ctx, tx, `WHERE r.id > $2 AND `+blockedFilter, `r.id`, actor, after, BlockedPage+1)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var next *int64
	if len(out) > BlockedPage {
		out = out[:BlockedPage]
		id := out[len(out)-1].RevisionID
		next = &id
	}
	return out, next, nil
}

func reviewItems(ctx context.Context, tx pgx.Tx, filter, order string, actor, arg int64, limit int) ([]ReviewItem, error) {
	rows, err := tx.Query(ctx, `
		SELECT r.id, r.course_id, r.revision_number, r.kind, r.title, r.language, r.subject_id, r.author_id, au.display_name,
		       r.submitted_at, course_reviewer_eligible($1, r.id)
		  FROM course_revisions r
		  JOIN users au ON au.id = r.author_id
		  `+filter+`
		   AND r.status = 'in_review' AND r.withdrawn_at IS NULL
		 ORDER BY `+order+`
		 LIMIT $3`, actor, arg, limit)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReviewItem, error) {
		var it ReviewItem
		err := row.Scan(&it.RevisionID, &it.CourseID, &it.Number, &it.Kind, &it.Title, &it.Language, &it.SubjectID,
			&it.AuthorID, &it.AuthorName, &it.SubmittedAt, &it.Eligible)
		return it, err
	})
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].RevisionID
	}
	blocked, err := reviewBlockedBatch(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Blocked = blocked[items[i].RevisionID]
	}
	if items == nil {
		items = []ReviewItem{}
	}
	return items, nil
}

// StepChange is one step of a revision under review compared with its
// comparison revision: "unchanged" (same lesson and pinned revision),
// "replaced" (same lesson, another revision; Previous* and Diff set) or
// "added" (a lesson the comparison revision does not have), or "moved"
// (same lesson and pinned revision, but another module, position or title).
// For a step of a lesson the comparison revision has, Previous* give its
// module, position and title there and StructureChanged tells whether they
// differ (also for a replaced step).
type StepChange struct {
	ModuleKey              string
	Position               int
	Title                  string
	LessonID               int64
	RevisionID             int64
	RevisionNumber         int
	Change                 string
	PreviousRevisionID     *int64
	PreviousRevisionNumber *int
	PreviousModuleKey      *string
	PreviousPosition       *int
	PreviousTitle          *string
	StructureChanged       bool
	Diff                   *ContentDiff
}

// RemovedStep is a step of the comparison revision whose lesson the
// revision no longer has.
type RemovedStep struct {
	ModuleKey  string
	Position   int
	Title      string
	LessonID   int64
	RevisionID int64
}

// ReviewPayload is what a reviewer reads (contract §6).
type ReviewPayload struct {
	Revision Revision
	// Eligible: the caller may decide on it now.
	Eligible bool
	// Problems: why not (empty when Eligible).
	Problems []ReviewerProblem
	Steps    []StepChange
	Removed  []RemovedStep
	// MetadataChanged: overview, cover, placement, modules or step
	// structure (lesson, module, position, title of each step) differ from
	// the comparison revision (always true without one).
	MetadataChanged bool
}

// ReviewRevision returns the review payload of a submitted course revision:
// to an active reviewer of its subject while it is in review, and to
// publishers and administrators whatever its status after submission.
// Anyone else, a draft and a missing revision get ErrNotFound.
func (s *Store) ReviewRevision(ctx context.Context, actor, revisionID int64) (ReviewPayload, error) {
	var p ReviewPayload
	err := s.tx(ctx, actor, nil, func(tx pgx.Tx) error {
		var status Status
		var subject int64
		err := tx.QueryRow(ctx, `SELECT status, subject_id FROM course_revisions WHERE id = $1`, revisionID).Scan(&status, &subject)
		if errors.Is(err, pgx.ErrNoRows) || status == StatusDraft {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		staff := permissions.RequireAnyTx(ctx, tx, actor, permissions.Publisher, permissions.Administrator)
		if staff != nil {
			if deniedWithCause(staff) {
				return staff
			}
			if err := permissions.RequireReviewerTx(ctx, tx, actor, subject); err != nil {
				if errors.Is(err, permissions.ErrDenied) && !deniedWithCause(err) {
					return ErrNotFound
				}
				return err
			}
			if status != StatusInReview {
				return ErrNotFound
			}
		}
		if p.Revision, err = loadRevision(ctx, tx, revisionID); err != nil {
			return err
		}
		if p.Problems, err = reviewerProblems(ctx, tx, actor, revisionID); err != nil {
			return err
		}
		p.Eligible = len(p.Problems) == 0 && status == StatusInReview && p.Revision.WithdrawnAt == nil
		if p.Problems == nil {
			p.Problems = []ReviewerProblem{}
		}
		return loadChanges(ctx, tx, &p)
	})
	return p, err
}

func loadChanges(ctx context.Context, tx pgx.Tx, p *ReviewPayload) error {
	r := p.Revision
	rows, err := tx.Query(ctx, `
		SELECT s.module_key::text, s.position, s.title, s.lesson_id, s.lesson_revision_id, lr.revision_number,
		       ps.lesson_revision_id, pr.revision_number, lr.content, pr.content,
		       ps.module_key::text, ps.position, ps.title
		  FROM course_steps s
		  JOIN course_modules m ON m.course_revision_id = s.course_revision_id AND m.module_key = s.module_key
		  JOIN lesson_revisions lr ON lr.id = s.lesson_revision_id
		  LEFT JOIN course_steps ps ON ps.course_revision_id = $2 AND ps.lesson_id = s.lesson_id
		  LEFT JOIN lesson_revisions pr ON pr.id = ps.lesson_revision_id
		 WHERE s.course_revision_id = $1
		 ORDER BY m.position, s.position`, r.ID, r.CompareRevisionID)
	if err != nil {
		return err
	}
	type row struct {
		StepChange
		inComparison  *int64
		content, prev []byte
	}
	list, err := pgx.CollectRows(rows, func(rw pgx.CollectableRow) (row, error) {
		var x row
		err := rw.Scan(&x.ModuleKey, &x.Position, &x.Title, &x.LessonID, &x.RevisionID, &x.RevisionNumber,
			&x.inComparison, &x.PreviousRevisionNumber, &x.content, &x.prev,
			&x.PreviousModuleKey, &x.PreviousPosition, &x.PreviousTitle)
		return x, err
	})
	if err != nil {
		return err
	}
	p.Steps = []StepChange{}
	for _, x := range list {
		c := x.StepChange
		c.StructureChanged = x.inComparison != nil &&
			(*c.PreviousModuleKey != c.ModuleKey || *c.PreviousPosition != c.Position || *c.PreviousTitle != c.Title)
		switch {
		case x.inComparison == nil:
			c.Change = "added"
			c.PreviousRevisionNumber = nil
		case *x.inComparison == c.RevisionID && c.StructureChanged:
			c.Change = "moved"
			c.PreviousRevisionNumber = nil
		case *x.inComparison == c.RevisionID:
			c.Change = "unchanged"
			c.PreviousRevisionNumber = nil
		default:
			c.Change = "replaced"
			c.PreviousRevisionID = x.inComparison
			d, err := DiffContent(x.prev, x.content)
			if err != nil {
				return err
			}
			c.Diff = &d
		}
		p.Steps = append(p.Steps, c)
	}
	rows, err = tx.Query(ctx, `
		SELECT s.module_key::text, s.position, s.title, s.lesson_id, s.lesson_revision_id
		  FROM course_steps s
		 WHERE s.course_revision_id = $2
		   AND NOT EXISTS (SELECT 1 FROM course_steps n WHERE n.course_revision_id = $1 AND n.lesson_id = s.lesson_id)
		 ORDER BY s.module_key, s.position`, r.ID, r.CompareRevisionID)
	if err != nil {
		return err
	}
	p.Removed, err = pgx.CollectRows(rows, func(rw pgx.CollectableRow) (RemovedStep, error) {
		var x RemovedStep
		err := rw.Scan(&x.ModuleKey, &x.Position, &x.Title, &x.LessonID, &x.RevisionID)
		return x, err
	})
	if err != nil {
		return err
	}
	if p.Removed == nil {
		p.Removed = []RemovedStep{}
	}
	return tx.QueryRow(ctx, `
		SELECT c.id IS NULL
		    OR (r.title, r.summary, r.objectives, r.prerequisites, r.cover_media_id, r.cover_alt)
		       IS DISTINCT FROM (c.title, c.summary, c.objectives, c.prerequisites, c.cover_media_id, c.cover_alt)
		    OR EXISTS ((SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = r.id
		                EXCEPT SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = c.id)
		               UNION ALL
		               (SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = c.id
		                EXCEPT SELECT assignment_id FROM course_revision_assignments WHERE course_revision_id = r.id))
		    OR EXISTS ((SELECT module_key, position, title, chapter_id FROM course_modules WHERE course_revision_id = r.id
		                EXCEPT SELECT module_key, position, title, chapter_id FROM course_modules WHERE course_revision_id = c.id)
		               UNION ALL
		               (SELECT module_key, position, title, chapter_id FROM course_modules WHERE course_revision_id = c.id
		                EXCEPT SELECT module_key, position, title, chapter_id FROM course_modules WHERE course_revision_id = r.id))
		    OR EXISTS ((SELECT module_key, position, title, lesson_id FROM course_steps WHERE course_revision_id = r.id
		                EXCEPT SELECT module_key, position, title, lesson_id FROM course_steps WHERE course_revision_id = c.id)
		               UNION ALL
		               (SELECT module_key, position, title, lesson_id FROM course_steps WHERE course_revision_id = c.id
		                EXCEPT SELECT module_key, position, title, lesson_id FROM course_steps WHERE course_revision_id = r.id))
		  FROM course_revisions r LEFT JOIN course_revisions c ON c.id = r.compare_revision_id
		 WHERE r.id = $1`, r.ID).Scan(&p.MetadataChanged)
}

// ContentDiff compares two lesson documents (schema v1): the top-level
// fields that differ, and the blocks as an ordered edit script.
type ContentDiff struct {
	Fields []FieldChange
	Blocks []BlockChange
}

// FieldChange is a changed top-level field (title, objective, ...).
type FieldChange struct {
	Name     string
	Old, New json.RawMessage
}

// BlockChange is one block of the edit script: "equal", "removed" or
// "added".
type BlockChange struct {
	Op    string
	Block json.RawMessage
}

// DiffContent diffs two lesson documents. Blocks are compared by their
// canonical JSON (keys sorted); the edit script is a longest common
// subsequence.
func DiffContent(oldDoc, newDoc []byte) (ContentDiff, error) {
	var o, n map[string]json.RawMessage
	if err := json.Unmarshal(oldDoc, &o); err != nil {
		return ContentDiff{}, err
	}
	if err := json.Unmarshal(newDoc, &n); err != nil {
		return ContentDiff{}, err
	}
	d := ContentDiff{Fields: []FieldChange{}, Blocks: []BlockChange{}}
	keys := map[string]bool{}
	for k := range o {
		keys[k] = true
	}
	for k := range n {
		keys[k] = true
	}
	var names []string
	for k := range keys {
		if k != "blocks" {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	for _, k := range names {
		a, b := canonical(o[k]), canonical(n[k])
		if !bytes.Equal(a, b) {
			d.Fields = append(d.Fields, FieldChange{Name: k, Old: nullable(a), New: nullable(b)})
		}
	}
	var ob, nb []json.RawMessage
	_ = json.Unmarshal(o["blocks"], &ob)
	_ = json.Unmarshal(n["blocks"], &nb)
	oc, nc := make([][]byte, len(ob)), make([][]byte, len(nb))
	for i := range ob {
		oc[i] = canonical(ob[i])
	}
	for i := range nb {
		nc[i] = canonical(nb[i])
	}
	// lcs[i][j] = length of the LCS of oc[i:] and nc[j:].
	lcs := make([][]int, len(oc)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(nc)+1)
	}
	for i := len(oc) - 1; i >= 0; i-- {
		for j := len(nc) - 1; j >= 0; j-- {
			if bytes.Equal(oc[i], nc[j]) {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(oc) || j < len(nc) {
		switch {
		case i < len(oc) && j < len(nc) && bytes.Equal(oc[i], nc[j]):
			d.Blocks = append(d.Blocks, BlockChange{Op: "equal", Block: nc[j]})
			i, j = i+1, j+1
		case j < len(nc) && (i == len(oc) || lcs[i][j+1] >= lcs[i+1][j]):
			d.Blocks = append(d.Blocks, BlockChange{Op: "added", Block: nc[j]})
			j++
		default:
			d.Blocks = append(d.Blocks, BlockChange{Op: "removed", Block: oc[i]})
			i++
		}
	}
	return d, nil
}

// canonical re-encodes JSON with sorted object keys (encoding/json sorts
// map keys); nil stays nil.
func canonical(raw json.RawMessage) []byte {
	if raw == nil {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

func nullable(b []byte) json.RawMessage {
	if b == nil {
		return json.RawMessage("null")
	}
	return b
}
