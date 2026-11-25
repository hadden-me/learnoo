package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Simulated accounts and subjects only.
const cliPassword = "simulated-password-3\n"

type cli struct {
	t    *testing.T
	pool *pgxpool.Pool
	log  *slog.Logger
}

// newCLI points LEARNOO_DATABASE_URL at a fresh, migrated schema.
func newCLI(t *testing.T) *cli {
	pool := dbtest.EmptySchema(t)
	var schema string
	if err := pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	url := dbtest.URL(t)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	t.Setenv("LEARNOO_DATABASE_URL", url+sep+"search_path="+schema)
	c := &cli{t: t, pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.ok("", "migrate")
	return c
}

func (c *cli) run(stdin string, args ...string) (string, error) {
	var out bytes.Buffer
	err := run(args, strings.NewReader(stdin), &out, c.log)
	return out.String(), err
}

func (c *cli) ok(stdin string, args ...string) string {
	c.t.Helper()
	out, err := c.run(stdin, args...)
	if err != nil {
		c.t.Fatalf("%v: %v", args, err)
	}
	return out
}

func (c *cli) count(sql string) int {
	c.t.Helper()
	var n int
	if err := c.pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

// snapshot counts every table an operator command can change.
func (c *cli) snapshot() string {
	c.t.Helper()
	var parts []string
	for _, table := range []string{"users", "user_permissions", "reviewer_assignments", "permission_audit", "subjects", "sessions", "account_status_audit"} {
		parts = append(parts, fmt.Sprintf("%s=%d", table, c.count("SELECT count(*) FROM "+table)))
	}
	var disabled int
	_ = c.pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE disabled_at IS NOT NULL`).Scan(&disabled)
	return strings.Join(parts, " ") + fmt.Sprintf(" disabled=%d", disabled)
}

func (c *cli) subject(code string) {
	c.t.Helper()
	if _, err := c.pool.Exec(context.Background(), `INSERT INTO subjects (code) VALUES ($1)`, code); err != nil {
		c.t.Fatal(err)
	}
}

func wantOut(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Fatalf("output %q does not contain %q", out, want)
	}
}

func TestBootstrapAdminCommand(t *testing.T) {
	c := newCLI(t)
	out := c.ok(cliPassword, "bootstrap-admin", "-email", "Admin@Example.test", "-name", "Simulated Admin")
	wantOut(t, out, "created administrator")
	if got := c.snapshot(); got != "users=1 user_permissions=3 reviewer_assignments=0 permission_audit=3 subjects=0 sessions=0 account_status_audit=0 disabled=0" {
		t.Fatalf("after bootstrap: %s", got)
	}
	if n := c.count(`SELECT count(*) FROM permission_audit WHERE actor_id IS NULL AND action = 'grant' AND subject_id IS NULL`); n != 3 {
		t.Fatalf("%d operator grant audit rows, want 3", n)
	}
	if n := c.count(`SELECT count(*) FROM permission_audit WHERE operator_label = '` + permissions.LocalOperatorLabel() + `'`); n != 3 {
		t.Fatalf("%d audit rows carry the local operator label, want 3", n)
	}
	before := c.snapshot()
	_, err := c.run(cliPassword, "bootstrap-admin", "-email", "other@example.test", "-name", "Other")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second bootstrap: %v", err)
	}
	if c.snapshot() != before {
		t.Fatal("refused bootstrap changed the database")
	}
	out = c.ok("", "show-user", "-email", "admin@example.test")
	for _, want := range []string{"status: active", "global permissions: author, publisher, administrator", "reviewer subjects: (none)", "grant administrator by operator " + permissions.LocalOperatorLabel()} {
		wantOut(t, out, want)
	}
}

func TestConcurrentBootstrapAdminCommands(t *testing.T) {
	c := newCLI(t)
	const n = 4
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = c.run(cliPassword, "bootstrap-admin", "-email", fmt.Sprintf("admin%d@example.test", i), "-name", "Simulated Admin")
		}()
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || c.count(`SELECT count(*) FROM users`) != 1 || c.count(`SELECT count(*) FROM permission_audit`) != 3 {
		t.Fatalf("wins=%d %s", wins, c.snapshot())
	}
}

func TestGrantAndRevokeCommands(t *testing.T) {
	c := newCLI(t)
	c.ok(cliPassword, "bootstrap-admin", "-email", "admin@example.test", "-name", "Simulated Admin")
	c.ok(cliPassword, "create-user", "-email", "teacher@example.test", "-name", "Simulated Teacher")
	c.subject("simulated-math")
	c.subject("simulated-physics")
	audit := func() int { return c.count(`SELECT count(*) FROM permission_audit`) }

	steps := []struct {
		args []string
		want string
		rows int // audit rows added
	}{
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "author"}, "granted author to teacher@example.test", 1},
		{[]string{"grant", "-email", "TEACHER@example.test", "-permission", "author"}, "no change: teacher@example.test already holds author", 0},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "publisher"}, "granted publisher", 1},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "administrator"}, "granted administrator", 1},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "simulated-math"}, "granted reviewer for subject simulated-math", 1},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "simulated-math"}, "no change: teacher@example.test already holds reviewer for subject simulated-math", 0},
		{[]string{"revoke", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "simulated-physics"}, "no change: teacher@example.test does not hold reviewer for subject simulated-physics", 0},
		{[]string{"revoke", "-email", "teacher@example.test", "-permission", "publisher"}, "revoked publisher from teacher@example.test", 1},
		{[]string{"revoke", "-email", "teacher@example.test", "-permission", "publisher"}, "no change: teacher@example.test does not hold publisher", 0},
		{[]string{"revoke", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "simulated-math"}, "revoked reviewer for subject simulated-math", 1},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "simulated-physics"}, "granted reviewer for subject simulated-physics", 1},
		// Two administrators: the bootstrap one may lose administrator.
		{[]string{"revoke", "-email", "admin@example.test", "-permission", "administrator"}, "revoked administrator from admin@example.test", 1},
	}
	for _, s := range steps {
		before := audit()
		out := c.ok("", s.args...)
		wantOut(t, out, s.want)
		if got := audit() - before; got != s.rows {
			t.Fatalf("%v: %d audit rows added, want %d", s.args, got, s.rows)
		}
	}
	var actor *int64
	var action, perm, subject string
	err := c.pool.QueryRow(context.Background(), `SELECT a.actor_id, a.action, a.permission, coalesce(s.code, '')
		FROM permission_audit a LEFT JOIN subjects s ON s.id = a.subject_id
		JOIN users u ON u.id = a.target_user_id
		WHERE u.email = 'teacher@example.test' ORDER BY a.id DESC LIMIT 1`).Scan(&actor, &action, &perm, &subject)
	if err != nil || actor != nil || action != "grant" || perm != "reviewer" || subject != "simulated-physics" {
		t.Fatalf("last audit row: actor=%v %s %s %s %v", actor, action, perm, subject, err)
	}

	// The teacher is now the only active administrator.
	before := c.snapshot()
	_, err = c.run("", "revoke", "-email", "teacher@example.test", "-permission", "administrator")
	if err == nil || !strings.Contains(err.Error(), "last active administrator") {
		t.Fatalf("last administrator revoke: %v", err)
	}
	if c.snapshot() != before {
		t.Fatal("refused revoke changed the database")
	}

	// Every change above was made by the CLI: each audit row carries the
	// local operator label and no actor.
	label := permissions.LocalOperatorLabel()
	var total, labelled int
	err = c.pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE actor_id IS NULL AND operator_label = $1)
		FROM permission_audit`, label).Scan(&total, &labelled)
	if err != nil || total == 0 || labelled != total {
		t.Fatalf("%d of %d audit rows carry operator label %q (%v)", labelled, total, label, err)
	}

	out := c.ok("", "show-user", "-email", "teacher@example.test")
	for _, want := range []string{
		"global permissions: author, administrator\n",
		"reviewer subjects: simulated-physics\n",
		" grant reviewer subject=simulated-math by operator " + label + "\n",
		" revoke publisher by operator " + label + "\n",
	} {
		wantOut(t, out, want)
	}
	if n := strings.Count(out, " by operator "+label+"\n"); n != strings.Count(out, " by ") || n == 0 {
		t.Fatalf("show-user audit lines without the exact operator label:\n%s", out)
	}
}

func TestInvalidOperatorCommandsChangeNothing(t *testing.T) {
	c := newCLI(t)
	c.ok(cliPassword, "bootstrap-admin", "-email", "admin@example.test", "-name", "Simulated Admin")
	c.ok(cliPassword, "create-user", "-email", "teacher@example.test", "-name", "Simulated Teacher")
	c.subject("simulated-math")
	before := c.snapshot()
	for _, tc := range []struct {
		args  []string
		stdin string
		want  string
	}{
		{[]string{"grant", "-email", "nobody@example.test", "-permission", "author"}, "", "no account with email"},
		{[]string{"grant", "-email", "not-an-email", "-permission", "author"}, "", "invalid email"},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "missing"}, "", `unknown subject code "missing"`},
		{[]string{"revoke", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "missing"}, "", "unknown subject code"},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "superuser"}, "", "unknown permission"},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "Author"}, "", "unknown permission"},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "reviewer"}, "", "give -subject"},
		{[]string{"revoke", "-email", "teacher@example.test", "-permission", "reviewer"}, "", "give -subject"},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "author", "-subject", "simulated-math"}, "", "global permission"},
		{[]string{"revoke", "-email", "teacher@example.test", "-permission", "administrator", "-subject", "simulated-math"}, "", "global permission"},
		{[]string{"grant", "-email", "teacher@example.test"}, "", "needs -email and -permission"},
		{[]string{"grant", "-permission", "author"}, "", "needs -email and -permission"},
		{[]string{"grant", "-email", "teacher@example.test", "-permission", "author", "extra"}, "", "needs -email and -permission"},
		{[]string{"grant", "-bogus"}, "", "flag provided but not defined"},
		{[]string{"revoke", "-email", "admin@example.test", "-permission", "administrator"}, "", "last active administrator"},
		{[]string{"show-user", "-email", "nobody@example.test"}, "", "no account with email"},
		{[]string{"show-user"}, "", "needs -email"},
		{[]string{"disable-user"}, "", "disable-user needs -email"},
		{[]string{"enable-user", "-email", "teacher@example.test", "extra"}, "", "enable-user needs -email"},
		{[]string{"disable-user", "-bogus"}, "", "flag provided but not defined"},
		{[]string{"disable-user", "-email", "nobody@example.test"}, "", "no account with email"},
		{[]string{"enable-user", "-email", "nobody@example.test"}, "", "no account with email"},
		{[]string{"disable-user", "-email", "not-an-email"}, "", "invalid email"},
		{[]string{"disable-user", "-email", "admin@example.test"}, "", "last active administrator"},
		{[]string{"bootstrap-admin", "-email", "x@example.test"}, cliPassword, "needs -email and -name"},
		{[]string{"bootstrap-admin", "-email", "x@example.test", "-name", "X"}, "short\n", "at least 12"},
		{[]string{"bootstrap-admin", "-email", "x@example.test", "-name", "X"}, "", "at least 12"},
		{[]string{"bootstrap-admin", "-email", "x@example.test", "-name", "X"}, cliPassword, "already exists"},
		{[]string{"frobnicate"}, "", "unknown command"},
	} {
		_, err := c.run(tc.stdin, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want %q", tc.args, err, tc.want)
		}
		if got := c.snapshot(); got != before {
			t.Fatalf("%v changed the database: %s -> %s", tc.args, before, got)
		}
	}
}

