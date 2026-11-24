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
	"os"
	"os/user"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	db *pgxpool.Pool
}

// NewStore builds a Store. Grant and audit times are the database
// transaction time (now()), so a grant's granted_at and its audit row's
// occurred_at always agree.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
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
//
// Require reads through the pool, outside any caller transaction, so it is
// only a fast early check (as in Middleware). A state-changing action must
// re-check with RequireTx inside its own transaction.
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

// RequireTx checks the global permission p inside tx. The grant and account
// rows are share-locked until tx ends, so a concurrent revocation or account
// disablement waits for the protected change instead of interleaving with it.
func RequireTx(ctx context.Context, tx pgx.Tx, userID int64, p Permission) error {
	if !p.global() || userID == 0 {
		return ErrDenied
	}
	var ok bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_permissions p JOIN users u ON u.id = p.user_id
			 WHERE p.user_id = $1 AND p.permission = $2 AND u.disabled_at IS NULL
			   FOR SHARE OF p, u)`, userID, string(p)).Scan(&ok)
	if err != nil {
		return errors.Join(ErrDenied, err)
	}
	if !ok {
		return ErrDenied
	}
	return nil
}

// RequireReviewer returns nil only if userID is assigned to review subjectID.
// Like Require, it is a fast early check outside any caller transaction;
// state-changing review actions must re-check with RequireReviewerTx.
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

// RequireReviewerTx checks inside tx that userID is assigned to review
// subjectID, share-locking the assignment and account rows until tx ends,
// like RequireTx.
func RequireReviewerTx(ctx context.Context, tx pgx.Tx, userID, subjectID int64) error {
	if userID == 0 {
		return ErrDenied
	}
	var ok bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM reviewer_assignments r JOIN users u ON u.id = r.user_id
			 WHERE r.user_id = $1 AND r.subject_id = $2 AND u.disabled_at IS NULL
			   FOR SHARE OF r, u)`, userID, subjectID).Scan(&ok)
	if err != nil {
		return errors.Join(ErrDenied, err)
	}
	if !ok {
		return ErrDenied
	}
	return nil
}

// Actor identifies who changes permissions. An operator is a local tool
// with direct database access (the LRN-013 CLI); a user actor must hold
// administrator at the time of the change.
type Actor struct {
	userID   int64
	operator bool
	label    string // operators only; see OperatorAs
}

// Operator is an unattributed operator actor, audited with no actor_id and
// the label UnattributedOperator. Operator tools must use OperatorAs instead
// so the audit records who ran them; Operator remains for tests and existing
// callers. Operators skip authorization entirely: anyone with local database
// access is already an operator (docs/DECISIONS.md "Operator trust
// boundary"). No operator actor may be reachable from HTTP code;
// internal/server tests enforce this.
var Operator = Actor{operator: true}

// UnattributedOperator is the audit label stored for Operator.
const UnattributedOperator = "operator (unattributed)"

// MaxOperatorLabel is the maximum operator label length in characters; the
// database enforces the same bound.
const MaxOperatorLabel = 200

// ErrInvalidOperatorLabel means an operator label is empty, too long or
// contains control characters.
var ErrInvalidOperatorLabel = errors.New("invalid operator label")

// OperatorAs returns an operator actor whose changes are audited with label
// (for example LocalOperatorLabel()) and no actor_id. The label is trimmed;
// it must be non-empty, at most MaxOperatorLabel characters, valid UTF-8 and
// free of control characters. It is self-asserted, not authenticated.
func OperatorAs(label string) (Actor, error) {
	label = strings.TrimSpace(label)
	if !validLabel(label) {
		return Actor{}, ErrInvalidOperatorLabel
	}
	return Actor{operator: true, label: label}, nil
}

func validLabel(label string) bool {
	if label == "" || !utf8.ValidString(label) || utf8.RuneCountInString(label) > MaxOperatorLabel {
		return false
	}
	return strings.IndexFunc(label, unicode.IsControl) < 0
}

// LocalOperatorLabel returns "<os user>@<hostname>" for the process running
// an operator tool, falling back to "unknown" for either part when it cannot
// be determined or would make an invalid label. It is a self-asserted label
// for the audit trail, not authentication: whoever has local database access
// is trusted as an operator regardless of the label.
func LocalOperatorLabel() string {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host := "unknown"
	if h, err := os.Hostname(); err == nil && h != "" {
		host = h
	}
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if !validLabel(s) || utf8.RuneCountInString(s) > MaxOperatorLabel/2-1 {
			return "unknown"
		}
		return s
	}
	return clean(name) + "@" + clean(host)
}

