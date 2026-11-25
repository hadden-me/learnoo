// Package accounts disables and enables accounts. A disabled account cannot
// sign in, its sessions are revoked, and it holds no effective permissions
// (its grants stay stored). Every change is audited in account_status_audit
// in the same transaction, with the actor's user id or, for operator tools,
// its operator label, and disabling the last active administrator is
// refused under the same lock as revoking administrator
// (permissions.GuardAdministratorRemoval).
//
// Only the operator CLI (cmd/server) may call Disable and Enable, and only
// this package may write users.disabled_at or account_status_audit;
// internal/server tests enforce both.
package accounts

import (
	"context"
	"fmt"
	"time"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store changes account status.
type Store struct {
	db *pgxpool.Pool
}

// NewStore builds a Store. Status and audit times are the database
// transaction time (now()), so disabled_at and the audit row's occurred_at
// always agree.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// Disable disables target and deletes its sessions. Disabling a disabled
// account is a no-op with no audit row. It returns permissions.ErrDenied for
// an unauthorized actor, permissions.ErrUnknownUser for a missing target and
// permissions.ErrLastAdministrator, changing nothing, if target is the only
// active administrator. It reports whether anything changed.
func (s *Store) Disable(ctx context.Context, actor permissions.Actor, target int64) (bool, error) {
	return s.change(ctx, actor, target, "disable")
}

// Enable re-enables target and deletes any sessions it has, so the account
// starts without sessions; enabling an active account is a no-op.
func (s *Store) Enable(ctx context.Context, actor permissions.Actor, target int64) (bool, error) {
	return s.change(ctx, actor, target, "enable")
}

func (s *Store) change(ctx context.Context, actor permissions.Actor, target int64, action string) (bool, error) {
	var changed bool
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// Both directions change the set of active administrators, so both
		// take the administrators lock first, as every such operation does;
		// then Authorize's row locks; then the guard and the change.
		if err := permissions.LockAdministrators(ctx, tx); err != nil {
			return err
		}
		if err := permissions.Authorize(ctx, tx, actor); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, target).Scan(&exists); err != nil {
			return fmt.Errorf("load user: %w", err)
		}
		if !exists {
			return permissions.ErrUnknownUser
		}
		var sql string
		if action == "disable" {
			if err := permissions.GuardAdministratorRemoval(ctx, tx, target); err != nil {
				return err
			}
			sql = `UPDATE users SET disabled_at = now(), updated_at = now() WHERE id = $1 AND disabled_at IS NULL`
		} else {
			sql = `UPDATE users SET disabled_at = NULL, updated_at = now() WHERE id = $1 AND disabled_at IS NOT NULL`
		}
		tag, err := tx.Exec(ctx, sql, target)
		if err != nil {
			return fmt.Errorf("%s user: %w", action, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		changed = true
		// Disabling revokes every session. Enabling does too: a session
		// must never outlive a disable, so none may come back on enable,
		// even one a racing login created (auth.Login re-checks the account
		// under a row lock, but enabling starts clean regardless).
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, target); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO account_status_audit (actor_id, operator_label, target_user_id, action)
			VALUES ($1, $2, $3, $4)`, actor.AuditID(), actor.AuditLabel(), target, action)
		if err != nil {
			return fmt.Errorf("audit account status: %w", err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// AuditEntry is one row of the account status audit.
type AuditEntry struct {
	OccurredAt time.Time
	ActorID    *int64 // nil for operator tools
	// OperatorLabel is the self-asserted label of the operator tool that
	// made the change, or "" when ActorID is set.
	OperatorLabel string
	TargetID      int64
	Action        string // "disable" or "enable"
}

// Audit returns the status history of target, oldest first.
func (s *Store) Audit(ctx context.Context, target int64) ([]AuditEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT occurred_at, actor_id, coalesce(operator_label, ''), target_user_id, action
		FROM account_status_audit WHERE target_user_id = $1 ORDER BY id`, target)
	if err != nil {
		return nil, fmt.Errorf("load account status audit: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		err := r.Scan(&e.OccurredAt, &e.ActorID, &e.OperatorLabel, &e.TargetID, &e.Action)
		return e, err
	})
}
