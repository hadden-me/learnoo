package review

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
)

// Read models for the Studio's publisher and administrator views (issue
// #66): the publisher queue, a publisher's read of a revision awaiting
// publication, a lesson's publication state and history, and the
// administrator's lesson list. They change nothing. Each rechecks its
// permission inside its own transaction (grant and account rows
// share-locked), like every other editorial endpoint, so a revocation that
// commits after the middleware check still denies.

// MaxPublishQueue bounds PublishQueue; MaxAdminLessons bounds AdminLessons;
// MaxLessonRevisions and MaxLessonHistory bound LessonPublication's lists.
const (
	MaxPublishQueue    = 200
	MaxAdminLessons    = 200
	MaxLessonRevisions = 200
	MaxLessonHistory   = 100
)

// awaitingPublication is the rule for "approved, never published, not
// withdrawn, still the lesson's candidate" on revision row r of lesson l.
// candidate_closed_at alone already covers publication and withdrawal; the
// explicit checks keep the rule readable and independent of the trigger.
const awaitingPublication = `r.status = 'approved' AND r.candidate_closed_at IS NULL AND l.archived_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM revision_withdrawals w WHERE w.revision_id = r.id)
	AND NOT EXISTS (SELECT 1 FROM publication_audit p WHERE p.revision_id = r.id AND p.action = 'publish')`

// PublishQueueItem is one approved revision awaiting publication.
type PublishQueueItem struct {
	RevisionID          int64
	LessonID            int64
	LessonPublicID      string
	Number              int
	Title               string
	Language            string
	SubjectID           int64
	SubjectCode         string
	AuthorID            int64
	AuthorName          string
	ApprovedBy          int64
	ApproverName        string
	ApprovedAt          time.Time
	BaseRevisionID      *int64
	PublishedRevisionID *int64
}

// Stale reports that the lesson's published revision changed since the
// revision was drafted: Publish would refuse it with ErrStaleBase.
func (it PublishQueueItem) Stale() bool { return !sameID(it.BaseRevisionID, it.PublishedRevisionID) }

// PublishQueue lists approved revisions that were never published, are not
// withdrawn and belong to lessons that are not archived, oldest approval
// first, at most MaxPublishQueue. It requires the publisher permission.
func (s *Store) PublishQueue(ctx context.Context, actor int64) ([]PublishQueueItem, error) {
	var items []PublishQueueItem
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Publisher); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT r.id, r.lesson_id, l.public_id::text, r.revision_number, r.title, r.language, r.subject_id, s.code,
			       r.author_id, au.display_name, d.reviewer_id, rv.display_name, d.decided_at,
			       r.base_revision_id, l.published_revision_id
			  FROM lesson_revisions r
			  JOIN lessons l ON l.id = r.lesson_id
			  JOIN subjects s ON s.id = r.subject_id
			  JOIN review_decisions d ON d.revision_id = r.id AND d.action = 'approve'
			  JOIN users au ON au.id = r.author_id
			  JOIN users rv ON rv.id = d.reviewer_id
			 WHERE `+awaitingPublication+`
			 ORDER BY d.decided_at, r.id
			 LIMIT $1`, MaxPublishQueue)
		if err != nil {
			return err
		}
		items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (PublishQueueItem, error) {
			var q PublishQueueItem
			err := row.Scan(&q.RevisionID, &q.LessonID, &q.LessonPublicID, &q.Number, &q.Title, &q.Language,
				&q.SubjectID, &q.SubjectCode, &q.AuthorID, &q.AuthorName, &q.ApprovedBy, &q.ApproverName,
				&q.ApprovedAt, &q.BaseRevisionID, &q.PublishedRevisionID)
			return q, err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []PublishQueueItem{}
	}
	return items, nil
}

// PublishRevision returns a revision awaiting publication (the PublishQueue
// rule) to a publisher, so they can read what they publish. Any other
// revision, including a missing one, is ErrNotFound; a caller without the
// publisher permission gets ErrDenied.
func (s *Store) PublishRevision(ctx context.Context, actor, revisionID int64) (lessons.Revision, error) {
	var rev lessons.Revision
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Publisher); err != nil {
			return err
		}
		var ok bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lesson_revisions r JOIN lessons l ON l.id = r.lesson_id
			WHERE r.id = $1 AND `+awaitingPublication+`)`, revisionID).Scan(&ok)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		rev, err = lessons.RevisionTx(ctx, tx, revisionID)
		return err
	})
	if err != nil {
		return lessons.Revision{}, err
	}
	return rev, nil
}

