package courses

// Publication, restoration, suspension, archive, withdrawal, draft discard
// and transfer of courses (LMS-B04, docs/LMS_CONTRACT.md §7). The database
// keeps the pointer and its audit, the archive state, the owner and
// withdrawals consistent (migrations/0015); this file adds the checks that
// need the caller and maps refusals to typed errors.
//
// Every write runs in one READ COMMITTED transaction that first rechecks
// the caller's permission (grant and account rows share-locked), then takes
// its locks in the contract §2 order: curriculum editions, lessons and the
// pinned lesson revisions (FOR SHARE, ascending), the course FOR UPDATE,
// then the course revision.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
)

// MaxReason bounds a reason, in characters (database checks too).
const MaxReason = 2000

var (
	// ErrStalePointer means the course's published revision is not the one
	// the caller expected. Nothing was changed.
	ErrStalePointer = errors.New("published revision is not the expected one")
	// ErrStaleBase means the course's published revision changed since the
	// revision's draft was created. Nothing was changed.
	ErrStaleBase = errors.New("published revision changed since this revision was drafted")
	// ErrNotApproved means the revision to publish is not approved.
	ErrNotApproved = errors.New("course revision is not approved")
	// ErrAlreadyPublished means the revision was already published; use
	// restoration to point back to it.
	ErrAlreadyPublished = errors.New("course revision was already published")
	// ErrNeverPublished means the revision to restore was never published.
	ErrNeverPublished = errors.New("course revision was never published")
	// ErrAlreadyCurrent means the revision to restore is already published.
	ErrAlreadyCurrent = errors.New("course revision is already the published revision")
	// ErrPublished means the revision to withdraw is the published one
	// (suspend the course instead).
	ErrPublished = errors.New("course revision is published")
	// ErrNotWithdrawable means the revision is neither in review nor
	// approved.
	ErrNotWithdrawable = errors.New("only revisions in review or approved can be withdrawn")
	// ErrNotArchived means the course to unarchive is not archived.
	ErrNotArchived = errors.New("course is not archived")
	// ErrNeverSubmitted means an archive request for a course with nothing
	// submitted: delete it instead.
	ErrNeverSubmitted = errors.New("course was never submitted")
	// ErrReasonRequired means a required reason is missing.
	ErrReasonRequired = errors.New("a reason is required")
	// ErrInvalidReason means the reason is too long or not valid text.
	ErrInvalidReason = errors.New("invalid reason")
	// ErrUnknownUser means a transfer target is not an active author.
	ErrUnknownUser = errors.New("the target is not an active author")
)

func normalizeReason(r string) (string, error) {
	r = strings.TrimSpace(r)
	if !utf8.ValidString(r) || strings.ContainsRune(r, 0) || utf8.RuneCountInString(r) > MaxReason {
		return "", fmt.Errorf("%w: at most %d characters of valid text", ErrInvalidReason, MaxReason)
	}
	if r == "" {
		return "", ErrReasonRequired
	}
	return r, nil
}

// PointerChange is the result of a change of a course's published pointer.
type PointerChange struct {
	CourseID           int64
	Action             string
	RevisionID         *int64
	PreviousRevisionID *int64
	ActorID            int64
	Reason             *string
	OccurredAt         time.Time
}

type courseRow struct {
	owner     int64
	subject   int64
	published *int64
	archived  bool
}

