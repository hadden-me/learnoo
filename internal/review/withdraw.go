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
)

// Withdrawal, emergency unpublication and restoration (LRN-009).
//
// The database keeps withdrawals, the published pointer and its audit
// consistent (migrations/0007_withdrawal_restoration.sql): every pointer
// change, including to NULL, needs a publication_audit row of the same
// transaction; a withdrawn revision can never be named by the pointer; a
// withdrawal of a pending approved candidate closes it. This file adds the
// caller checks and the expected-current-pointer comparison:
//
//   - Withdraw and Unpublish require the global administrator permission.
//   - Restore requires publisher or administrator.
//
// Every operation names the pointer the caller expects to replace. It is
// compared with the current pointer under the lesson row lock; a mismatch
// returns ErrStalePointer and changes nothing. Lock order: the lesson row,
// then its revision rows.

// MaxReason bounds a withdrawal or unpublication reason, in characters
// (database check too).
const MaxReason = 2000

var (
	// ErrWithdrawn means the revision was withdrawn: it can never be
	// published or restored.
	ErrWithdrawn = errors.New("revision is withdrawn")
	// ErrAlreadyWithdrawn means the revision to withdraw is already withdrawn.
	ErrAlreadyWithdrawn = errors.New("revision is already withdrawn")
	// ErrStalePointer means the lesson's published revision is not the one
	// the caller expected. Nothing was changed.
	ErrStalePointer = errors.New("published revision is not the expected one")
	// ErrNotPublished means the lesson has no published revision to unpublish.
	ErrNotPublished = errors.New("lesson is not published")
	// ErrNeverPublished means the revision to restore was never published.
	// Approved revisions that were never published go through Publish.
	ErrNeverPublished = errors.New("revision was never published")
	// ErrAlreadyCurrent means the revision to restore is already published.
	ErrAlreadyCurrent = errors.New("revision is already the published revision")
	// ErrIneligible means the revision to restore no longer passes the
	// current content or media checks.
	ErrIneligible = errors.New("revision is no longer eligible for publication")
	// ErrReasonRequired means a withdrawal or unpublication has no reason.
	ErrReasonRequired = errors.New("a reason is required")
	// ErrInvalidReason means the reason is too long or not valid text.
	ErrInvalidReason = errors.New("invalid reason")
)

// PointerChange is one audited change of a lesson's published revision.
type PointerChange struct {
	LessonID int64
	// Action is "publish", "restore" or "unpublish".
	Action string
	// RevisionID is the new published revision, nil after unpublication.
	RevisionID         *int64
	PreviousRevisionID *int64
	ActorID            int64
	// Reason is set for unpublication only.
	Reason     string
	OccurredAt time.Time
}

// Withdrawal is the result of Withdraw.
type Withdrawal struct {
	LessonID    int64
	RevisionID  int64
	ActorID     int64
	Reason      string
	WithdrawnAt time.Time
	// Unpublished is set when the revision was the published one: the
	// lesson was unpublished in the same transaction.
	Unpublished *PointerChange
}

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

func (s *Store) checkHooks(ctx context.Context, tx pgx.Tx, check func() error) error {
	if testHookBeforeCheck != nil {
		testHookBeforeCheck()
	}
	if err := check(); err != nil {
		return err
	}
	if testHookAfterCheck != nil {
		testHookAfterCheck(tx)
	}
	return nil
}

// lockRevisionLesson locks the revision's lesson FOR UPDATE and returns its ID,
// current pointer and archival state.
func lockRevisionLesson(ctx context.Context, tx pgx.Tx, revisionID int64) (lessonID int64, current *int64, archived bool, err error) {
	// A revision's lesson never changes, so reading it unlocked first is safe.
	err = tx.QueryRow(ctx, `SELECT lesson_id FROM lesson_revisions WHERE id = $1`, revisionID).Scan(&lessonID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, ErrNotFound
	}
	if err != nil {
		return 0, nil, false, err
	}
	err = tx.QueryRow(ctx, `SELECT published_revision_id, archived_at IS NOT NULL FROM lessons WHERE id = $1 FOR UPDATE`,
		lessonID).Scan(&current, &archived)
	return lessonID, current, archived, err
}