func TestBootstrapAdminRefusesExistingEmail(t *testing.T) {
	c := newCLI(t)
	c.ok(cliPassword, "create-user", "-email", "existing@example.test", "-name", "Simulated Existing")
	before := c.snapshot()
	_, err := c.run(cliPassword, "bootstrap-admin", "-email", " Existing@Example.test", "-name", "Simulated Admin")
	if err == nil || !strings.Contains(err.Error(), "already registered") || !strings.Contains(err.Error(), "grant -permission administrator") {
		t.Fatalf("bootstrap with existing email: %v", err)
	}
	if got := c.snapshot(); got != before {
		t.Fatalf("refused bootstrap changed the database: %s -> %s", before, got)
	}
	// The documented alternative works with no administrator yet.
	for _, p := range []string{"administrator", "publisher", "author"} {
		c.ok("", "grant", "-email", "existing@example.test", "-permission", p)
	}
	wantOut(t, c.ok("", "show-user", "-email", "existing@example.test"), "global permissions: author, publisher, administrator")
}

func TestSubjectCodeIsNormalised(t *testing.T) {
	c := newCLI(t)
	c.ok(cliPassword, "create-user", "-email", "teacher@example.test", "-name", "Simulated Teacher")
	c.subject("simulated-math")
	wantOut(t, c.ok("", "grant", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "  Simulated-MATH "), "granted reviewer for subject simulated-math")
	wantOut(t, c.ok("", "grant", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "simulated-math"), "no change")
	wantOut(t, c.ok("", "revoke", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "SIMULATED-MATH"), "revoked reviewer for subject simulated-math")
	before := c.snapshot()
	for _, args := range [][]string{
		{"grant", "-email", "teacher@example.test", "-permission", "reviewer", "-subject", "   "},
		{"grant", "-email", "teacher@example.test", "-permission", "author", "-subject", "   "},
	} {
		if _, err := c.run("", args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if c.snapshot() != before {
		t.Fatal("rejected subject changed the database")
	}
}

func TestOperatorCommandsRefusePendingMigrations(t *testing.T) {
	pool := dbtest.EmptySchema(t)
	var schema string
	if err := pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	url := dbtest.URL(t)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	t.Setenv("LEARNOO_DATABASE_URL", url+sep+"search_path="+schema)
	c := &cli{t: t, pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, tc := range []struct {
		stdin string
		args  []string
	}{
		{"", []string{"grant", "-email", "a@example.test", "-permission", "author"}},
		{"", []string{"revoke", "-email", "a@example.test", "-permission", "author"}},
		{"", []string{"show-user", "-email", "a@example.test"}},
		{"", []string{"disable-user", "-email", "a@example.test"}},
		{"", []string{"enable-user", "-email", "a@example.test"}},
		{cliPassword, []string{"bootstrap-admin", "-email", "a@example.test", "-name", "A"}},
	} {
		if _, err := c.run(tc.stdin, tc.args...); err == nil || !strings.Contains(err.Error(), "pending migrations") {
			t.Errorf("%v: %v", tc.args, err)
		}
	}
	if n := c.count(`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name <> 'schema_migrations'`); n != 0 {
		t.Fatalf("%d tables created", n)
	}
}

// A disabled administrator is not active: it neither blocks bootstrap-admin
// nor lets the last active administrator lose administrator. (Accounts are
// disabled directly in the database here: disable-user refuses to disable
// the last active administrator.)
func TestDisabledAdministratorIsNotActive(t *testing.T) {
	c := newCLI(t)
	c.ok(cliPassword, "bootstrap-admin", "-email", "old@example.test", "-name", "Simulated Old Admin")
	if _, err := c.pool.Exec(context.Background(), `UPDATE users SET disabled_at = now() WHERE email = 'old@example.test'`); err != nil {
		t.Fatal(err)
	}
	wantOut(t, c.ok("", "show-user", "-email", "old@example.test"), "status: disabled (holds no effective permissions)")
	c.ok(cliPassword, "bootstrap-admin", "-email", "new@example.test", "-name", "Simulated New Admin")
	before := c.snapshot()
	if _, err := c.run("", "revoke", "-email", "new@example.test", "-permission", "administrator"); err == nil || !strings.Contains(err.Error(), "last active administrator") {
		t.Fatalf("last active administrator revoke with the other disabled: %v", err)
	}
	if c.snapshot() != before {
		t.Fatal("refused revoke changed the database")
	}
	wantOut(t, c.ok("", "revoke", "-email", "old@example.test", "-permission", "administrator"), "revoked administrator from old@example.test")
}

func TestDisableAndEnableUserCommands(t *testing.T) {
	c := newCLI(t)
	c.ok(cliPassword, "bootstrap-admin", "-email", "admin@example.test", "-name", "Simulated Admin")
	c.ok(cliPassword, "create-user", "-email", "teacher@example.test", "-name", "Simulated Teacher")
	ctx := context.Background()
	// Two simulated sessions for the teacher, one for the administrator.
	_, err := c.pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, csrf_token, created_at, last_seen_at, idle_expires_at, absolute_expires_at)
		SELECT sha256(convert_to(u.email || n, 'UTF8')), u.id, 'csrf', now(), now(), now() + interval '1 hour', now() + interval '1 day'
		  FROM users u CROSS JOIN generate_series(1, 2) n
		 WHERE u.email = 'teacher@example.test' OR (u.email = 'admin@example.test' AND n = 1)`)
	if err != nil {
		t.Fatal(err)
	}
	label := permissions.LocalOperatorLabel()

	wantOut(t, c.ok("", "disable-user", "-email", "teacher@example.test"), "disabled teacher@example.test and revoked its sessions")
	if n := c.count(`SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.email = 'teacher@example.test'`); n != 0 {
		t.Fatalf("%d teacher sessions left", n)
	}
	if n := c.count(`SELECT count(*) FROM sessions`); n != 1 {
		t.Fatalf("administrator session revoked too: %d left", n)
	}
	wantOut(t, c.ok("", "disable-user", "-email", "teacher@example.test"), "no change: teacher@example.test is already disabled")
	out := c.ok("", "show-user", "-email", "teacher@example.test")
	wantOut(t, out, "status: disabled")
	wantOut(t, out, "account status audit:\n")
	wantOut(t, out, " disable by operator "+label+"\n")

	// Enabling also purges any session the disabled account holds.
	if _, err := c.pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, csrf_token, created_at, last_seen_at, idle_expires_at, absolute_expires_at)
		SELECT sha256('leftover'), id, 'csrf', now(), now(), now() + interval '1 hour', now() + interval '1 day'
		  FROM users WHERE email = 'teacher@example.test'`); err != nil {
		t.Fatal(err)
	}
	wantOut(t, c.ok("", "enable-user", "-email", "teacher@example.test"), "enabled teacher@example.test")
	if n := c.count(`SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.email = 'teacher@example.test'`); n != 0 {
		t.Fatalf("enable kept %d teacher sessions", n)
	}
	wantOut(t, c.ok("", "enable-user", "-email", "teacher@example.test"), "no change: teacher@example.test is already enabled")
	if n := c.count(`SELECT count(*) FROM account_status_audit WHERE actor_id IS NULL AND operator_label = '` + label + `'`); n != 2 {
		t.Fatalf("%d status audit rows with the exact local operator label, want 2", n)
	}
	if n := c.count(`SELECT count(*) FROM account_status_audit`); n != 2 {
		t.Fatalf("%d status audit rows, want 2", n)
	}
	out = c.ok("", "show-user", "-email", "teacher@example.test")
	wantOut(t, out, "status: active")
	lines := strings.Split(out, "\n")
	var statusLines []string
	for i, l := range lines {
		if l == "account status audit:" {
			statusLines = lines[i+1:]
		}
	}
	if len(statusLines) < 2 || !strings.HasSuffix(statusLines[0], " disable by operator "+label) ||
		!strings.HasSuffix(statusLines[1], " enable by operator "+label) {
		t.Fatalf("status audit lines without the exact operator label:\n%s", out)
	}

	// With a second administrator the first may be disabled; the second
	// then may not, by disable or by revoke.
	c.ok("", "grant", "-email", "teacher@example.test", "-permission", "administrator")
	wantOut(t, c.ok("", "disable-user", "-email", "admin@example.test"), "disabled admin@example.test")
	before := c.snapshot()
	if _, err := c.run("", "disable-user", "-email", "teacher@example.test"); err == nil || !strings.Contains(err.Error(), "last active administrator") {
		t.Fatalf("last administrator disable: %v", err)
	}
	if _, err := c.run("", "revoke", "-email", "teacher@example.test", "-permission", "administrator"); err == nil || !strings.Contains(err.Error(), "last active administrator") {
		t.Fatalf("last administrator revoke with the other disabled: %v", err)
	}
	if c.snapshot() != before {
		t.Fatal("refused changes changed the database")
	}
	wantOut(t, c.ok("", "show-user", "-email", "admin@example.test"), "status: disabled (holds no effective permissions)")
}
