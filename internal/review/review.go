// Package review records review decisions on submitted lesson revisions and
// publishes approved ones (LRN-007).
//
// The database keeps decisions, statuses, the published pointer and the
// publication audit consistent (migrations/0006_review_publication.sql); this
// package adds the checks that need the caller and maps failures to typed
// errors. Every write rechecks the caller's permission inside its
// transaction with the grant and account rows share-locked, so a concurrent
// revocation or disablement waits for the write or denies it:
//
//   - Approve and RequestChanges require the reviewer permission for the
//     revision's subject; administrators need the assignment like anyone
//     else. The reviewer must not be the revision's author.
//   - Publish requires the global publisher permission. Reviewing never
//     implies publishing, and publishing never bypasses review: only an
//     approved, never-published revision of the lesson whose base is the
//     current published revision can be published.
//
// Lock order matches internal/lessons and the database triggers: the lesson
// row first, then its revision rows.
package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Decision actions as stored in review_decisions.
const (
	ActionApprove        = "approve"
	ActionRequestChanges = "request_changes"
)

// MaxComment bounds a reviewer comment, in characters (database check too).
const MaxComment = 5000

// MaxQueue bounds how many revisions ReviewQueue returns.
const MaxQueue = 200

var (
	// ErrDenied is permissions.ErrDenied: the caller lacks the permission
	// (reviewer for the revision's subject, or publisher), or is disabled.
	ErrDenied = permissions.ErrDenied
	// ErrNotFound is lessons.ErrNotFound: the revision does not exist, or a
	// reviewer read found it outside review.
	ErrNotFound = lessons.ErrNotFound
	// ErrSelfReview means the reviewer authored the revision.
	ErrSelfReview = errors.New("a reviewer cannot decide on their own revision")
	// ErrNotInReview means the revision is not (or no longer) in review.
	ErrNotInReview = errors.New("revision is not in review")
	// ErrCommentRequired means a change request has no comment.
	ErrCommentRequired = errors.New("a comment is required to request changes")
	// ErrInvalidComment means the comment is too long or not valid text.
	ErrInvalidComment = errors.New("invalid comment")
	// ErrNotApproved means the revision to publish is not approved.
	ErrNotApproved = errors.New("revision is not approved")
	// ErrAlreadyPublished means the revision is or was already published,
	// so it is no longer the lesson's candidate. Re-pointing the lesson to
	// an earlier revision is restoration (LRN-009).
	ErrAlreadyPublished = errors.New("revision was already published")
	// ErrStaleBase means the lesson's published revision changed since the
	// revision's draft was created. Nothing was changed.
	ErrStaleBase = errors.New("published revision changed since this revision was drafted")
	// ErrArchived is lessons.ErrArchived: archived lessons are not published.
	ErrArchived = lessons.ErrArchived
	// ErrRejected reports a change refused by a database guard. The Store's
	// own checks normally return a more specific error first.
	ErrRejected = errors.New("change rejected by a database guard")
)

// Store records decisions and publications.
type Store struct {
	db *pgxpool.Pool
}

// NewStore builds a Store.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Test hooks (export_test.go). beforeCheck runs inside the transaction just
// before the in-transaction permission check, afterCheck right after it.
var (
	testHookBeforeCheck func()
	testHookAfterCheck  func(tx pgx.Tx)
)

// normalizeComment trims surrounding whitespace and validates the length.
func normalizeComment(c string) (string, error) {
	c = strings.TrimSpace(c)
	if !utf8.ValidString(c) || strings.ContainsRune(c, 0) || utf8.RuneCountInString(c) > MaxComment {
		return "", fmt.Errorf("%w: at most %d characters of valid text", ErrInvalidComment, MaxComment)
	}
	return c, nil
}

// Approve approves the in-review revision as actor, with an optional comment.
func (s *Store) Approve(ctx context.Context, actor, revisionID int64, comment string) (lessons.Revision, error) {
	return s.decide(ctx, actor, revisionID, ActionApprove, comment)
}

// RequestChanges sends the in-review revision back with a required comment.
// The author then starts a successor draft; the revision stays as history.
func (s *Store) RequestChanges(ctx context.Context, actor, revisionID int64, comment string) (lessons.Revision, error) {
	return s.decide(ctx, actor, revisionID, ActionRequestChanges, comment)
}