func isWithdrawn(ctx context.Context, tx pgx.Tx, revisionID int64) (bool, error) {
	var w bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM revision_withdrawals WHERE revision_id = $1)`, revisionID).Scan(&w)
	return w, err
}

// unpublishTx audits and clears the lesson's pointer, which must be current.
func unpublishTx(ctx context.Context, tx pgx.Tx, actor, lessonID, current int64, reason string) (PointerChange, error) {
	ch := PointerChange{LessonID: lessonID, Action: "unpublish", PreviousRevisionID: &current, ActorID: actor, Reason: reason}
	err := tx.QueryRow(ctx, `
		INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action, reason)
		VALUES ($1, NULL, $2, $3, 'unpublish', $4)
		RETURNING occurred_at`, lessonID, current, actor, reason).Scan(&ch.OccurredAt)
	if err != nil {
		return PointerChange{}, mapErr(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE lessons SET published_revision_id = NULL WHERE id = $1`, lessonID); err != nil {
		return PointerChange{}, mapErr(err)
	}
	return ch, nil
}

// Withdraw withdraws an approved revision as actor (administrator) with a
// required reason. expected is the lesson's published revision the caller
// expects (nil for none); a mismatch returns ErrStalePointer. If the
// revision is the published one, the lesson is unpublished in the same
// transaction. A withdrawn pending candidate is closed by the database, so
// the lesson accepts a new draft. A withdrawn revision can never be
// published or restored.
func (s *Store) Withdraw(ctx context.Context, actor, revisionID int64, reason string, expected *int64) (Withdrawal, error) {
	reason, err := normalizeReason(reason)
	if err != nil {
		return Withdrawal{}, err
	}
	var out Withdrawal
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := s.checkHooks(ctx, tx, func() error {
			return permissions.RequireTx(ctx, tx, actor, permissions.Administrator)
		}); err != nil {
			return err
		}
		lessonID, current, _, err := lockRevisionLesson(ctx, tx, revisionID)
		if err != nil {
			return err
		}
		var status lessons.Status
		if err := tx.QueryRow(ctx, `SELECT status FROM lesson_revisions WHERE id = $1 FOR SHARE`, revisionID).Scan(&status); err != nil {
			return err
		}
		withdrawn, err := isWithdrawn(ctx, tx, revisionID)
		if err != nil {
			return err
		}
		switch {
		case withdrawn:
			return ErrAlreadyWithdrawn
		case status != lessons.StatusApproved:
			return ErrNotApproved
		case !sameID(expected, current):
			return ErrStalePointer
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO revision_withdrawals (lesson_id, revision_id, actor_id, reason)
			VALUES ($1, $2, $3, $4)
			RETURNING occurred_at`, lessonID, revisionID, actor, reason).Scan(&out.WithdrawnAt)
		if err != nil {
			return mapErr(err)
		}
		if current != nil && *current == revisionID {
			ch, err := unpublishTx(ctx, tx, actor, lessonID, revisionID, reason)
			if err != nil {
				return err
			}
			out.Unpublished = &ch
		}
		out.LessonID, out.RevisionID, out.ActorID, out.Reason = lessonID, revisionID, actor, reason
		return nil
	})
	if err != nil {
		return Withdrawal{}, err
	}
	return out, nil
}

// Unpublish clears the lesson's published revision as actor (administrator,
// emergency unpublication) with a required reason. expected is the
// published revision the caller expects to remove; a mismatch returns
// ErrStalePointer, and a lesson without a published revision
// ErrNotPublished. The revision itself stays approved and can be restored.
func (s *Store) Unpublish(ctx context.Context, actor, lessonID int64, reason string, expected int64) (PointerChange, error) {
	reason, err := normalizeReason(reason)
	if err != nil {
		return PointerChange{}, err
	}
	var out PointerChange
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := s.checkHooks(ctx, tx, func() error {
			return permissions.RequireTx(ctx, tx, actor, permissions.Administrator)
		}); err != nil {
			return err
		}
		var current *int64
		err := tx.QueryRow(ctx, `SELECT published_revision_id FROM lessons WHERE id = $1 FOR UPDATE`, lessonID).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case current == nil:
			return ErrNotPublished
		case *current != expected:
			return ErrStalePointer
		}
		out, err = unpublishTx(ctx, tx, actor, lessonID, *current, reason)
		return err
	})
	if err != nil {
		return PointerChange{}, err
	}
	return out, nil
}

// Restore points the revision's lesson back at a revision that was
// published before, as actor (publisher or administrator). expected is the
// published revision the caller expects to replace (nil for none); a
// mismatch returns ErrStalePointer. The revision must be approved, never
// withdrawn, previously published and still eligible (content valid under
// the current schema, media references resolving). The historical base
// check of Publish is deliberately not applied.
func (s *Store) Restore(ctx context.Context, actor, revisionID int64, expected *int64) (PointerChange, error) {
	var out PointerChange
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := s.checkHooks(ctx, tx, func() error {
			return permissions.RequireAnyTx(ctx, tx, actor, permissions.Publisher, permissions.Administrator)
		}); err != nil {
			return err
		}
		lessonID, current, archived, err := lockRevisionLesson(ctx, tx, revisionID)
		if err != nil {
			return err
		}
		var status lessons.Status
		if err := tx.QueryRow(ctx, `SELECT status FROM lesson_revisions WHERE id = $1 FOR SHARE`, revisionID).Scan(&status); err != nil {
			return err
		}
		withdrawn, err := isWithdrawn(ctx, tx, revisionID)
		if err != nil {
			return err
		}
		var published bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM publication_audit WHERE revision_id = $1 AND action = 'publish')`,
			revisionID).Scan(&published); err != nil {
			return err
		}
		switch {
		case status != lessons.StatusApproved:
			return ErrNotApproved
		case withdrawn:
			return ErrWithdrawn
		case !published:
			return ErrNeverPublished
		case archived:
			return ErrArchived
		case !sameID(expected, current):
			return ErrStalePointer
		case current != nil && *current == revisionID:
			return ErrAlreadyCurrent
		}
		if err := checkEligible(ctx, tx, revisionID); err != nil {
			return err
		}
		out = PointerChange{LessonID: lessonID, Action: "restore", RevisionID: &revisionID, PreviousRevisionID: current, ActorID: actor}
		err = tx.QueryRow(ctx, `
			INSERT INTO publication_audit (lesson_id, revision_id, previous_revision_id, actor_id, action)
			VALUES ($1, $2, $3, $4, 'restore')
			RETURNING occurred_at`, lessonID, revisionID, current, actor).Scan(&out.OccurredAt)
		if err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE lessons SET published_revision_id = $2 WHERE id = $1`, lessonID, revisionID); err != nil {
			return mapErr(err)
		}
		return nil
	})
	if err != nil {
		return PointerChange{}, err
	}
	return out, nil
}

// checkEligible rechecks a revision before restoration: its content must
// still pass the current schema v1 validator, and every media reference
// must resolve to an asset. Media assets are immutable and never deleted
// (migrations/0004), so the second check only fails on a damaged database;
// there is no media withdrawal state yet.
func checkEligible(ctx context.Context, tx pgx.Tx, revisionID int64) error {
	if err := lessons.CheckStoredContentTx(ctx, tx, revisionID); err != nil {
		if errors.Is(err, lessons.ErrInvalidContent) {
			return fmt.Errorf("%w: %w", ErrIneligible, err)
		}
		return err
	}
	var missing int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM revision_media rm
		LEFT JOIN media_assets a ON a.id = rm.media_asset_id
		WHERE rm.revision_id = $1 AND a.id IS NULL`, revisionID).Scan(&missing)
	if err != nil {
		return err
	}
	if missing > 0 {
		return fmt.Errorf("%w: %d media references do not resolve", ErrIneligible, missing)
	}
	return nil
}