func lockCourseRow(ctx context.Context, tx pgx.Tx, courseID int64, mode string) (courseRow, error) {
	var c courseRow
	err := tx.QueryRow(ctx, `SELECT owner_id, subject_id, published_revision_id, archived_at IS NOT NULL FROM courses WHERE id = $1 `+mode,
		courseID).Scan(&c.owner, &c.subject, &c.published, &c.archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func pointerAudit(ctx context.Context, tx pgx.Tx, courseID int64, revision, previous *int64, actor int64, action string, reason *string) (PointerChange, error) {
	pc := PointerChange{CourseID: courseID, Action: action, RevisionID: revision, PreviousRevisionID: previous, ActorID: actor, Reason: reason}
	err := tx.QueryRow(ctx, `
		INSERT INTO course_publication_audit (course_id, revision_id, previous_revision_id, actor_id, action, reason)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING occurred_at`, courseID, revision, previous, actor, action, reason).Scan(&pc.OccurredAt)
	return pc, err
}

// lockForPointer takes, for publishing or restoring revisionID, the
// editions, lessons and pinned revisions of the revision (FOR SHARE), then
// the course FOR UPDATE, and compares the pointer with expected. The
// revision is submitted (checked by the caller), so its children do not
// change.
func lockForPointer(ctx context.Context, tx pgx.Tx, courseID, revisionID int64, expected *int64) (courseRow, error) {
	assignments, lessonIDs, revisionIDs, err := revisionDependencies(ctx, tx, revisionID)
	if err != nil {
		return courseRow{}, err
	}
	if err := lockEditionsAndLessons(ctx, tx, assignments, lessonIDs, revisionIDs); err != nil {
		return courseRow{}, err
	}
	c, err := lockCourseRow(ctx, tx, courseID, "FOR UPDATE")
	if err != nil {
		return c, err
	}
	if !sameID(c.published, expected) {
		return c, ErrStalePointer
	}
	return c, nil
}

// Publish points the revision's course at the approved revision, as a
// publisher, with its course_publication_audit row (contract §5
// "publishable"): approved by a recorded decision, never published, not
// withdrawn, base IS NOT DISTINCT FROM the pointer and comparison revision
// still the course's (course_comparison_revision) (ErrStaleBase), every
// step eligible (*IneligibleError), placement valid in publish mode
// (ErrPlacementInvalid), course not archived. expected is the pointer the
// caller saw (ErrStalePointer otherwise). On any failure nothing changes.
func (s *Store) Publish(ctx context.Context, actor, revisionID int64, expected *int64) (PointerChange, error) {
	var pc PointerChange
	err := s.tx(ctx, actor, []permissions.Permission{permissions.Publisher}, func(tx pgx.Tx) error {
		var courseID int64
		var status Status
		err := tx.QueryRow(ctx, `SELECT course_id, status FROM course_revisions WHERE id = $1`, revisionID).Scan(&courseID, &status)
		if errors.Is(err, pgx.ErrNoRows) || status == StatusDraft {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		c, err := lockForPointer(ctx, tx, courseID, revisionID, expected)
		if err != nil {
			return err
		}
		var base *int64
		var closed, withdrawn, approval, compared bool
		if err := tx.QueryRow(ctx, `
			SELECT r.status, r.base_revision_id, r.candidate_closed_at IS NOT NULL, r.withdrawn_at IS NOT NULL,
			       EXISTS (SELECT 1 FROM course_review_decisions d WHERE d.revision_id = r.id AND d.action = 'approve'),
			       r.compare_revision_id IS NOT DISTINCT FROM course_comparison_revision(r.course_id)
			  FROM course_revisions r WHERE r.id = $1 FOR SHARE`, revisionID).Scan(&status, &base, &closed, &withdrawn, &approval, &compared); err != nil {
			return err
		}
		switch {
		case withdrawn:
			return ErrWithdrawn
		case status != StatusApproved || !approval:
			return ErrNotApproved
		case closed || (c.published != nil && *c.published == revisionID):
			return ErrAlreadyPublished
		case c.archived:
			return ErrArchived
		case !sameID(base, c.published), !compared:
			// A restore or a withdrawal since the draft was created changed
			// the comparison revision the review was made against.
			return ErrStaleBase
		}
		bad, err := ineligibleSteps(ctx, tx, revisionID)
		if err != nil {
			return err
		}
		if len(bad) > 0 {
			return &IneligibleError{Steps: bad}
		}
		if err := placementProblems(ctx, tx, revisionID, "publish"); err != nil {
			return err
		}
		if pc, err = pointerAudit(ctx, tx, courseID, &revisionID, c.published, actor, "publish", nil); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE courses SET published_revision_id = $2 WHERE id = $1`, courseID, revisionID)
		return err
	})
	return pc, err
}

// Restore points the course back at a previously published revision, as a
// publisher or administrator (lifting a suspension is a restore): approved,
// not withdrawn, with an earlier publish row (ErrNeverPublished), every
// step eligible now (*IneligibleError), course not archived. Placement is
// not rechecked (contract §3). expected is the pointer the caller saw.
func (s *Store) Restore(ctx context.Context, actor, courseID, revisionID int64, expected *int64) (PointerChange, error) {
	var pc PointerChange
	err := s.tx(ctx, actor, []permissions.Permission{permissions.Publisher, permissions.Administrator}, func(tx pgx.Tx) error {
		var owner int64
		var status Status
		err := tx.QueryRow(ctx, `SELECT course_id, status FROM course_revisions WHERE id = $1`, revisionID).Scan(&owner, &status)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != courseID) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == StatusDraft {
			return ErrNeverPublished
		}
		c, err := lockForPointer(ctx, tx, courseID, revisionID, expected)
		if err != nil {
			return err
		}
		var withdrawn, published bool
		if err := tx.QueryRow(ctx, `
			SELECT r.status, r.withdrawn_at IS NOT NULL,
			       EXISTS (SELECT 1 FROM course_publication_audit a WHERE a.revision_id = r.id AND a.action = 'publish')
			  FROM course_revisions r WHERE r.id = $1 FOR SHARE`, revisionID).Scan(&status, &withdrawn, &published); err != nil {
			return err
		}
		switch {
		case withdrawn:
			return ErrWithdrawn
		case status != StatusApproved || !published:
			return ErrNeverPublished
		case c.published != nil && *c.published == revisionID:
			return ErrAlreadyCurrent
		case c.archived:
			return ErrArchived
		}
		bad, err := ineligibleSteps(ctx, tx, revisionID)
		if err != nil {
			return err
		}
		if len(bad) > 0 {
			return &IneligibleError{Steps: bad}
		}
		if pc, err = pointerAudit(ctx, tx, courseID, &revisionID, c.published, actor, "restore", nil); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE courses SET published_revision_id = $2 WHERE id = $1`, courseID, revisionID)
		return err
	})
	return pc, err
}

// Suspend clears the course's pointer, as an administrator, with a reason:
// the course leaves discovery and reading at once. expected must be the
// current, non-NULL pointer (ErrStalePointer, ErrNotPublished).
func (s *Store) Suspend(ctx context.Context, actor, courseID int64, expected *int64, reason string) (PointerChange, error) {
	reason, err := normalizeReason(reason)
	if err != nil {
		return PointerChange{}, err
	}
	var pc PointerChange
	err = s.tx(ctx, actor, []permissions.Permission{permissions.Administrator}, func(tx pgx.Tx) error {
		c, err := lockCourseRow(ctx, tx, courseID, "FOR UPDATE")
		if err != nil {
			return err
		}
		switch {
		case !sameID(c.published, expected):
			return ErrStalePointer
		case c.published == nil:
			return ErrNotPublished
		}
		if pc, err = pointerAudit(ctx, tx, courseID, nil, c.published, actor, "suspend", &reason); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE courses SET published_revision_id = NULL WHERE id = $1`, courseID)
		return err
	})
	return pc, err
}

// LifecycleEvent is a course_lifecycle_audit row.
type LifecycleEvent struct {
	ID          int64
	CourseID    int64
	ActorID     int64
	Action      string
	Reason      string
	RevisionID  *int64
	FromOwnerID *int64
	ToOwnerID   *int64
	OccurredAt  time.Time
}

func lifecycle(ctx context.Context, tx pgx.Tx, e LifecycleEvent) (LifecycleEvent, error) {
	err := tx.QueryRow(ctx, `
		INSERT INTO course_lifecycle_audit (course_id, actor_id, action, reason, revision_id, from_owner_id, to_owner_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, occurred_at`,
		e.CourseID, e.ActorID, e.Action, e.Reason, e.RevisionID, e.FromOwnerID, e.ToOwnerID).Scan(&e.ID, &e.OccurredAt)
	return e, err
}

// Archive archives the course, as an administrator, with a reason: it sets
// archived_at and clears the pointer with an "archive" row of
// course_publication_audit, or, when the pointer is already NULL, records
// only a course_lifecycle_audit row. expected is the pointer the caller saw.
// It returns the pointer change (Action "archive") or a zero PointerChange
// with Action "archive" and no revision when nothing was published.
func (s *Store) Archive(ctx context.Context, actor, courseID int64, expected *int64, reason string) (PointerChange, error) {
	reason, err := normalizeReason(reason)
	if err != nil {
		return PointerChange{}, err
	}
	var pc PointerChange
	err = s.tx(ctx, actor, []permissions.Permission{permissions.Administrator}, func(tx pgx.Tx) error {
		c, err := lockCourseRow(ctx, tx, courseID, "FOR UPDATE")
		if err != nil {
			return err
		}
		switch {
		case c.archived:
			return ErrArchived
		case !sameID(c.published, expected):
			return ErrStalePointer
		}
		if c.published != nil {
			if pc, err = pointerAudit(ctx, tx, courseID, nil, c.published, actor, "archive", &reason); err != nil {
				return err
			}
		} else {
			e, err := lifecycle(ctx, tx, LifecycleEvent{CourseID: courseID, ActorID: actor, Action: "archive", Reason: reason})
			if err != nil {
				return err
			}
			pc = PointerChange{CourseID: courseID, Action: "archive", ActorID: actor, Reason: &reason, OccurredAt: e.OccurredAt}
		}
		_, err = tx.Exec(ctx, `UPDATE courses SET archived_at = now(), published_revision_id = NULL WHERE id = $1`, courseID)
		return err
	})
	return pc, err
}

// Unarchive clears archived_at, as an administrator, with a reason. Access
// returns only through a restore or a new publication.
func (s *Store) Unarchive(ctx context.Context, actor, courseID int64, reason string) (LifecycleEvent, error) {
	return s.adminLifecycle(ctx, actor, courseID, reason, func(tx pgx.Tx, c courseRow, e *LifecycleEvent) error {
		if !c.archived {
			return ErrNotArchived
		}
		e.Action = "unarchive"
		ev, err := lifecycle(ctx, tx, *e)
		if err != nil {
			return err
		}
		*e = ev
		_, err = tx.Exec(ctx, `UPDATE courses SET archived_at = NULL WHERE id = $1`, courseID)
		return err
	})
}

// Transfer gives the course to target, an active author, as an
// administrator with a reason. Refused with ErrActiveCandidateExists while
// a candidate is open (discard or withdraw it first). History keeps its
// authors.
func (s *Store) Transfer(ctx context.Context, actor, courseID, target int64, reason string) (LifecycleEvent, error) {
	return s.adminLifecycle(ctx, actor, courseID, reason, func(tx pgx.Tx, c courseRow, e *LifecycleEvent) error {
		if c.owner == target {
			return invalid("the course already belongs to that user")
		}
		var active bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM course_revisions
			                WHERE course_id = $1 AND withdrawn_at IS NULL
			                  AND (status IN ('draft', 'in_review') OR (status = 'approved' AND candidate_closed_at IS NULL)))`,
			courseID).Scan(&active); err != nil {
			return err
		}
		if active {
			return ErrActiveCandidateExists
		}
		e.Action, e.FromOwnerID, e.ToOwnerID = "transfer", &c.owner, &target
		ev, err := lifecycle(ctx, tx, *e)
		if err != nil {
			return err
		}
		*e = ev
		_, err = tx.Exec(ctx, `UPDATE courses SET owner_id = $2 WHERE id = $1`, courseID, target)
		return err
	}, target)
}

// adminLifecycle runs fn as an administrator under the course lock. extra
// users (a transfer target) must be active authors; their grant rows are
// share-locked with the caller's, before the course (contract §2).
func (s *Store) adminLifecycle(ctx context.Context, actor, courseID int64, reason string,
	fn func(tx pgx.Tx, c courseRow, e *LifecycleEvent) error, authors ...int64) (LifecycleEvent, error) {
	reason, err := normalizeReason(reason)
	if err != nil {
		return LifecycleEvent{}, err
	}
	e := LifecycleEvent{CourseID: courseID, ActorID: actor, Reason: reason}
	err = s.tx(ctx, actor, []permissions.Permission{permissions.Administrator}, func(tx pgx.Tx) error {
		for _, u := range authors {
			if err := permissions.RequireTx(ctx, tx, u, permissions.Author); err != nil {
				if deniedWithCause(err) {
					return err
				}
				return ErrUnknownUser
			}
		}
		c, err := lockCourseRow(ctx, tx, courseID, "FOR UPDATE")
		if err != nil {
			return err
		}
		return fn(tx, c, &e)
	})
	return e, err
}

// DiscardDraftAsAdmin removes an open draft of any course, as an
// administrator with a reason, for recovery when the owner cannot.
func (s *Store) DiscardDraftAsAdmin(ctx context.Context, actor, revisionID int64, reason string) (LifecycleEvent, error) {
	var courseID int64
	err := s.db.QueryRow(ctx, `SELECT course_id FROM course_revisions WHERE id = $1`, revisionID).Scan(&courseID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Still check the permission first, so a non-administrator learns
		// nothing about which IDs exist.
		_, perr := s.adminLifecycle(ctx, actor, 0, reason, func(pgx.Tx, courseRow, *LifecycleEvent) error { return ErrNotFound })
		if errors.Is(perr, ErrNotFound) {
			return LifecycleEvent{}, ErrNotFound
		}
		return LifecycleEvent{}, perr
	}
	if err != nil {
		return LifecycleEvent{}, err
	}
	return s.adminLifecycle(ctx, actor, courseID, reason, func(tx pgx.Tx, c courseRow, e *LifecycleEvent) error {
		var status Status
		err := tx.QueryRow(ctx, `SELECT status FROM course_revisions WHERE id = $1 AND course_id = $2 FOR UPDATE`, revisionID, courseID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != StatusDraft {
			return ErrNotDraft
		}
		e.Action, e.RevisionID = "discard_draft", &revisionID
		ev, err := lifecycle(ctx, tx, *e)
		if err != nil {
			return err
		}
		*e = ev
		_, err = tx.Exec(ctx, `DELETE FROM course_revisions WHERE id = $1 AND status = 'draft'`, revisionID)
		return err
	})
}

// RequestArchive records the owner's request to archive their course, with
// a reason, for administrators (ArchiveRequests). A course that was never
// submitted is deleted instead (ErrNeverSubmitted).
func (s *Store) RequestArchive(ctx context.Context, actor, courseID int64, reason string) (LifecycleEvent, error) {
	reason, err := normalizeReason(reason)
	if err != nil {
		return LifecycleEvent{}, err
	}
	e := LifecycleEvent{CourseID: courseID, ActorID: actor, Action: "archive_request", Reason: reason}
	err = s.write(ctx, actor, func(tx pgx.Tx) error {
		c, err := lockCourseRow(ctx, tx, courseID, "FOR SHARE")
		if err != nil {
			return err
		}
		if c.owner != actor {
			return ErrNotFound
		}
		if c.archived {
			return ErrArchived
		}
		var submitted bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM course_revisions WHERE course_id = $1 AND status <> 'draft')`,
			courseID).Scan(&submitted); err != nil {
			return err
		}
		if !submitted {
			return ErrNeverSubmitted
		}
		e, err = lifecycle(ctx, tx, e)
		return err
	})
	return e, err
}

// ArchiveRequest is an archive request with the course's state.
type ArchiveRequest struct {
	LifecycleEvent
	Archived            bool
	PublishedRevisionID *int64
	Title               string
}

// ArchiveRequests lists the archive requests, newest first (at most
// MaxQueue), for administrators.
func (s *Store) ArchiveRequests(ctx context.Context, actor int64) ([]ArchiveRequest, error) {
	var out []ArchiveRequest
	err := s.tx(ctx, actor, []permissions.Permission{permissions.Administrator}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT a.id, a.course_id, a.actor_id, a.reason, a.occurred_at, c.archived_at IS NOT NULL, c.published_revision_id,
			       coalesce((SELECT r.title FROM course_revisions r WHERE r.course_id = c.id AND r.status <> 'draft'
			                  ORDER BY r.revision_number DESC LIMIT 1), '')
			  FROM course_lifecycle_audit a JOIN courses c ON c.id = a.course_id
			 WHERE a.action = 'archive_request'
			 ORDER BY a.id DESC LIMIT $1`, MaxQueue)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ArchiveRequest, error) {
			var x ArchiveRequest
			x.Action = "archive_request"
			err := r.Scan(&x.ID, &x.CourseID, &x.ActorID, &x.Reason, &x.OccurredAt, &x.Archived, &x.PublishedRevisionID, &x.Title)
			return x, err
		})
		return err
	})
	if out == nil {
		out = []ArchiveRequest{}
	}
	return out, err
}