// LessonRevisionState is one submitted revision of a lesson as seen by
// publishers and administrators. Drafts are not listed.
type LessonRevisionState struct {
	ID             int64
	Number         int
	Status         lessons.Status
	Title          string
	AuthorID       int64
	AuthorName     string
	BaseRevisionID *int64
	SubmittedAt    *time.Time
	ApprovedBy     *int64
	ApproverName   *string
	ApprovedAt     *time.Time
	// EverPublished: a publish audit row names the revision (restoration
	// is limited to such revisions).
	EverPublished bool
	// Candidate: approved and still the lesson's active candidate.
	Candidate  bool
	Withdrawal *WithdrawalRecord
	// WithdrawnByName is the withdrawing administrator's display name.
	WithdrawnByName string
}

// PointerEvent is one audited change of the published pointer, with the
// actor's display name.
type PointerEvent struct {
	PointerChange
	ActorName string
}

// LessonPublication is a lesson's publication state for publishers and
// administrators.
type LessonPublication struct {
	LessonID            int64
	PublicID            string
	SubjectID           int64
	SubjectCode         string
	Language            string
	Archived            bool
	PublishedRevisionID *int64
	// Revisions are the submitted revisions, newest first.
	Revisions []LessonRevisionState
	// History is the pointer audit, newest first.
	History []PointerEvent
}

var publicIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var lessonIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// ParseLessonRef accepts a numeric lesson ID or a lesson's public ID (a
// lower-case UUID, as in reader URLs). It returns ok false for anything else.
func ParseLessonRef(ref string) (id int64, publicID string, ok bool) {
	if lessonIDPattern.MatchString(ref) {
		n, err := strconv.ParseInt(ref, 10, 64)
		return n, "", err == nil
	}
	if publicIDPattern.MatchString(ref) {
		return 0, ref, true
	}
	return 0, "", false
}

// LessonPublication returns the lesson named by ref (see ParseLessonRef)
// with its submitted revisions and pointer history. It requires publisher
// or administrator: both may restore, and both are global permissions, so
// they may learn which lessons exist (ErrNotFound for a missing lesson).
func (s *Store) LessonPublication(ctx context.Context, actor int64, ref string) (LessonPublication, error) {
	id, publicID, ok := ParseLessonRef(ref)
	var out LessonPublication
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := permissions.RequireAnyTx(ctx, tx, actor, permissions.Publisher, permissions.Administrator); err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		err := tx.QueryRow(ctx, `
			SELECT l.id, l.public_id::text, l.subject_id, s.code, l.language, l.archived_at IS NOT NULL, l.published_revision_id
			  FROM lessons l JOIN subjects s ON s.id = l.subject_id
			 WHERE ($1 <> 0 AND l.id = $1) OR ($1 = 0 AND l.public_id::text = $2)`, id, publicID).Scan(
			&out.LessonID, &out.PublicID, &out.SubjectID, &out.SubjectCode, &out.Language, &out.Archived, &out.PublishedRevisionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT r.id, r.revision_number, r.status, r.title, r.author_id, au.display_name, r.base_revision_id,
			       r.submitted_at, d.reviewer_id, rv.display_name, d.decided_at,
			       EXISTS (SELECT 1 FROM publication_audit p WHERE p.revision_id = r.id AND p.action = 'publish'),
			       r.status = 'approved' AND r.candidate_closed_at IS NULL,
			       w.actor_id, w.reason, w.occurred_at, wu.display_name
			  FROM lesson_revisions r
			  JOIN users au ON au.id = r.author_id
			  LEFT JOIN review_decisions d ON d.revision_id = r.id AND d.action = 'approve'
			  LEFT JOIN users rv ON rv.id = d.reviewer_id
			  LEFT JOIN revision_withdrawals w ON w.revision_id = r.id
			  LEFT JOIN users wu ON wu.id = w.actor_id
			 WHERE r.lesson_id = $1 AND r.status <> 'draft'
			 ORDER BY r.revision_number DESC
			 LIMIT $2`, out.LessonID, MaxLessonRevisions)
		if err != nil {
			return err
		}
		out.Revisions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (LessonRevisionState, error) {
			var r LessonRevisionState
			var status string
			var wActor *int64
			var wReason, wName *string
			var wAt *time.Time
			err := row.Scan(&r.ID, &r.Number, &status, &r.Title, &r.AuthorID, &r.AuthorName, &r.BaseRevisionID,
				&r.SubmittedAt, &r.ApprovedBy, &r.ApproverName, &r.ApprovedAt, &r.EverPublished, &r.Candidate,
				&wActor, &wReason, &wAt, &wName)
			r.Status = lessons.Status(status)
			if err == nil && wActor != nil {
				r.Withdrawal = &WithdrawalRecord{LessonID: out.LessonID, RevisionID: r.ID, ActorID: *wActor, Reason: *wReason, WithdrawnAt: *wAt}
				r.WithdrawnByName = *wName
			}
			return r, err
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT p.lesson_id, p.action, p.revision_id, p.previous_revision_id, p.actor_id, coalesce(p.reason, ''),
			       p.occurred_at, u.display_name
			  FROM publication_audit p JOIN users u ON u.id = p.actor_id
			 WHERE p.lesson_id = $1
			 ORDER BY p.id DESC
			 LIMIT $2`, out.LessonID, MaxLessonHistory)
		if err != nil {
			return err
		}
		out.History, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (PointerEvent, error) {
			var e PointerEvent
			err := row.Scan(&e.LessonID, &e.Action, &e.RevisionID, &e.PreviousRevisionID, &e.ActorID, &e.Reason,
				&e.OccurredAt, &e.ActorName)
			return e, err
		})
		return err
	})
	if err != nil {
		return LessonPublication{}, err
	}
	if out.Revisions == nil {
		out.Revisions = []LessonRevisionState{}
	}
	if out.History == nil {
		out.History = []PointerEvent{}
	}
	return out, nil
}

