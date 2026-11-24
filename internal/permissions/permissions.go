// Package permissions stores and checks the four independent permissions of
// the confirmed role model (docs/DECISIONS.md "Role model"):
//
//   - author, publisher, administrator: global grants;
//   - reviewer: granted per subject.
//
// No permission implies another: reviewing never implies publishing, and
// administrator does not imply reviewer (administrators need a subject
// assignment like anyone else). Every check denies by default: unknown
// permissions, missing rows, disabled accounts and database errors all deny.
package permissions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Permission is one independently granted capability.
type Permission string

const (
	Author        Permission = "author"
	Reviewer      Permission = "reviewer" // always scoped to a subject
	Publisher     Permission = "publisher"
	Administrator Permission = "administrator"
)

// global reports whether p is granted without a subject.
func (p Permission) global() bool {
	return p == Author || p == Publisher || p == Administrator
}

// AccountType is the user-facing presentation of a set of permissions.
type AccountType string

const (
	Student AccountType = "student"
	Teacher AccountType = "teacher"
	Admin   AccountType = "administrator"
)

// DefaultPermissions returns the global permissions granted when an account
// of the given type is set up. reviewer (per subject) and, for teachers,
// publisher are never defaults; they are granted separately.
func DefaultPermissions(t AccountType) ([]Permission, error) {
	switch t {
	case Student:
		return nil, nil
	case Teacher:
		return []Permission{Author}, nil
	case Admin:
		return []Permission{Administrator, Publisher, Author}, nil
	}
	return nil, fmt.Errorf("unknown account type %q", t)
}

var (
	// ErrDenied means the user lacks the permission (or it could not be proven).
	ErrDenied = errors.New("permission denied")
	// ErrUnknownSubject means the subject does not exist.
	ErrUnknownSubject = errors.New("unknown subject")
	// ErrUnknownUser means the target user does not exist.
	ErrUnknownUser = errors.New("unknown user")
)

// Set is a user's effective permissions at load time.
type Set struct {
	UserID           int64
	global           map[Permission]bool
	reviewerSubjects map[int64]bool
}

// Has reports a global permission. It is always false for Reviewer, which
// must be checked with CanReview and a subject.
func (s Set) Has(p Permission) bool { return p.global() && s.global[p] }

// CanReview reports whether the user is assigned to review subjectID.
func (s Set) CanReview(subjectID int64) bool { return s.reviewerSubjects[subjectID] }

// Global lists the held global permissions in a stable order.
func (s Set) Global() []Permission {
	var out []Permission
	for _, p := range []Permission{Author, Publisher, Administrator} {
		if s.global[p] {
			out = append(out, p)
		}
	}
	return out
}

// ReviewerSubjects lists the subjects the user may review.
func (s Set) ReviewerSubjects() []int64 {
	out := make([]int64, 0, len(s.reviewerSubjects))
	for id := range s.reviewerSubjects {
		out = append(out, id)
	}
	return out
}

// AccountType derives the presented account type from the permissions.
func (s Set) AccountType() AccountType {
	switch {
	case s.global[Administrator]:
		return Admin
	case s.global[Author]:
		return Teacher
	}
	return Student
}

// Store reads and changes permissions.
type Store struct {
	db  *pgxpool.Pool
	now func() time.Time
}

