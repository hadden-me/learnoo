package users

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The account directory read by the administration API (LMS-B10): accounts
// with their status and stored grants, and their permission and status
// history. Reads only: grants and status change only through
// internal/permissions and internal/accounts. Callers authorize.

// DirectoryPageSize is the number of accounts per page of List.
const DirectoryPageSize = 50

// MaxHistory bounds the events History returns.
const MaxHistory = 200

// ReviewerSubject is a subject an account holds a reviewer grant for.
type ReviewerSubject struct {
	ID        int64
	Code      string
	NameFr    *string
	NameAr    *string
	RetiredAt *time.Time
}

// Account is an account with its stored grants, whether or not it is
// disabled (a disabled account holds no effective permission).
type Account struct {
	User
	// Permissions are the stored global grants in the order author,
	// publisher, administrator.
	Permissions []string
	// ReviewerSubjects are the stored reviewer grants by subject code.
	ReviewerSubjects []ReviewerSubject
}

// Filter narrows List. Zero values do not filter.
type Filter struct {
	// Query matches case-insensitively as a substring of the email or the
	// display name.
	Query string
	// Permission is "author", "publisher", "administrator", "reviewer" (any
	// reviewer grant) or "none" (no grant at all).
	Permission string
	// Status is "active" or "disabled".
	Status string
	// After lists accounts with a greater ID (keyset paging).
	After int64
}

// ErrInvalidFilter means a Filter field has an unknown value.
var ErrInvalidFilter = errors.New("invalid filter")

const accountColumns = `u.id, u.email, u.display_name, u.created_at, u.disabled_at IS NOT NULL,
	coalesce((SELECT array_agg(p.permission ORDER BY array_position(ARRAY['author', 'publisher', 'administrator'], p.permission))
	            FROM user_permissions p WHERE p.user_id = u.id), '{}')`

// List returns at most DirectoryPageSize accounts by ascending ID and the
// ID to pass as After for the next page (0 on the last page), from one
// snapshot.
func List(ctx context.Context, db *pgxpool.Pool, f Filter) ([]Account, int64, error) {
	switch f.Permission {
	case "", "author", "publisher", "administrator", "reviewer", "none":
	default:
		return nil, 0, ErrInvalidFilter
	}
	switch f.Status {
	case "", "active", "disabled":
	default:
		return nil, 0, ErrInvalidFilter
	}
	var out []Account
	var next int64
	err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+accountColumns+`
			  FROM users u
			 WHERE u.id > $1
			   AND ($2 = '' OR strpos(lower(u.email), lower($2)) > 0 OR strpos(lower(u.display_name), lower($2)) > 0)
			   AND CASE $3
			         WHEN '' THEN true
			         WHEN 'reviewer' THEN EXISTS (SELECT 1 FROM reviewer_assignments r WHERE r.user_id = u.id)
			         WHEN 'none' THEN NOT EXISTS (SELECT 1 FROM user_permissions p WHERE p.user_id = u.id)
			                      AND NOT EXISTS (SELECT 1 FROM reviewer_assignments r WHERE r.user_id = u.id)
			         ELSE EXISTS (SELECT 1 FROM user_permissions p WHERE p.user_id = u.id AND p.permission = $3)
			       END
			   AND ($4 = '' OR ($4 = 'disabled') = (u.disabled_at IS NOT NULL))
			 ORDER BY u.id
			 LIMIT $5`, f.After, f.Query, f.Permission, f.Status, DirectoryPageSize+1)
		if err != nil {
			return fmt.Errorf("list accounts: %w", err)
		}
		out, err = pgx.CollectRows(rows, scanAccount)
		if err != nil {
			return fmt.Errorf("list accounts: %w", err)
		}
		if len(out) > DirectoryPageSize {
			out = out[:DirectoryPageSize]
			next = out[len(out)-1].ID
		}
		return attachSubjects(ctx, tx, out)
	})
	if err != nil {
		return nil, 0, err
	}
	return out, next, nil
}