func (s *Store) decide(ctx context.Context, actor, revisionID int64, action, comment string) (lessons.Revision, error) {
	comment, err := normalizeComment(comment)
	if err != nil {
		return lessons.Revision{}, err
	}
	if action == ActionRequestChanges && comment == "" {
		return lessons.Revision{}, ErrCommentRequired
	}
	if actor == 0 {
		return lessons.Revision{}, ErrDenied
	}
	var rev lessons.Revision
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// A revision's lesson never changes, so reading it unlocked first is safe.
		var lessonID int64
		err := tx.QueryRow(ctx, `SELECT lesson_id FROM lesson_revisions WHERE id = $1`, revisionID).Scan(&lessonID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Lesson first (shared: no publication meanwhile), then the revision
		// for update, since its status changes below.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM lessons WHERE id = $1 FOR SHARE`, lessonID); err != nil {
			return err
		}
		var author, subject int64
		var status lessons.Status
		err = tx.QueryRow(ctx, `SELECT author_id, subject_id, status FROM lesson_revisions WHERE id = $1 FOR UPDATE`,
			revisionID).Scan(&author, &subject, &status)
		if err != nil {
			return err
		}
		if testHookBeforeCheck != nil {
			testHookBeforeCheck()
		}
		// The permission comes first, so callers without it learn nothing
		// about the revision's author or status.
		if err := permissions.RequireReviewerTx(ctx, tx, actor, subject); err != nil {
			return err
		}
		if testHookAfterCheck != nil {
			testHookAfterCheck(tx)
		}
		if author == actor {
			return ErrSelfReview
		}
		if status != lessons.StatusInReview {
			return ErrNotInReview
		}
		var c *string
		if comment != "" {
			c = &comment
		}
		if _, err := tx.Exec(ctx, `INSERT INTO review_decisions (revision_id, reviewer_id, action, comment)
			VALUES ($1, $2, $3, $4)`, revisionID, actor, action, c); err != nil {
			return mapErr(err)
		}
		next := lessons.StatusApproved
		if action == ActionRequestChanges {
			next = lessons.StatusChangesRequested
		}
		tag, err := tx.Exec(ctx, `UPDATE lesson_revisions SET status = $2, updated_at = now()
			WHERE id = $1 AND status = 'in_review'`, revisionID, string(next))
		if err != nil {
			return mapErr(err)
		}
		if tag.RowsAffected() != 1 {
			return ErrNotInReview
		}
		rev, err = lessons.RevisionTx(ctx, tx, revisionID)
		return err
	})
	return rev, err
}

// Publication is the result of Publish.
type Publication struct {
	LessonID           int64
	RevisionID         int64
	PreviousRevisionID *int64
	ActorID            int64
	PublishedAt        time.Time
}

// Publish points the revision's lesson at the revision, as actor, and audits
// it in the same transaction. The revision must be an approved revision that
// was never published, and its base revision must still be the lesson's
// published revision; otherwise ErrStaleBase is returned and nothing changes.
// The database closes the revision as the lesson's candidate.
func (s *Store) Publish(ctx context.Context, actor, revisionID int64) (Publication, error) {
	var pub Publication
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if testHookBeforeCheck != nil {
			testHookBeforeCheck()
		}
		if err := permissions.RequireTx(ctx, tx, actor, permissions.Publisher); err != nil {
			return err
		}
		if testHookAfterCheck != nil {
			testHookAfterCheck(tx)
		}
		var lessonID int64
		err := tx.QueryRow(ctx, `SELECT lesson_id FROM lesson_revisions WHERE id = $1`, revisionID).Scan(&lessonID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// The lesson row lock serializes publications of the lesson and keeps
		// the pointer stable until it is replaced below.
		var current *int64
		var archived bool
		err = tx.QueryRow(ctx, `SELECT published_revision_id, archived_at IS NOT NULL FROM lessons WHERE id = $1 FOR UPDATE`,
			lessonID).Scan(&current, &archived)
		if err != nil {
			return err
		}
		var status lessons.Status
		var base *int64
		var closed bool
		err = tx.QueryRow(ctx, `SELECT status, base_revision_id, candidate_closed_at IS NOT NULL
			FROM lesson_revisions WHERE id = $1 FOR SHARE`, revisionID).Scan(&status, &base, &closed)
		if err != nil {
			return err
		}
		switch {
		case status != lessons.StatusApproved:
			return ErrNotApproved
		case closed || (current != nil && *current == revisionID):
			return ErrAlreadyPublished
		case archived:
			return ErrArchived
		case !sameID(base, current):
			return ErrStaleBase
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action)
			VALUES ($1, $2, $3, $4, 'publish')
			RETURNING occurred_at`, lessonID, revisionID, current, actor).Scan(&pub.PublishedAt)
		if err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE lessons SET published_revision_id = $2 WHERE id = $1`, lessonID, revisionID); err != nil {
			return mapErr(err)
		}
		pub.LessonID, pub.RevisionID, pub.PreviousRevisionID, pub.ActorID = lessonID, revisionID, current, actor
		return nil
	})
	if err != nil {
		return Publication{}, err
	}
	return pub, nil
}