// AdminLesson is one row of the administrator's lesson list.
type AdminLesson struct {
	LessonID            int64
	PublicID            string
	SubjectCode         string
	Language            string
	Archived            bool
	PublishedRevisionID *int64
	// Title is the published revision's title, or the latest approved one's.
	Title string
	// PendingRevisionID is the approved candidate awaiting publication, if any.
	PendingRevisionID *int64
	// LastChange is the latest approval or pointer change.
	LastChange time.Time
}

// AdminLessons lists lessons with at least one approved revision (the ones
// withdrawal, unpublication and restoration can act on), most recently
// changed first, at most MaxAdminLessons. It requires administrator.
func (s *Store) AdminLessons(ctx context.Context, actor int64) ([]AdminLesson, error) {
	var out []AdminLesson
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Administrator); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT l.id, l.public_id::text, s.code, l.language, l.archived_at IS NOT NULL, l.published_revision_id,
			       coalesce(pr.title, la.title),
			       (SELECT c.id FROM lesson_revisions c WHERE c.lesson_id = l.id AND c.status = 'approved'
			           AND c.candidate_closed_at IS NULL LIMIT 1),
			       greatest(la.decided_at, (SELECT max(p.occurred_at) FROM publication_audit p WHERE p.lesson_id = l.id))
			  FROM lessons l
			  JOIN subjects s ON s.id = l.subject_id
			  LEFT JOIN lesson_revisions pr ON pr.id = l.published_revision_id
			  JOIN LATERAL (SELECT r.title, d.decided_at FROM lesson_revisions r
			                  JOIN review_decisions d ON d.revision_id = r.id AND d.action = 'approve'
			                 WHERE r.lesson_id = l.id ORDER BY r.revision_number DESC LIMIT 1) la ON true
			 ORDER BY 9 DESC, l.id DESC
			 LIMIT $1`, MaxAdminLessons)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminLesson, error) {
			var a AdminLesson
			err := row.Scan(&a.LessonID, &a.PublicID, &a.SubjectCode, &a.Language, &a.Archived, &a.PublishedRevisionID,
				&a.Title, &a.PendingRevisionID, &a.LastChange)
			return a, err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []AdminLesson{}
	}
	return out, nil
}
