package invitations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/invitations"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LMS-B10: administrators issue invitations through IssueBy; the issuer is
// the administrator's actor_id, never an operator label (migrations/0016).

func account(t *testing.T, db *pgxpool.Pool, name string, perms ...permissions.Permission) int64 {
	t.Helper()
	ctx := context.Background()
	u, err := users.Create(ctx, db, name+"@example.test", name, fakeHash)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := permissions.OperatorAs(label)
	for _, p := range perms {
		if _, err := permissions.NewStore(db).Grant(ctx, op, u.ID, p); err != nil {
			t.Fatal(err)
		}
	}
	return u.ID
}

func TestAdministratorIssuesInvitationAudited(t *testing.T) {
	db, s := newStore(t)
	ctx := context.Background()
	admin := account(t, db, "test-admin", permissions.Administrator)
	author := account(t, db, "test-author", permissions.Author)
	off := account(t, db, "test-admin-off", permissions.Administrator)
	if _, err := db.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, off); err != nil {
		t.Fatal(err)
	}

	token, inv, err := s.IssueBy(ctx, admin, "New.Student@Example.TEST", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if inv.IssuedBy == nil || *inv.IssuedBy != admin || inv.OperatorLabel != "" || inv.Email != "new.student@example.test" {
		t.Fatalf("invitation %+v", inv)
	}
	if n := count(t, db, `SELECT count(*) FROM student_invitations WHERE id = $1 AND issued_by = $2 AND operator_label IS NULL`, inv.ID, admin); n != 1 {
		t.Fatalf("stored issuer rows = %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM student_invitation_audit
		WHERE invitation_id = $1 AND action = 'issue' AND actor_id = $2 AND operator_label IS NULL AND user_id IS NULL`, inv.ID, admin); n != 1 {
		t.Fatalf("issue audit rows = %d", n)
	}
	// The invitation is accepted like an operator's.
	if _, err := s.Accept(ctx, token, "TEST Élève", fakeHash); err != nil {
		t.Fatal(err)
	}

	for name, actor := range map[string]int64{"author": author, "disabled administrator": off, "unknown": 999999} {
		if _, _, err := s.IssueBy(ctx, actor, "other@example.test", time.Hour); !errors.Is(err, permissions.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM student_invitations WHERE email = 'other@example.test'`); n != 0 {
		t.Fatalf("denied issue stored %d invitations", n)
	}
	if _, _, err := s.IssueBy(ctx, admin, "test-author@example.test", time.Hour); !errors.Is(err, invitations.ErrEmailTaken) {
		t.Errorf("existing account: %v", err)
	}

	// The issuer is immutable, and exactly one of the two is set.
	wantGuard(t, "change issuer", func() error {
		_, err := db.Exec(ctx, `UPDATE student_invitations SET issued_by = $2 WHERE id = $1`, inv.ID, author)
		return err
	}())
	for name, sql := range map[string]string{
		"both":    `INSERT INTO student_invitations (token_hash, email, operator_label, issued_by, created_at, expires_at) VALUES (sha256('a'), 'x@example.test', 'x@y', $1, now(), now() + interval '1 hour')`,
		"neither": `INSERT INTO student_invitations (token_hash, email, created_at, expires_at) VALUES (sha256('b'), 'y@example.test', now(), now() + interval '1 hour') RETURNING $1::bigint`,
	} {
		_, err := db.Exec(ctx, sql, admin)
		if err == nil {
			t.Errorf("%s issuers accepted", name)
		}
	}
}