func sameID(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ReviewRevision returns an in-review revision to a reviewer assigned to its
// subject. Any other caller, and any revision outside review, gets
// ErrNotFound, so revision IDs cannot be probed. A reviewer may read their
// own revision but cannot decide on it.
func (s *Store) ReviewRevision(ctx context.Context, actor, revisionID int64) (lessons.Revision, error) {
	var rev lessons.Revision
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		rev, err = lessons.RevisionTx(ctx, tx, revisionID)
		if err != nil {
			return err
		}
		if err := permissions.RequireReviewerTx(ctx, tx, actor, rev.SubjectID); err != nil {
			if errors.Is(err, ErrDenied) && !deniedWithCause(err) {
				return ErrNotFound
			}
			return err
		}
		if rev.Status != lessons.StatusInReview {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return lessons.Revision{}, err
	}
	return rev, nil
}

// QueueItem is one revision awaiting review.
type QueueItem struct {
	RevisionID  int64
	LessonID    int64
	Number      int
	Title       string
	Language    string
	SubjectID   int64
	ChapterID   *int64
	AuthorID    int64
	SubmittedAt time.Time
}

// ReviewQueue lists in-review revisions, oldest submission first, in the
// subjects actor is an active reviewer of, excluding actor's own revisions
// (they cannot decide on them). subjectID, if non-zero, restricts the list to
// one subject; a subject the actor does not review returns ErrNotFound. At
// most MaxQueue items are returned.
func (s *Store) ReviewQueue(ctx context.Context, actor, subjectID int64) ([]QueueItem, error) {
	var items []QueueItem
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if subjectID != 0 {
			if err := permissions.RequireReviewerTx(ctx, tx, actor, subjectID); err != nil {
				if errors.Is(err, ErrDenied) && !deniedWithCause(err) {
					return ErrNotFound
				}
				return err
			}
		}
		rows, err := tx.Query(ctx, `
			SELECT r.id, r.lesson_id, r.revision_number, r.title, r.language, r.subject_id, r.chapter_id,
			       r.author_id, r.submitted_at
			  FROM lesson_revisions r
			  JOIN reviewer_assignments a ON a.subject_id = r.subject_id AND a.user_id = $1
			  JOIN users u ON u.id = a.user_id AND u.disabled_at IS NULL
			 WHERE r.status = 'in_review' AND r.author_id <> $1 AND ($2 = 0 OR r.subject_id = $2)
			 ORDER BY r.submitted_at, r.id
			 LIMIT $3`, actor, subjectID, MaxQueue)
		if err != nil {
			return err
		}
		items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (QueueItem, error) {
			var q QueueItem
			err := row.Scan(&q.RevisionID, &q.LessonID, &q.Number, &q.Title, &q.Language, &q.SubjectID,
				&q.ChapterID, &q.AuthorID, &q.SubmittedAt)
			return q, err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []QueueItem{}
	}
	return items, nil
}

// PublicationHistory returns the publication audit of a lesson, oldest
// first. It performs no authorization.
func (s *Store) PublicationHistory(ctx context.Context, lessonID int64) ([]Publication, error) {
	rows, err := s.db.Query(ctx, `SELECT lesson_id, revision_id, previous_revision_id, actor_id, occurred_at
		FROM publication_audit WHERE lesson_id = $1 ORDER BY id`, lessonID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Publication, error) {
		var p Publication
		err := r.Scan(&p.LessonID, &p.RevisionID, &p.PreviousRevisionID, &p.ActorID, &p.PublishedAt)
		return p, err
	})
}

// deniedWithCause reports an ErrDenied joined with the database error that
// prevented the check: a failure, not a denial.
func deniedWithCause(err error) bool {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		for _, e := range joined.Unwrap() {
			if !errors.Is(e, ErrDenied) {
				return true
			}
		}
	}
	return false
}

// mapErr turns guard violations into ErrRejected and leaves everything
// else (including deadlocks and serialization failures) unchanged.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "LRN01" {
		return fmt.Errorf("%w: %s", ErrRejected, pgErr.Message)
	}
	return err
}
