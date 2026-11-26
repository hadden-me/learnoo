package invitations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/invitations"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Simulated email addresses, labels and placeholder hashes only.

const (
	label = "test-operator@test-host"
	// A syntactically valid placeholder; these tests never verify passwords.
	fakeHash = "$argon2id$v=19$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g"
)

func newStore(t *testing.T) (*pgxpool.Pool, *invitations.Store) {
	t.Helper()
	db := dbtest.Migrated(t)
	return db, invitations.NewStore(db, nil)
}

func count(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIssueStoresOnlyTheTokenHash(t *testing.T) {
	db, s := newStore(t)
	ctx := context.Background()
	token, inv, err := s.Issue(ctx, "  Student.Invited@Example.TEST ", label, invitations.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 43 || inv.Email != "student.invited@example.test" || inv.OperatorLabel != label {
		t.Fatalf("unexpected invitation %+v (token length %d)", inv, len(token))
	}
	if got := inv.ExpiresAt.Sub(inv.CreatedAt); got != invitations.DefaultTTL {
		t.Fatalf("lifetime %s", got)
	}
	// Nothing in the database contains the token.
	if n := count(t, db, `SELECT count(*) FROM student_invitations
		WHERE position(convert_to($1, 'UTF8') IN token_hash) > 0 OR email LIKE '%' || $1 || '%' OR operator_label LIKE '%' || $1 || '%'`, token); n != 0 {
		t.Fatal("token stored in plaintext")
	}
	if n := count(t, db, `SELECT count(*) FROM student_invitation_audit WHERE invitation_id = $1 AND action = 'issue' AND operator_label = $2 AND user_id IS NULL`, inv.ID, label); n != 1 {
		t.Fatalf("issue audit rows = %d", n)
	}
	if email, err := s.Pending(ctx, token); err != nil || email != inv.Email {
		t.Fatalf("Pending = %q, %v", email, err)
	}
	// Two invitations never share a token.
	other, _, err := s.Issue(ctx, "student.invited@example.test", label, time.Hour)
	if err != nil || other == token {
		t.Fatalf("second invitation: %v (same token %v)", err, other == token)
	}
}

func TestIssueValidation(t *testing.T) {
	db, s := newStore(t)
	ctx := context.Background()
	if _, err := users.Create(ctx, db, "existing@example.test", "Existing", fakeHash); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		email, label string
		ttl          time.Duration
		want         error
	}{
		"bad email":       {"not-an-email", label, time.Hour, users.ErrInvalidEmail},
		"existing user":   {"Existing@example.test", label, time.Hour, invitations.ErrEmailTaken},
		"empty label":     {"a@example.test", "  ", time.Hour, invitations.ErrInvalidLabel},
		"control label":   {"a@example.test", "op\n@host", time.Hour, invitations.ErrInvalidLabel},
		"rtl override":    {"a@example.test", "alice@host\u202Etsoh@eve", time.Hour, invitations.ErrInvalidLabel},
		"zero width":      {"a@example.test", "op\u200B@host", time.Hour, invitations.ErrInvalidLabel},
		"isolate":         {"a@example.test", "op\u2066@host", time.Hour, invitations.ErrInvalidLabel},
		"byte order mark": {"a@example.test", "op\uFEFF@host", time.Hour, invitations.ErrInvalidLabel},
		"line separator":  {"a@example.test", "op\u2028@host", time.Hour, invitations.ErrInvalidLabel},
		"long label":      {"a@example.test", strings.Repeat("x", 201), time.Hour, invitations.ErrInvalidLabel},
		"ttl too short":   {"a@example.test", label, time.Minute, invitations.ErrInvalidTTL},
		"ttl too long":    {"a@example.test", label, 31 * 24 * time.Hour, invitations.ErrInvalidTTL},
		"ttl is negative": {"a@example.test", label, -time.Hour, invitations.ErrInvalidTTL},
	} {
		if _, _, err := s.Issue(ctx, tc.email, tc.label, tc.ttl); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM student_invitations`); n != 0 {
		t.Fatalf("%d invitations stored by rejected calls", n)
	}
	if !strings.Contains(permissions.LocalOperatorLabel(), "@") {
		t.Fatalf("LocalOperatorLabel = %q", permissions.LocalOperatorLabel())
	}
	if _, _, err := s.Issue(ctx, "local@example.test", permissions.LocalOperatorLabel(), time.Hour); err != nil {
		t.Fatalf("local operator label rejected: %v", err)
	}
}

func TestAcceptCreatesStudentOnceAndAudits(t *testing.T) {
	db, s := newStore(t)
	ctx := context.Background()
	token, inv, err := s.Issue(ctx, "invited@example.test", label, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Accept(ctx, token, "  Simulated Student ", fakeHash)
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "invited@example.test" || u.DisplayName != "Simulated Student" {
		t.Fatalf("unexpected user %+v", u)
	}
	// A Student holds no permissions.
	if n := count(t, db, `SELECT (SELECT count(*) FROM user_permissions WHERE user_id = $1) + (SELECT count(*) FROM reviewer_assignments WHERE user_id = $1)`, u.ID); n != 0 {
		t.Fatalf("student has %d permissions", n)
	}
	if n := count(t, db, `SELECT count(*) FROM student_invitations WHERE id = $1 AND accepted_user_id = $2 AND accepted_at IS NOT NULL`, inv.ID, u.ID); n != 1 {
		t.Fatal("invitation not marked accepted")
	}
	if n := count(t, db, `SELECT count(*) FROM student_invitation_audit WHERE invitation_id = $1 AND action = 'accept' AND user_id = $2`, inv.ID, u.ID); n != 1 {
		t.Fatal("acceptance not audited")
	}
	// Reuse, lookup after use, and a second account are all refused.
	if _, err := s.Accept(ctx, token, "Another Name", fakeHash); !errors.Is(err, invitations.ErrInvalid) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := s.Pending(ctx, token); !errors.Is(err, invitations.ErrInvalid) {
		t.Fatalf("pending after use: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("%d users", n)
	}
}

func TestWrongMalformedAndExpiredTokensAreInvalid(t *testing.T) {
	db, s := newStore(t)
	ctx := context.Background()
	token, _, err := s.Issue(ctx, "invited@example.test", label, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Same length and alphabet, one character different.
	wrong := []byte(token)
	if wrong[0] == 'A' {
		wrong[0] = 'B'
	} else {
		wrong[0] = 'A'
	}
	// An old invitation: issued 2 days ago with a 1 day lifetime.
	past := invitations.NewStore(db, func() time.Time { return time.Now().Add(-48 * time.Hour) })
	expired, _, err := past.Issue(ctx, "late@example.test", label, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{
		"wrong":     string(wrong),
		"malformed": "not-a-token",
		"empty":     "",
		"too long":  token + "A",
		"expired":   expired,
	} {
		if _, err := s.Pending(ctx, tok); !errors.Is(err, invitations.ErrInvalid) {
			t.Errorf("%s pending: %v", name, err)
		}
		if _, err := s.Accept(ctx, tok, "Name", fakeHash); !errors.Is(err, invitations.ErrInvalid) {
			t.Errorf("%s accept: %v", name, err)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM users`); n != 0 {
		t.Fatalf("%d users created", n)
	}
	// The valid token still works afterwards.
	if _, err := s.Accept(ctx, token, "Name", fakeHash); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptRefusesInvalidNamesAndExistingAccounts(t *testing.T) {
	db, s := newStore(t)
	ctx := context.Background()
	token, _, err := s.Issue(ctx, "invited@example.test", label, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "   ", strings.Repeat("é", 101), "bad\x00name", "tab\tname",
		"spoof‮eman", "isolate⁧x", "line sep", string([]byte{0xff, 0xfe})} {
		if _, err := s.Accept(ctx, token, name, fakeHash); !errors.Is(err, invitations.ErrInvalidName) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	for _, name := range []string{"Amel", "أمل", "Élève ✓", strings.Repeat("é", 100)} {
		if _, err := invitations.NormalizeDisplayName(name); err != nil {
			t.Errorf("name %q rejected: %v", name, err)
		}
	}
	// An account created for the email after the invitation was issued
	// (for example with create-user) makes the invitation unusable.
	if _, err := users.Create(ctx, db, "invited@example.test", "Existing", fakeHash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, token, "Name", fakeHash); !errors.Is(err, invitations.ErrEmailTaken) {
		t.Fatalf("existing account: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM student_invitations WHERE accepted_at IS NOT NULL`); n != 0 {
		t.Fatal("invitation consumed by a failed acceptance")
	}
}

func TestCheckPassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"short":                                false,
		strings.Repeat("a", 11):                false,
		strings.Repeat("a", 12):                true,
		strings.Repeat("é", 12):                true,
		strings.Repeat("a", 1024):              true,
		strings.Repeat("a", 1025):              false,
		string([]byte{0xff}) + "aaaaaaaaaaaaa": false,
	} {
		if err := invitations.CheckPassword(pw); (err == nil) != ok {
			t.Errorf("password of %d bytes: err=%v, want ok=%v", len(pw), err, ok)
		}
	}
}

// Concurrent acceptances of one token create exactly one account. This is a
// smoke test: goroutine timing may not overlap; the deterministic lock check
// is TestAcceptWaitsForConcurrentAcceptance.
func TestConcurrentAcceptanceCreatesOneAccount(t *testing.T) {
	db, s := newStore(t)
	ctx := context.Background()
	token, _, err := s.Issue(ctx, "race@example.test", label, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = s.Accept(ctx, token, "Racer", fakeHash)
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, invitations.ErrInvalid):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d acceptances succeeded", ok)
	}
	if c := count(t, db, `SELECT count(*) FROM users WHERE email = 'race@example.test'`); c != 1 {
		t.Fatalf("%d accounts", c)
	}
	if c := count(t, db, `SELECT count(*) FROM student_invitation_audit WHERE action = 'accept'`); c != 1 {
		t.Fatalf("%d acceptance audit rows", c)
	}
}

func wantGuard(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != "LRN01" && pgErr.Code != "23514") {
		t.Errorf("%s: got %v, want a guard (LRN01) or check (23514) violation", what, err)
	}
}