// PointerHistory returns every audited change of the lesson's published
// revision (publication, restoration, unpublication), oldest first. It
// performs no authorization.
func (s *Store) PointerHistory(ctx context.Context, lessonID int64) ([]PointerChange, error) {
	rows, err := s.db.Query(ctx, `SELECT lesson_id, action, revision_id, previous_revision_id, actor_id,
		coalesce(reason, ''), occurred_at
		FROM publication_audit WHERE lesson_id = $1 ORDER BY id`, lessonID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PointerChange, error) {
		var c PointerChange
		err := r.Scan(&c.LessonID, &c.Action, &c.RevisionID, &c.PreviousRevisionID, &c.ActorID, &c.Reason, &c.OccurredAt)
		return c, err
	})
}

// WithdrawalRecord is one stored withdrawal.
type WithdrawalRecord struct {
	LessonID    int64
	RevisionID  int64
	ActorID     int64
	Reason      string
	WithdrawnAt time.Time
}

// Withdrawals returns the withdrawals of a lesson, oldest first. It performs
// no authorization.
func (s *Store) Withdrawals(ctx context.Context, lessonID int64) ([]WithdrawalRecord, error) {
	rows, err := s.db.Query(ctx, `SELECT lesson_id, revision_id, actor_id, reason, occurred_at
		FROM revision_withdrawals WHERE lesson_id = $1 ORDER BY id`, lessonID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (WithdrawalRecord, error) {
		var w WithdrawalRecord
		err := r.Scan(&w.LessonID, &w.RevisionID, &w.ActorID, &w.Reason, &w.WithdrawnAt)
		return w, err
	})
}