// auditLabel is the operator label stored in the audit, or nil for users.
func (a Actor) auditLabel() *string {
	if !a.operator {
		return nil
	}
	if a.label == "" {
		l := UnattributedOperator
		return &l
	}
	return &a.label
}

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
// An unknown target returns ErrUnknownUser.
//
// Permission changes by a user actor share-lock the actor's administrator
// grant. Two administrators changing each other's grants at the same time
// can deadlock; PostgreSQL then aborts one with SQLSTATE 40P01. Callers
// should report such an error (or retry the whole call); it never leaves a
// partial change.
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

// RevokeReviewer removes target's review assignment for subjectID. An
// unknown target or subject returns ErrUnknownUser or ErrUnknownSubject.
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

// authorize checks the actor inside the transaction. RequireTx's FOR SHARE
// blocks a concurrent revocation of the actor's administrator grant, or
// disablement of the actor's account, until commit.
func authorize(ctx context.Context, tx pgx.Tx, actor Actor) error {
	if actor.operator {
		return nil
	}
	return RequireTx(ctx, tx, actor.userID, Administrator)
}

func (s *Store) apply(ctx context.Context, tx pgx.Tx, actor Actor, target int64, action string, p Permission, subject *int64) (bool, error) {
	var actorID *int64
	if !actor.operator {
		actorID = &actor.userID
	}
	var tag pgconn.CommandTag
	var err error
	switch {
	case action == "grant" && subject == nil:
		tag, err = tx.Exec(ctx, `INSERT INTO user_permissions (user_id, permission, granted_at, granted_by)
			VALUES ($1, $2, now(), $3) ON CONFLICT DO NOTHING`, target, string(p), actorID)
	case action == "revoke" && subject == nil:
		tag, err = tx.Exec(ctx, `DELETE FROM user_permissions WHERE user_id = $1 AND permission = $2`, target, string(p))
	case action == "grant":
		tag, err = tx.Exec(ctx, `INSERT INTO reviewer_assignments (user_id, subject_id, granted_at, granted_by)
			VALUES ($1, $2, now(), $3) ON CONFLICT DO NOTHING`, target, *subject, actorID)
	default:
		tag, err = tx.Exec(ctx, `DELETE FROM reviewer_assignments WHERE user_id = $1 AND subject_id = $2`, target, *subject)
	}
	if err != nil {
		return false, mapFK(err)
	}
	if tag.RowsAffected() == 0 {
		if action == "revoke" {
			// Nothing to delete: distinguish an absent grant (a no-op) from
			// an unknown target or subject. Grants already fail on the
			// foreign keys.
			return false, checkExists(ctx, tx, target, subject)
		}
		return false, nil
	}
	// occurred_at defaults to now(), the transaction time, so it equals the
	// grant's granted_at.
	_, err = tx.Exec(ctx, `INSERT INTO permission_audit (actor_id, operator_label, target_user_id, action, permission, subject_id)
		VALUES ($1, $2, $3, $4, $5, $6)`, actorID, actor.auditLabel(), target, action, string(p), subject)
	if err != nil {
		return false, fmt.Errorf("audit permission change: %w", mapFK(err))
	}
	return true, nil
}

func checkExists(ctx context.Context, tx pgx.Tx, user int64, subject *int64) error {
	var userOK, subjectOK bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1),
		$2::bigint IS NULL OR EXISTS (SELECT 1 FROM subjects WHERE id = $2)`, user, subject).Scan(&userOK, &subjectOK)
	switch {
	case err != nil:
		return err
	case !userOK:
		return ErrUnknownUser
	case !subjectOK:
		return ErrUnknownSubject
	}
	return nil
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
	ActorID    *int64 // nil for operator changes
	// OperatorLabel identifies the operator when ActorID is nil; empty for
	// changes by a signed-in user.
	OperatorLabel string
	TargetID      int64
	Action        string
	Permission    Permission
	SubjectID     *int64
}

// Audit returns the permission history of target, oldest first.
func (s *Store) Audit(ctx context.Context, target int64) ([]AuditEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT occurred_at, actor_id, coalesce(operator_label, ''), target_user_id, action, permission, subject_id
		FROM permission_audit WHERE target_user_id = $1 ORDER BY id`, target)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		var p string
		err := r.Scan(&e.OccurredAt, &e.ActorID, &e.OperatorLabel, &e.TargetID, &e.Action, &p, &e.SubjectID)
		e.Permission = Permission(p)
		return e, err
	})
}