// Get returns one account, or ErrNotFound.
func Get(ctx context.Context, db *pgxpool.Pool, id int64) (Account, error) {
	var a Account
	err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+accountColumns+` FROM users u WHERE u.id = $1`, id)
		if err != nil {
			return fmt.Errorf("load account: %w", err)
		}
		list, err := pgx.CollectRows(rows, scanAccount)
		if err != nil {
			return fmt.Errorf("load account: %w", err)
		}
		if len(list) == 0 {
			return ErrNotFound
		}
		if err := attachSubjects(ctx, tx, list); err != nil {
			return err
		}
		a = list[0]
		return nil
	})
	return a, err
}

func scanAccount(r pgx.CollectableRow) (Account, error) {
	var a Account
	err := r.Scan(&a.ID, &a.Email, &a.DisplayName, &a.CreatedAt, &a.Disabled, &a.Permissions)
	if a.Permissions == nil {
		a.Permissions = []string{}
	}
	a.ReviewerSubjects = []ReviewerSubject{}
	return a, err
}

func attachSubjects(ctx context.Context, tx pgx.Tx, accounts []Account) error {
	if len(accounts) == 0 {
		return nil
	}
	ids := make([]int64, len(accounts))
	index := make(map[int64]int, len(accounts))
	for i, a := range accounts {
		ids[i] = a.ID
		index[a.ID] = i
	}
	rows, err := tx.Query(ctx, `
		SELECT r.user_id, s.id, s.code, s.name_fr, s.name_ar, s.retired_at
		  FROM reviewer_assignments r JOIN subjects s ON s.id = r.subject_id
		 WHERE r.user_id = ANY ($1)
		 ORDER BY s.code COLLATE "C"`, ids)
	if err != nil {
		return fmt.Errorf("list reviewer subjects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var user int64
		var s ReviewerSubject
		if err := rows.Scan(&user, &s.ID, &s.Code, &s.NameFr, &s.NameAr, &s.RetiredAt); err != nil {
			return fmt.Errorf("list reviewer subjects: %w", err)
		}
		i := index[user]
		accounts[i].ReviewerSubjects = append(accounts[i].ReviewerSubjects, s)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list reviewer subjects: %w", err)
	}
	return nil
}

// Event is one entry of an account's permission or status history.
type Event struct {
	Kind          string // "permission" or "status"
	Action        string // grant, revoke, disable, enable
	Permission    *string
	SubjectID     *int64
	SubjectCode   *string
	ActorID       *int64
	ActorName     *string
	OperatorLabel *string
	OccurredAt    time.Time
}

// History returns the permission_audit and account_status_audit rows of
// account id, newest first, at most MaxHistory. Exactly one of ActorID and
// OperatorLabel is set on each.
func History(ctx context.Context, db *pgxpool.Pool, id int64) ([]Event, error) {
	rows, err := db.Query(ctx, `
		SELECT kind, action, permission, subject_id, subject_code, actor_id, actor_name, operator_label, occurred_at
		  FROM (SELECT 'permission' AS kind, a.action, a.permission, a.subject_id, s.code AS subject_code,
		               a.actor_id, actor.display_name AS actor_name, a.operator_label, a.occurred_at, a.id
		          FROM permission_audit a
		          LEFT JOIN subjects s ON s.id = a.subject_id
		          LEFT JOIN users actor ON actor.id = a.actor_id
		         WHERE a.target_user_id = $1
		        UNION ALL
		        SELECT 'status', a.action, NULL, NULL, NULL,
		               a.actor_id, actor.display_name, a.operator_label, a.occurred_at, a.id
		          FROM account_status_audit a
		          LEFT JOIN users actor ON actor.id = a.actor_id
		         WHERE a.target_user_id = $1) h
		 ORDER BY occurred_at DESC, kind, id DESC
		 LIMIT $2`, id, MaxHistory)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.Kind, &e.Action, &e.Permission, &e.SubjectID, &e.SubjectCode, &e.ActorID, &e.ActorName, &e.OperatorLabel, &e.OccurredAt)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	return events, nil
}