// NewStore builds a Store. now may be nil to use the wall clock.
func NewStore(db *pgxpool.Pool, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

// Load returns the effective permissions of userID. Unknown or disabled
// users get an empty set.
func (s *Store) Load(ctx context.Context, userID int64) (Set, error) {
	return load(ctx, s.db, userID)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func load(ctx context.Context, q querier, userID int64) (Set, error) {
	set := Set{UserID: userID, global: map[Permission]bool{}, reviewerSubjects: map[int64]bool{}}
	rows, err := q.Query(ctx, `
		SELECT p.permission, NULL::bigint
		  FROM user_permissions p JOIN users u ON u.id = p.user_id
		 WHERE p.user_id = $1 AND u.disabled_at IS NULL
		UNION ALL
		SELECT 'reviewer', r.subject_id
		  FROM reviewer_assignments r JOIN users u ON u.id = r.user_id
		 WHERE r.user_id = $1 AND u.disabled_at IS NULL`, userID)
	if err != nil {
		return Set{}, fmt.Errorf("load permissions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var subject *int64
		if err := rows.Scan(&p, &subject); err != nil {
			return Set{}, err
		}
		perm := Permission(p)
		switch {
		case perm == Reviewer && subject != nil:
			set.reviewerSubjects[*subject] = true
		case perm.global():
			set.global[perm] = true
		}
	}
	if err := rows.Err(); err != nil {
		return Set{}, fmt.Errorf("load permissions: %w", err)
	}
	return set, nil
}

// Require returns nil only if userID holds the global permission p.
// Reviewer always fails here: use RequireReviewer with a subject.
func (s *Store) Require(ctx context.Context, userID int64, p Permission) error {
	if !p.global() {
		return ErrDenied
	}
	set, err := s.Load(ctx, userID)
	if err != nil {
		return errors.Join(ErrDenied, err)
	}
	if !set.Has(p) {
		return ErrDenied
	}
	return nil
}

// RequireReviewer returns nil only if userID is assigned to review subjectID.
func (s *Store) RequireReviewer(ctx context.Context, userID, subjectID int64) error {
	set, err := s.Load(ctx, userID)
	if err != nil {
		return errors.Join(ErrDenied, err)
	}
	if !set.CanReview(subjectID) {
		return ErrDenied
	}
	return nil
}

// Actor identifies who changes permissions. Operator is a local tool with
// direct database access (the future LRN-013 CLI); a user actor must hold
// administrator at the time of the change.
type Actor struct {
	userID   int64
	operator bool
}

// Operator is the actor for trusted local tools; audited with no actor_id.
var Operator = Actor{operator: true}

// UserActor is a signed-in user acting through the application.
func UserActor(userID int64) Actor { return Actor{userID: userID} }

// Grant gives target a global permission. Granting a held permission is a
// no-op and writes no audit row. It reports whether anything changed.
func (s *Store) Grant(ctx context.Context, actor Actor, target int64, p Permission) (bool, error) {
	if !p.global() {
		return false, fmt.Errorf("grant %q: use GrantReviewer for subject-scoped permissions", p)
	}
	return s.change(ctx, actor, target, "grant", p, nil)
}

// Revoke removes a global permission; revoking an absent one is a no-op.
func (s *Store) Revoke(ctx context.Context, actor Actor, target int64, p Permission) (bool, error) {
	if !p.global() {
		return false, fmt.Errorf("revoke %q: use RevokeReviewer for subject-scoped permissions", p)
	}
	return s.change(ctx, actor, target, "revoke", p, nil)
}

// GrantReviewer assigns target to review subjectID.
func (s *Store) GrantReviewer(ctx context.Context, actor Actor, target, subjectID int64) (bool, error) {
	return s.change(ctx, actor, target, "grant", Reviewer, &subjectID)
}

// RevokeReviewer removes target's review assignment for subjectID.
func (s *Store) RevokeReviewer(ctx context.Context, actor Actor, target, subjectID int64) (bool, error) {
	return s.change(ctx, actor, target, "revoke", Reviewer, &subjectID)
}

// GrantDefaults grants the account type's default permissions in one
// transaction, each audited.
func (s *Store) GrantDefaults(ctx context.Context, actor Actor, target int64, t AccountType) error {
	perms, err := DefaultPermissions(t)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := authorize(ctx, tx, actor); err != nil {
			return err
		}
		for _, p := range perms {
			if _, err := s.apply(ctx, tx, actor, target, "grant", p, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) change(ctx context.Context, actor Actor, target int64, action string, p Permission, subject *int64) (bool, error) {
	var changed bool
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := authorize(ctx, tx, actor); err != nil {
			return err
		}
		var err error
		changed, err = s.apply(ctx, tx, actor, target, action, p, subject)
		return err
	})
	return changed, err
}

// authorize checks the actor inside the transaction. FOR SHARE blocks a
// concurrent revocation of the actor's administrator grant until commit.
func authorize(ctx context.Context, tx pgx.Tx, actor Actor) error {
	if actor.operator {
		return nil
	}
	if actor.userID == 0 {
		return ErrDenied
	}
	var ok bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_permissions p JOIN users u ON u.id = p.user_id
			 WHERE p.user_id = $1 AND p.permission = 'administrator' AND u.disabled_at IS NULL
			   FOR SHARE OF p)`, actor.userID).Scan(&ok)
	if err != nil {
		return errors.Join(ErrDenied, err)
	}
	if !ok {
		return ErrDenied
	}
	return nil
}

func (s *Store) apply(ctx context.Context, tx pgx.Tx, actor Actor, target int64, action string, p Permission, subject *int64) (bool, error) {
	now := s.now().UTC()
	var actorID *int64
	if !actor.operator {
		actorID = &actor.userID
	}
	var tag pgconn.CommandTag
	var err error
	switch {
	case action == "grant" && subject == nil:
		tag, err = tx.Exec(ctx, `INSERT INTO user_permissions (user_id, permission, granted_at, granted_by)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, target, string(p), now, actorID)
	case action == "revoke" && subject == nil:
		tag, err = tx.Exec(ctx, `DELETE FROM user_permissions WHERE user_id = $1 AND permission = $2`, target, string(p))
	case action == "grant":
		tag, err = tx.Exec(ctx, `INSERT INTO reviewer_assignments (user_id, subject_id, granted_at, granted_by)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, target, *subject, now, actorID)
	default:
		tag, err = tx.Exec(ctx, `DELETE FROM reviewer_assignments WHERE user_id = $1 AND subject_id = $2`, target, *subject)
	}
	if err != nil {
		return false, mapFK(err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO permission_audit (occurred_at, actor_id, target_user_id, action, permission, subject_id)
		VALUES ($1, $2, $3, $4, $5, $6)`, now, actorID, target, action, string(p), subject)
	if err != nil {
		return false, fmt.Errorf("audit permission change: %w", mapFK(err))
	}
	return true, nil
}

func mapFK(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		switch pgErr.ConstraintName {
		case "reviewer_assignments_subject_id_fkey", "permission_audit_subject_id_fkey":
			return ErrUnknownSubject
		case "user_permissions_user_id_fkey", "reviewer_assignments_user_id_fkey", "permission_audit_target_user_id_fkey":
			return ErrUnknownUser
		}
	}
	return err
}

// AuditEntry is one row of the permission audit.
type AuditEntry struct {
	OccurredAt time.Time
	ActorID    *int64
	TargetID   int64
	Action     string
	Permission Permission
	SubjectID  *int64
}

// Audit returns the permission history of target, oldest first.
func (s *Store) Audit(ctx context.Context, target int64) ([]AuditEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT occurred_at, actor_id, target_user_id, action, permission, subject_id
		FROM permission_audit WHERE target_user_id = $1 ORDER BY id`, target)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		var p string
		err := r.Scan(&e.OccurredAt, &e.ActorID, &e.TargetID, &e.Action, &p, &e.SubjectID)
		e.Permission = Permission(p)
		return e, err
	})
}