// The database itself keeps invitations single-use and the audit append-only,
// whatever SQL the application role sends.
func TestDatabaseGuards(t *testing.T) {
	db, s := newStore(t)
	ctx := context.Background()
	_, inv, err := s.Issue(ctx, "guarded@example.test", label, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, err := users.Create(ctx, db, "other@example.test", "Other", fakeHash)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) error {
		_, err := db.Exec(ctx, sql, args...)
		return err
	}
	wantGuard(t, "insert pre-accepted", exec(`INSERT INTO student_invitations (token_hash, email, operator_label, created_at, expires_at, accepted_at, accepted_user_id)
		VALUES (sha256('x'), 'x@example.test', 'op', now(), now() + interval '1 hour', now(), $1)`, other.ID))
	wantGuard(t, "lifetime over 30 days", exec(`INSERT INTO student_invitations (token_hash, email, operator_label, created_at, expires_at)
		VALUES (sha256('y'), 'y@example.test', 'op', now(), now() + interval '31 days')`))
	wantGuard(t, "unnormalized email", exec(`INSERT INTO student_invitations (token_hash, email, operator_label, created_at, expires_at)
		VALUES (sha256('z'), 'Z@example.test', 'op', now(), now() + interval '1 hour')`))
	// The label constraint matches the operator audit tables (0002, 0003).
	insertLabel := func(n int, l string) error {
		return exec(`INSERT INTO student_invitations (token_hash, email, operator_label, created_at, expires_at)
			VALUES (sha256(convert_to($1, 'UTF8')), $1 || '@example.test', $2, now(), now() + interval '1 hour')`, fmt.Sprintf("label%d", n), l)
	}
	n := 0
	for name, l := range map[string]string{
		"control":             "op\nx",
		"c1 control":          "op\u0085x",
		"rtl override":        "alice@host\u202Etsoh@eve",
		"zero width space":    "op\u200Bx",
		"isolate":             "op\u2066x",
		"byte order mark":     "op\uFEFFx",
		"soft hyphen":         "op\u00ADx",
		"line separator":      "op\u2028x",
		"paragraph separator": "op\u2029x",
		"tag character":       "op\U000E0041x",
		"padded":              " op",
		"too long":            strings.Repeat("a", 201),
	} {
		n++
		if err := insertLabel(n, l); err == nil {
			t.Errorf("label %s accepted", name)
		}
	}
	for name, l := range map[string]string{"accented": "élève@hôte", "arabic": "علي@host", "max length": strings.Repeat("é", 200)} {
		n++
		if err := insertLabel(n, l); err != nil {
			t.Errorf("label %s rejected: %v", name, err)
		}
	}
	// A temporary table cannot shadow the users table the guard reads.
	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Release on every path: a connection still held when the test fails
	// would make the pool's Close in the cleanup wait forever.
	t.Cleanup(conn.Release)
	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE users (id bigint, email text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO pg_temp.users VALUES ($1, 'guarded@example.test')`, other.ID); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, `UPDATE student_invitations SET accepted_at = now(), accepted_user_id = $2 WHERE id = $1`, inv.ID, other.ID)
	wantGuard(t, "accept through a shadow users table", err)
	if _, err := conn.Exec(ctx, `DROP TABLE pg_temp.users`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	wantGuard(t, "accept by an account with another email", exec(`UPDATE student_invitations SET accepted_at = now(), accepted_user_id = $2 WHERE id = $1`, inv.ID, other.ID))
	wantGuard(t, "extend expiry", exec(`UPDATE student_invitations SET expires_at = expires_at + interval '1 day' WHERE id = $1`, inv.ID))
	wantGuard(t, "change email", exec(`UPDATE student_invitations SET email = 'other@example.test' WHERE id = $1`, inv.ID))
	wantGuard(t, "delete", exec(`DELETE FROM student_invitations WHERE id = $1`, inv.ID))
	wantGuard(t, "truncate", exec(`TRUNCATE student_invitations CASCADE`))
	wantGuard(t, "audit update", exec(`UPDATE student_invitation_audit SET operator_label = 'forged'`))
	wantGuard(t, "audit delete", exec(`DELETE FROM student_invitation_audit`))
	wantGuard(t, "audit truncate", exec(`TRUNCATE student_invitation_audit`))
	wantGuard(t, "backdated audit", exec(`INSERT INTO student_invitation_audit (invitation_id, action, operator_label, occurred_at) VALUES ($1, 'issue', 'op', now() - interval '1 day')`, inv.ID))

	// Backdated acceptance and acceptance after expiry are refused.
	matching, err := users.Create(ctx, db, "guarded@example.test", "Guarded", fakeHash)
	if err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "backdated acceptance", exec(`UPDATE student_invitations SET accepted_at = now() - interval '1 minute', accepted_user_id = $2 WHERE id = $1`, inv.ID, matching.ID))
	past := invitations.NewStore(db, func() time.Time { return time.Now().Add(-48 * time.Hour) })
	_, old, err := past.Issue(ctx, "old@example.test", label, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	oldUser, err := users.Create(ctx, db, "old@example.test", "Old", fakeHash)
	if err != nil {
		t.Fatal(err)
	}
	wantGuard(t, "accept after expiry", exec(`UPDATE student_invitations SET accepted_at = now(), accepted_user_id = $2 WHERE id = $1`, old.ID, oldUser.ID))
	// Direct acceptance by the matching account works once, then never again.
	if err := exec(`UPDATE student_invitations SET accepted_at = now(), accepted_user_id = $2 WHERE id = $1`, inv.ID, matching.ID); err != nil {
		t.Fatalf("valid direct acceptance: %v", err)
	}
	wantGuard(t, "second acceptance", exec(`UPDATE student_invitations SET accepted_at = now(), accepted_user_id = $2 WHERE id = $1`, inv.ID, matching.ID))
	wantGuard(t, "clear acceptance", exec(`UPDATE student_invitations SET accepted_at = NULL, accepted_user_id = NULL WHERE id = $1`, inv.ID))
	if n := count(t, db, `SELECT count(*) FROM student_invitation_audit WHERE invitation_id = $1 AND action = 'accept'`, inv.ID); n != 1 {
		t.Fatalf("accept audit rows = %d", n)
	}
}

// Deterministic version of the race: a first transaction accepts the
// invitation and holds its row lock; Accept must wait on that lock (seen in
// pg_stat_activity, blocked by the first transaction's backend) and, once
// the first commits, see the invitation as used. Without the row lock,
// Accept would not wait on the invitation and would end on the account's
// unique email instead.
func TestAcceptWaitsForConcurrentAcceptance(t *testing.T) {
	db, s := newStore(t)
	ctx := context.Background()
	token, inv, err := s.Issue(ctx, "locked@example.test", label, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var holder int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM student_invitations WHERE id = $1 FOR UPDATE`, inv.ID); err != nil {
		t.Fatal(err)
	}
	var first int64
	if err := tx.QueryRow(ctx, `INSERT INTO users (email, display_name, password_hash) VALUES ('locked@example.test', 'First', $1) RETURNING id`, fakeHash).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE student_invitations SET accepted_at = now(), accepted_user_id = $2 WHERE id = $1`, inv.ID, first); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.Accept(ctx, token, "Second", fakeHash)
		done <- err
	}()
	blocked := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var n int
		if err := db.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND $1 = ANY (pg_blocking_pids(pid))
			   AND query LIKE '%FROM student_invitations%FOR UPDATE%'`, holder).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			blocked = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Accept returned without waiting for the invitation lock: %v", err)
		default:
		}
	}
	if !blocked {
		t.Fatal("Accept never waited on the invitation row lock")
	}
	select {
	case err := <-done:
		t.Fatalf("Accept returned while the lock was held: %v", err)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, invitations.ErrInvalid) {
			t.Fatalf("waiting acceptance: got %v, want ErrInvalid", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Accept still blocked after commit")
	}
	if n := count(t, db, `SELECT count(*) FROM users WHERE email = 'locked@example.test'`); n != 1 {
		t.Fatalf("%d accounts", n)
	}
}
