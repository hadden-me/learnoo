package courses

// Administrator reads for course recovery (LMS-B06): every course with its
// state, and one course with its revisions and audit trail, so that the
// Studio's recovery actions (suspend, restore, archive, unarchive,
// withdrawal, draft discard, transfer) are reached without knowing IDs.
// Read-only; every action keeps its own endpoint and checks.

import (
	"context"
	"errors"
	"time"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
)

// AdminPage bounds one page of AdminCourses.
const AdminPage = 100

// AdminCourse is a course in the administrators' list.
type AdminCourse struct {
	Course
	OwnerName string
	// Title of the latest revision.
	Title  string
	Latest *RevisionSummary
}

// AdminCourses pages through every course, newest first (by course ID):
// before is the last course ID of the previous page (0 for the first).
func (s *Store) AdminCourses(ctx context.Context, actor, before int64) ([]AdminCourse, *int64, error) {
	var out []AdminCourse
	err := s.tx(ctx, actor, []permissions.Permission{permissions.Administrator}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.id, c.public_id::text, c.owner_id, c.subject_id, c.language, c.published_revision_id, c.archived_at, c.created_at,
			       `+suspendedSQL+`, u.display_name,
			       r.id, r.revision_number, r.status, r.title, r.version, r.updated_at, r.withdrawn_at IS NOT NULL
			  FROM courses c
			  JOIN users u ON u.id = c.owner_id
			  LEFT JOIN LATERAL (SELECT id, revision_number, status, title, version, updated_at, withdrawn_at FROM course_revisions
			                      WHERE course_id = c.id ORDER BY revision_number DESC LIMIT 1) r ON true
			 WHERE $1 = 0 OR c.id < $1
			 ORDER BY c.id DESC
			 LIMIT $2`, before, AdminPage+1)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminCourse, error) {
			var a AdminCourse
			var id *int64
			var number, version *int
			var status, title *string
			var updated *time.Time
			var withdrawn *bool
			err := row.Scan(&a.ID, &a.PublicID, &a.OwnerID, &a.SubjectID, &a.Language, &a.PublishedRevisionID, &a.ArchivedAt,
				&a.CreatedAt, &a.Suspended, &a.OwnerName, &id, &number, &status, &title, &version, &updated, &withdrawn)
			if err == nil && id != nil {
				a.Title = *title
				a.Latest = &RevisionSummary{ID: *id, Number: *number, Status: Status(*status), Title: *title, Version: *version,
					UpdatedAt: *updated, Withdrawn: *withdrawn}
			}
			return a, err
		})
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var next *int64
	if len(out) > AdminPage {
		out = out[:AdminPage]
		id := out[len(out)-1].ID
		next = &id
	}
	if out == nil {
		out = []AdminCourse{}
	}
	return out, next, nil
}

// AdminRevision is one revision of a course in the administrators' view.
type AdminRevision struct {
	ID            int64
	Number        int
	Status        Status
	Kind          string
	Title         string
	AuthorID      int64
	AuthorName    string
	UpdatedAt     time.Time
	SubmittedAt   *time.Time
	WithdrawnAt   *time.Time
	EverPublished bool
	Decision      *Decision
}

// PointerEvent is a course_publication_audit row with the actor's name.
type PointerEvent struct {
	PointerChange
	ActorName string
}

// NamedLifecycleEvent is a course_lifecycle_audit row with the actor's name.
type NamedLifecycleEvent struct {
	LifecycleEvent
	ActorName string
}

// AdminCourseDetail is one course for administrators: its state, every
// revision (newest first) and its pointer and lifecycle audit (newest
// first, at most MaxQueue each).
type AdminCourseDetail struct {
	AdminCourse
	Revisions []AdminRevision
	History   []PointerEvent
	Lifecycle []NamedLifecycleEvent
	// Authors: active accounts holding author other than the owner (at
	// most MaxQueue, by name), the possible targets of a transfer.
	Authors []Account
}

// Account is a user's ID and display name.
type Account struct {
	ID   int64
	Name string
}

// AdminCourseByID returns one course for administrators (ErrNotFound when
// it does not exist).
func (s *Store) AdminCourseByID(ctx context.Context, actor, courseID int64) (AdminCourseDetail, error) {
	var d AdminCourseDetail
	err := s.tx(ctx, actor, []permissions.Permission{permissions.Administrator}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT c.id, c.public_id::text, c.owner_id, c.subject_id, c.language, c.published_revision_id, c.archived_at, c.created_at,
			       `+suspendedSQL+`, u.display_name
			  FROM courses c JOIN users u ON u.id = c.owner_id WHERE c.id = $1`, courseID).Scan(
			&d.ID, &d.PublicID, &d.OwnerID, &d.SubjectID, &d.Language, &d.PublishedRevisionID, &d.ArchivedAt, &d.CreatedAt,
			&d.Suspended, &d.OwnerName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT r.id, r.revision_number, r.status, r.kind, r.title, r.author_id, u.display_name, r.updated_at, r.submitted_at,
			       r.withdrawn_at,
			       EXISTS (SELECT 1 FROM course_publication_audit a WHERE a.course_id = r.course_id AND a.revision_id = r.id),
			       d.action, d.reviewer_id, ru.display_name, d.comment, d.decided_at
			  FROM course_revisions r
			  JOIN users u ON u.id = r.author_id
			  LEFT JOIN course_review_decisions d ON d.revision_id = r.id
			  LEFT JOIN users ru ON ru.id = d.reviewer_id
			 WHERE r.course_id = $1
			 ORDER BY r.revision_number DESC`, courseID)
		if err != nil {
			return err
		}
		d.Revisions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminRevision, error) {
			var r AdminRevision
			var status string
			var action, reviewerName *string
			var reviewer *int64
			var comment *string
			var decided *time.Time
			err := row.Scan(&r.ID, &r.Number, &status, &r.Kind, &r.Title, &r.AuthorID, &r.AuthorName, &r.UpdatedAt, &r.SubmittedAt,
				&r.WithdrawnAt, &r.EverPublished, &action, &reviewer, &reviewerName, &comment, &decided)
			r.Status = Status(status)
			if err == nil && action != nil {
				r.Decision = &Decision{Action: *action, ReviewerID: *reviewer, ReviewerName: *reviewerName, Comment: comment, DecidedAt: *decided}
			}
			return r, err
		})
		if err != nil {
			return err
		}
		if len(d.Revisions) > 0 {
			l := d.Revisions[0]
			d.Title = l.Title
			d.Latest = &RevisionSummary{ID: l.ID, Number: l.Number, Status: l.Status, Title: l.Title, UpdatedAt: l.UpdatedAt,
				Withdrawn: l.WithdrawnAt != nil}
		}
		rows, err = tx.Query(ctx, `
			SELECT a.action, a.revision_id, a.previous_revision_id, a.actor_id, u.display_name, a.reason, a.occurred_at
			  FROM course_publication_audit a JOIN users u ON u.id = a.actor_id
			 WHERE a.course_id = $1 ORDER BY a.id DESC LIMIT $2`, courseID, MaxQueue)
		if err != nil {
			return err
		}
		d.History, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (PointerEvent, error) {
			e := PointerEvent{PointerChange: PointerChange{CourseID: courseID}}
			err := row.Scan(&e.Action, &e.RevisionID, &e.PreviousRevisionID, &e.ActorID, &e.ActorName, &e.Reason, &e.OccurredAt)
			return e, err
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT a.id, a.actor_id, u.display_name, a.action, a.reason, a.revision_id, a.from_owner_id, a.to_owner_id, a.occurred_at
			  FROM course_lifecycle_audit a JOIN users u ON u.id = a.actor_id
			 WHERE a.course_id = $1 ORDER BY a.id DESC LIMIT $2`, courseID, MaxQueue)
		if err != nil {
			return err
		}
		d.Lifecycle, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (NamedLifecycleEvent, error) {
			e := NamedLifecycleEvent{LifecycleEvent: LifecycleEvent{CourseID: courseID}}
			err := row.Scan(&e.ID, &e.ActorID, &e.ActorName, &e.Action, &e.Reason, &e.RevisionID, &e.FromOwnerID, &e.ToOwnerID, &e.OccurredAt)
			return e, err
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT u.id, u.display_name FROM users u JOIN user_permissions p ON p.user_id = u.id AND p.permission = 'author'
			 WHERE u.disabled_at IS NULL AND u.id <> $1
			 ORDER BY u.display_name, u.id LIMIT $2`, d.OwnerID, MaxQueue)
		if err != nil {
			return err
		}
		d.Authors, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Account, error) {
			var a Account
			err := row.Scan(&a.ID, &a.Name)
			return a, err
		})
		return err
	})
	if d.Authors == nil {
		d.Authors = []Account{}
	}
	if d.Revisions == nil {
		d.Revisions = []AdminRevision{}
	}
	if d.History == nil {
		d.History = []PointerEvent{}
	}
	if d.Lifecycle == nil {
		d.Lifecycle = []NamedLifecycleEvent{}
	}
	return d, err
}