// Withdrawal is a course_revision_withdrawals row.
type Withdrawal struct {
	CourseID   int64
	RevisionID int64
	ActorID    int64
	Reason     string
	OccurredAt time.Time
}

// Withdraw withdraws a course revision in review or approved, as an
// administrator with a reason: it frees the course's candidate slot and
// blocks later decisions, publication and restoration of it. The published
// revision cannot be withdrawn (ErrPublished: suspend the course instead);
// a previously published one can, which prevents its restoration.
func (s *Store) Withdraw(ctx context.Context, actor, revisionID int64, reason string) (Withdrawal, error) {
	reason, err := normalizeReason(reason)
	if err != nil {
		return Withdrawal{}, err
	}
	w := Withdrawal{RevisionID: revisionID, ActorID: actor, Reason: reason}
	err = s.tx(ctx, actor, []permissions.Permission{permissions.Administrator}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT course_id FROM course_revisions WHERE id = $1`, revisionID).Scan(&w.CourseID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		c, err := lockCourseRow(ctx, tx, w.CourseID, "FOR UPDATE")
		if err != nil {
			return err
		}
		var status Status
		var withdrawn bool
		if err := tx.QueryRow(ctx, `SELECT status, withdrawn_at IS NOT NULL FROM course_revisions WHERE id = $1 FOR UPDATE`,
			revisionID).Scan(&status, &withdrawn); err != nil {
			return err
		}
		switch {
		case withdrawn:
			return ErrWithdrawn
		case status != StatusInReview && status != StatusApproved:
			return ErrNotWithdrawable
		case c.published != nil && *c.published == revisionID:
			return ErrPublished
		}
		return tx.QueryRow(ctx, `INSERT INTO course_revision_withdrawals (course_id, revision_id, actor_id, reason)
			VALUES ($1, $2, $3, $4) RETURNING occurred_at`, w.CourseID, revisionID, actor, reason).Scan(&w.OccurredAt)
	})
	return w, err
}

// PublishItem is an approved course revision awaiting publication.
type PublishItem struct {
	RevisionID          int64
	CourseID            int64
	CoursePublicID      string
	Number              int
	Kind                string
	Title               string
	Language            string
	SubjectID           int64
	AuthorID            int64
	AuthorName          string
	ApprovedBy          int64
	ApprovedAt          time.Time
	BaseRevisionID      *int64
	PublishedRevisionID *int64
	Archived            bool
	// IneligibleSteps counts steps whose pinned revision is not eligible
	// now (publication would be refused).
	IneligibleSteps int
	// StaleComparison: the course's comparison revision changed since the
	// draft was created (a restore or withdrawal).
	StaleComparison bool
}

// StaleBase reports whether publication would be refused for its base.
func (p PublishItem) StaleBase() bool {
	return !sameID(p.BaseRevisionID, p.PublishedRevisionID) || p.StaleComparison
}

// PublishQueue lists approved, never published, non-withdrawn course
// revisions, oldest approval first (at most MaxQueue), for publishers.
func (s *Store) PublishQueue(ctx context.Context, actor int64) ([]PublishItem, error) {
	var out []PublishItem
	err := s.tx(ctx, actor, []permissions.Permission{permissions.Publisher}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT r.id, r.course_id, c.public_id::text, r.revision_number, r.kind, r.title, r.language, r.subject_id,
			       r.author_id, au.display_name, d.reviewer_id, d.decided_at, r.base_revision_id, c.published_revision_id,
			       c.archived_at IS NOT NULL,
			       (SELECT count(*) FROM course_steps s WHERE s.course_revision_id = r.id
			           AND NOT course_step_revision_eligible(s.lesson_id, s.lesson_revision_id)),
			       r.compare_revision_id IS DISTINCT FROM course_comparison_revision(r.course_id)
			  FROM course_revisions r
			  JOIN courses c ON c.id = r.course_id
			  JOIN users au ON au.id = r.author_id
			  JOIN course_review_decisions d ON d.revision_id = r.id AND d.action = 'approve'
			 WHERE r.status = 'approved' AND r.candidate_closed_at IS NULL AND r.withdrawn_at IS NULL
			 ORDER BY d.decided_at, r.id
			 LIMIT $1`, MaxQueue)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (PublishItem, error) {
			var p PublishItem
			err := row.Scan(&p.RevisionID, &p.CourseID, &p.CoursePublicID, &p.Number, &p.Kind, &p.Title, &p.Language, &p.SubjectID,
				&p.AuthorID, &p.AuthorName, &p.ApprovedBy, &p.ApprovedAt, &p.BaseRevisionID, &p.PublishedRevisionID,
				&p.Archived, &p.IneligibleSteps, &p.StaleComparison)
			return p, err
		})
		return err
	})
	if out == nil {
		out = []PublishItem{}
	}
	return out, err
}
