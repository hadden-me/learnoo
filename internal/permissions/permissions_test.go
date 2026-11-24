package permissions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Simulated users and subjects only; no curriculum or educational claims.
var testArgon2 = auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}

const testPassword = "simulated-password-1"

type env struct {
	t     *testing.T
	db    *pgxpool.Pool
	store *permissions.Store
	ctx   context.Context
}

func newEnv(t *testing.T) *env {
	db := dbtest.Migrated(t)
	return &env{t: t, db: db, store: permissions.NewStore(db, nil), ctx: context.Background()}
}

func (e *env) user(name string) int64 {
	e.t.Helper()
	hash, err := auth.HashPassword(testPassword, testArgon2)
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := users.Create(e.ctx, e.db, name+"@example.test", name, hash)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.ID
}

func (e *env) subject(code string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(e.ctx, `INSERT INTO subjects (code) VALUES ($1) RETURNING id`, code).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) userOfType(name string, t permissions.AccountType) int64 {
	e.t.Helper()
	id := e.user(name)
	if err := e.store.GrantDefaults(e.ctx, permissions.Operator, id, t); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) set(id int64) permissions.Set {
	e.t.Helper()
	s, err := e.store.Load(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

var allGlobal = []permissions.Permission{permissions.Author, permissions.Publisher, permissions.Administrator}

func TestDenyByDefault(t *testing.T) {
	e := newEnv(t)
	u := e.user("nobody")
	math := e.subject("simulated-math")
	for _, p := range append(allGlobal, permissions.Reviewer, permissions.Permission("superuser"), "") {
		if err := e.store.Require(e.ctx, u, p); !errors.Is(err, permissions.ErrDenied) {
			t.Errorf("%q: want denied, got %v", p, err)
		}
	}
	if err := e.store.RequireReviewer(e.ctx, u, math); !errors.Is(err, permissions.ErrDenied) {
		t.Errorf("reviewer: want denied, got %v", err)
	}
	// Unknown users are denied, not errors that callers might ignore.
	if err := e.store.Require(e.ctx, 999999, permissions.Author); !errors.Is(err, permissions.ErrDenied) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestAccountTypeDefaults(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")
	for _, tc := range []struct {
		typ  permissions.AccountType
		want []permissions.Permission
	}{
		{permissions.Student, nil},
		{permissions.Teacher, []permissions.Permission{permissions.Author}},
		{permissions.Admin, []permissions.Permission{permissions.Author, permissions.Publisher, permissions.Administrator}},
	} {
		id := e.userOfType(string(tc.typ), tc.typ)
		s := e.set(id)
		if got := s.Global(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.typ, got, tc.want)
		}
		if s.AccountType() != tc.typ {
			t.Errorf("%s: presented as %s", tc.typ, s.AccountType())
		}
		// No default includes reviewing, for any subject.
		if s.CanReview(math) || len(s.ReviewerSubjects()) != 0 {
			t.Errorf("%s: default grants include reviewer", tc.typ)
		}
	}
	if _, err := permissions.DefaultPermissions("owner"); err == nil {
		t.Error("unknown account type accepted")
	}
}

func TestReviewerIsScopedPerSubject(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")
	physics := e.subject("simulated-physics")
	teacher := e.userOfType("teacher", permissions.Teacher)
	if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, teacher, math); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RequireReviewer(e.ctx, teacher, math); err != nil {
		t.Fatalf("assigned subject denied: %v", err)
	}
	if err := e.store.RequireReviewer(e.ctx, teacher, physics); !errors.Is(err, permissions.ErrDenied) {
		t.Fatalf("other subject allowed: %v", err)
	}
	if err := e.store.RequireReviewer(e.ctx, teacher, 999999); !errors.Is(err, permissions.ErrDenied) {
		t.Fatalf("unknown subject allowed: %v", err)
	}
	// Reviewer is never a global permission.
	if err := e.store.Require(e.ctx, teacher, permissions.Reviewer); !errors.Is(err, permissions.ErrDenied) {
		t.Fatalf("Require(reviewer) without subject allowed: %v", err)
	}
	// Revocation takes effect immediately.
	if _, err := e.store.RevokeReviewer(e.ctx, permissions.Operator, teacher, math); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RequireReviewer(e.ctx, teacher, math); !errors.Is(err, permissions.ErrDenied) {
		t.Fatalf("revoked reviewer still allowed: %v", err)
	}
}

func TestNoPermissionImpliesAnother(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")

	reviewer := e.user("reviewer")
	if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, reviewer, math); err != nil {
		t.Fatal(err)
	}
	for _, p := range allGlobal {
		if err := e.store.Require(e.ctx, reviewer, p); !errors.Is(err, permissions.ErrDenied) {
			t.Errorf("reviewer implied %s", p)
		}
	}

	publisher := e.user("publisher")
	if _, err := e.store.Grant(e.ctx, permissions.Operator, publisher, permissions.Publisher); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RequireReviewer(e.ctx, publisher, math); !errors.Is(err, permissions.ErrDenied) {
		t.Error("publisher implied reviewer")
	}
	if err := e.store.Require(e.ctx, publisher, permissions.Author); !errors.Is(err, permissions.ErrDenied) {
		t.Error("publisher implied author")
	}

	// Administrators need a subject assignment to review, like anyone else.
	admin := e.userOfType("admin", permissions.Admin)
	if err := e.store.RequireReviewer(e.ctx, admin, math); !errors.Is(err, permissions.ErrDenied) {
		t.Error("administrator implied reviewer")
	}

	// A teacher with a reviewer assignment still cannot publish.
	teacher := e.userOfType("teacher", permissions.Teacher)
	if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, teacher, math); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Require(e.ctx, teacher, permissions.Publisher); !errors.Is(err, permissions.ErrDenied) {
		t.Error("teacher+reviewer implied publisher")
	}
}

func TestDisabledAccountsHaveNoPermissions(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")
	admin := e.userOfType("admin", permissions.Admin)
	if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, admin, math); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, admin); err != nil {
		t.Fatal(err)
	}
	for _, p := range allGlobal {
		if err := e.store.Require(e.ctx, admin, p); !errors.Is(err, permissions.ErrDenied) {
			t.Errorf("disabled account kept %s", p)
		}
	}
	if err := e.store.RequireReviewer(e.ctx, admin, math); !errors.Is(err, permissions.ErrDenied) {
		t.Error("disabled account kept reviewer")
	}
	// ... and cannot grant.
	other := e.user("other")
	if _, err := e.store.Grant(e.ctx, permissions.UserActor(admin), other, permissions.Author); !errors.Is(err, permissions.ErrDenied) {
		t.Errorf("disabled administrator granted: %v", err)
	}
}

func TestOnlyAdministratorsChangePermissions(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")
	admin := e.userOfType("admin", permissions.Admin)
	teacher := e.userOfType("teacher", permissions.Teacher)
	publisher := e.user("publisher")
	if _, err := e.store.Grant(e.ctx, permissions.Operator, publisher, permissions.Publisher); err != nil {
		t.Fatal(err)
	}
	student := e.user("student")

	// Non-administrators cannot grant to others or to themselves.
	for name, actor := range map[string]int64{"teacher": teacher, "publisher": publisher, "student": student} {
		for _, target := range []int64{student, actor} {
			if _, err := e.store.Grant(e.ctx, permissions.UserActor(actor), target, permissions.Publisher); !errors.Is(err, permissions.ErrDenied) {
				t.Errorf("%s granted publisher to %d: %v", name, target, err)
			}
			if _, err := e.store.GrantReviewer(e.ctx, permissions.UserActor(actor), target, math); !errors.Is(err, permissions.ErrDenied) {
				t.Errorf("%s granted reviewer to %d: %v", name, target, err)
			}
		}
		if _, err := e.store.Revoke(e.ctx, permissions.UserActor(actor), admin, permissions.Administrator); !errors.Is(err, permissions.ErrDenied) {
			t.Errorf("%s revoked an administrator: %v", name, err)
		}
		if err := e.store.GrantDefaults(e.ctx, permissions.UserActor(actor), student, permissions.Admin); !errors.Is(err, permissions.ErrDenied) {
			t.Errorf("%s applied admin defaults: %v", name, err)
		}
	}
	if _, err := e.store.Grant(e.ctx, permissions.UserActor(0), student, permissions.Author); !errors.Is(err, permissions.ErrDenied) {
		t.Errorf("zero actor granted: %v", err)
	}
	if got := e.set(student).Global(); len(got) != 0 {
		t.Fatalf("denied grants left permissions behind: %v", got)
	}

	// Administrators can.
	if _, err := e.store.Grant(e.ctx, permissions.UserActor(admin), student, permissions.Author); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GrantReviewer(e.ctx, permissions.UserActor(admin), teacher, math); err != nil {
		t.Fatal(err)
	}
	// Reviewer must go through the subject-scoped API.
	if _, err := e.store.Grant(e.ctx, permissions.UserActor(admin), teacher, permissions.Reviewer); err == nil {
		t.Fatal("global reviewer grant accepted")
	}
}

func TestGrantsAndRevokesAreAudited(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")
	admin := e.userOfType("admin", permissions.Admin)
	target := e.user("target")
	actor := permissions.UserActor(admin)

	steps := []func() (bool, error){
		func() (bool, error) { return e.store.Grant(e.ctx, actor, target, permissions.Publisher) },
		func() (bool, error) { return e.store.Grant(e.ctx, actor, target, permissions.Publisher) }, // no-op
		func() (bool, error) { return e.store.GrantReviewer(e.ctx, actor, target, math) },
		func() (bool, error) { return e.store.RevokeReviewer(e.ctx, actor, target, math) },
		func() (bool, error) { return e.store.RevokeReviewer(e.ctx, actor, target, math) }, // no-op
		func() (bool, error) { return e.store.Revoke(e.ctx, actor, target, permissions.Publisher) },
	}
	wantChanged := []bool{true, false, true, true, false, true}
	for i, step := range steps {
		changed, err := step()
		if err != nil || changed != wantChanged[i] {
			t.Fatalf("step %d: changed=%v err=%v", i, changed, err)
		}
	}
	log, err := e.store.Audit(e.ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		action  string
		perm    permissions.Permission
		subject bool
	}
	var got []row
	for _, a := range log {
		if a.ActorID == nil || *a.ActorID != admin || a.TargetID != target || a.OccurredAt.IsZero() {
			t.Fatalf("bad audit actor/target/time: %+v", a)
		}
		got = append(got, row{a.Action, a.Permission, a.SubjectID != nil && *a.SubjectID == math})
	}
	want := []row{
		{"grant", permissions.Publisher, false},
		{"grant", permissions.Reviewer, true},
		{"revoke", permissions.Reviewer, true},
		{"revoke", permissions.Publisher, false},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("audit = %+v, want %+v", got, want)
	}

	// Operator changes are audited with no actor.
	if _, err := e.store.Grant(e.ctx, permissions.Operator, target, permissions.Author); err != nil {
		t.Fatal(err)
	}
	log, _ = e.store.Audit(e.ctx, target)
	if last := log[len(log)-1]; last.ActorID != nil || last.Permission != permissions.Author {
		t.Fatalf("operator audit row: %+v", last)
	}
	// Defaults are audited one row per permission.
	adminLog, _ := e.store.Audit(e.ctx, admin)
	if len(adminLog) != 3 {
		t.Fatalf("admin defaults audited %d rows, want 3", len(adminLog))
	}
}

func TestAuditIsAppendOnly(t *testing.T) {
	e := newEnv(t)
	e.userOfType("admin", permissions.Admin)
	for _, sql := range []string{
		`UPDATE permission_audit SET action = 'revoke'`,
		`DELETE FROM permission_audit`,
		`TRUNCATE permission_audit`,
	} {
		if _, err := e.db.Exec(e.ctx, sql); err == nil {
			t.Errorf("%s succeeded", sql)
		}
	}
}

func TestSchemaConstraints(t *testing.T) {
	e := newEnv(t)
	u := e.user("u")
	math := e.subject("simulated-math")
	for name, sql := range map[string]string{
		"unknown permission":    `INSERT INTO user_permissions (user_id, permission, granted_at) VALUES ($1, 'superuser', now())`,
		"reviewer as global":    `INSERT INTO user_permissions (user_id, permission, granted_at) VALUES ($1, 'reviewer', now())`,
		"unscoped review audit": `INSERT INTO permission_audit (occurred_at, target_user_id, action, permission) VALUES (now(), $1, 'grant', 'reviewer')`,
		"scoped author audit":   `INSERT INTO permission_audit (occurred_at, target_user_id, action, permission, subject_id) VALUES (now(), $1, 'grant', 'author', ` + strconv.FormatInt(math, 10) + `)`,
	} {
		if _, err := e.db.Exec(e.ctx, sql, u); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := e.db.Exec(e.ctx, `INSERT INTO subjects (code) VALUES ('simulated-math')`); err == nil {
		t.Error("duplicate subject code accepted")
	}
	if _, err := e.db.Exec(e.ctx, `INSERT INTO subjects (code) VALUES ('Bad Code')`); err == nil {
		t.Error("malformed subject code accepted")
	}
	if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, u, 999999); !errors.Is(err, permissions.ErrUnknownSubject) {
		t.Errorf("unknown subject: %v", err)
	}
	if _, err := e.store.Grant(e.ctx, permissions.Operator, 999999, permissions.Author); !errors.Is(err, permissions.ErrUnknownUser) {
		t.Errorf("unknown user: %v", err)
	}
	// A subject with reviewers cannot be deleted out from under them.
	if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, u, math); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(e.ctx, `DELETE FROM subjects WHERE id = $1`, math); err == nil {
		t.Error("subject with reviewer assignments deleted")
	}
}

// HTTP layer: the middleware is the server-side check future routes use.
func TestMiddlewareDeniesByDefault(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")
	physics := e.subject("simulated-physics")
	e.userOfType("student", permissions.Student)
	e.userOfType("teacher", permissions.Teacher)
	reviewer := e.userOfType("reviewer", permissions.Teacher)
	if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, reviewer, math); err != nil {
		t.Fatal(err)
	}
	e.userOfType("admin", permissions.Admin)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := auth.DefaultConfig
	cfg.Argon2 = testArgon2
	svc, err := auth.New(e.db, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ah := auth.NewHandlers(svc, log)
	pm := permissions.NewMiddleware(e.store, log)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	subjectFromPath := func(r *http.Request) (int64, error) { return strconv.ParseInt(r.PathValue("subject"), 10, 64) }
	mux := http.NewServeMux()
	ah.Register(mux)
	mux.Handle("GET /t/author", ah.Middleware(pm.Require(permissions.Author, ok)))
	mux.Handle("GET /t/publisher", ah.Middleware(pm.Require(permissions.Publisher, ok)))
	mux.Handle("GET /t/admin", ah.Middleware(pm.Require(permissions.Administrator, ok)))
	mux.Handle("GET /t/review/{subject}", ah.Middleware(pm.RequireReviewer(subjectFromPath, ok)))

	login := func(name string) string {
		b, _ := json.Marshal(map[string]string{"email": name + "@example.test", "password": testPassword})
		r := httptest.NewRequest(http.MethodPost, "http://learnoo.test/api/auth/login", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("login %s: %d %s", name, w.Code, w.Body)
		}
		return w.Result().Cookies()[0].Value
	}
	get := func(path, token string) int {
		r := httptest.NewRequest(http.MethodGet, "http://learnoo.test"+path, nil)
		if token != "" {
			r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	tokens := map[string]string{"": ""}
	for _, n := range []string{"student", "teacher", "reviewer", "admin"} {
		tokens[n] = login(n)
	}
	m, p := strconv.FormatInt(math, 10), strconv.FormatInt(physics, 10)
	cases := []struct {
		user, path string
		want       int
	}{
		{"", "/t/author", 401}, {"", "/t/review/" + m, 401},
		{"student", "/t/author", 403}, {"student", "/t/publisher", 403}, {"student", "/t/admin", 403}, {"student", "/t/review/" + m, 403},
		{"teacher", "/t/author", 200}, {"teacher", "/t/publisher", 403}, {"teacher", "/t/admin", 403}, {"teacher", "/t/review/" + m, 403},
		{"reviewer", "/t/review/" + m, 200}, {"reviewer", "/t/review/" + p, 403}, {"reviewer", "/t/publisher", 403}, {"reviewer", "/t/review/not-a-number", 403},
		{"admin", "/t/author", 200}, {"admin", "/t/publisher", 200}, {"admin", "/t/admin", 200}, {"admin", "/t/review/" + m, 403},
	}
	for _, c := range cases {
		if got := get(c.path, tokens[c.user]); got != c.want {
			t.Errorf("%q GET %s = %d, want %d", c.user, c.path, got, c.want)
		}
	}

	// Revocation applies to the next request in an existing session.
	if _, err := e.store.Revoke(e.ctx, permissions.Operator, reviewer, permissions.Author); err != nil {
		t.Fatal(err)
	}
	if got := get("/t/author", tokens["reviewer"]); got != 403 {
		t.Errorf("revoked author still allowed: %d", got)
	}
}

// blocked runs f in a goroutine and reports whether it is still running after
// a short wait; the returned channel yields f's error once it finishes.
func blocked(f func() error) (bool, <-chan error) {
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		done <- err
		return false, done
	case <-time.After(300 * time.Millisecond):
		return true, done
	}
}

func wait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent change still blocked after commit")
		return nil
	}
}

// A check inside the caller's transaction holds the grant until that
// transaction ends: a concurrent revocation or account disablement waits,
// and the next transaction sees it.
func TestTxChecksHoldGrantsUntilCommit(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")

	cases := []struct {
		name   string
		check  func(pgx.Tx, int64) error
		grant  func(int64)
		remove func(int64) error
	}{
		{
			name:  "revoke publisher",
			check: func(tx pgx.Tx, u int64) error { return permissions.RequireTx(e.ctx, tx, u, permissions.Publisher) },
			grant: func(u int64) {
				if _, err := e.store.Grant(e.ctx, permissions.Operator, u, permissions.Publisher); err != nil {
					t.Fatal(err)
				}
			},
			remove: func(u int64) error {
				_, err := e.store.Revoke(e.ctx, permissions.Operator, u, permissions.Publisher)
				return err
			},
		},
		{
			name:  "revoke reviewer",
			check: func(tx pgx.Tx, u int64) error { return permissions.RequireReviewerTx(e.ctx, tx, u, math) },
			grant: func(u int64) {
				if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, u, math); err != nil {
					t.Fatal(err)
				}
			},
			remove: func(u int64) error {
				_, err := e.store.RevokeReviewer(e.ctx, permissions.Operator, u, math)
				return err
			},
		},
		{
			name:  "disable account (publisher)",
			check: func(tx pgx.Tx, u int64) error { return permissions.RequireTx(e.ctx, tx, u, permissions.Publisher) },
			grant: func(u int64) {
				if _, err := e.store.Grant(e.ctx, permissions.Operator, u, permissions.Publisher); err != nil {
					t.Fatal(err)
				}
			},
			remove: func(u int64) error {
				_, err := e.db.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, u)
				return err
			},
		},
		{
			name:  "disable account (reviewer)",
			check: func(tx pgx.Tx, u int64) error { return permissions.RequireReviewerTx(e.ctx, tx, u, math) },
			grant: func(u int64) {
				if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, u, math); err != nil {
					t.Fatal(err)
				}
			},
			remove: func(u int64) error {
				_, err := e.db.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, u)
				return err
			},
		},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := e.user("holder" + strconv.Itoa(i))
			c.grant(u)

			tx, err := e.db.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(e.ctx)
			if err := c.check(tx, u); err != nil {
				t.Fatalf("check in tx A: %v", err)
			}
			isBlocked, done := blocked(func() error { return c.remove(u) })
			if !isBlocked {
				t.Fatalf("change did not wait for tx A (err=%v)", <-done)
			}
			if err := tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			if err := wait(t, done); err != nil {
				t.Fatalf("change after commit: %v", err)
			}

			// After the change commits, a new transaction is denied.
			tx2, err := e.db.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx2.Rollback(e.ctx)
			if err := c.check(tx2, u); !errors.Is(err, permissions.ErrDenied) {
				t.Fatalf("check after change: want denied, got %v", err)
			}
		})
	}
}

func TestTxChecksDenyByDefault(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")
	admin := e.userOfType("admin", permissions.Admin)
	if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, admin, math); err != nil {
		t.Fatal(err)
	}
	physics := e.subject("simulated-physics")
	err := pgx.BeginFunc(e.ctx, e.db, func(tx pgx.Tx) error {
		for _, p := range allGlobal {
			if err := permissions.RequireTx(e.ctx, tx, admin, p); err != nil {
				t.Errorf("held %s denied: %v", p, err)
			}
		}
		if err := permissions.RequireReviewerTx(e.ctx, tx, admin, math); err != nil {
			t.Errorf("assigned subject denied: %v", err)
		}
		denied := map[string]error{
			"reviewer as global": permissions.RequireTx(e.ctx, tx, admin, permissions.Reviewer),
			"unknown permission": permissions.RequireTx(e.ctx, tx, admin, "superuser"),
			"zero user":          permissions.RequireTx(e.ctx, tx, 0, permissions.Author),
			"unknown user":       permissions.RequireTx(e.ctx, tx, 999999, permissions.Author),
			"other subject":      permissions.RequireReviewerTx(e.ctx, tx, admin, physics),
			"unknown subject":    permissions.RequireReviewerTx(e.ctx, tx, admin, 999999),
			"zero reviewer":      permissions.RequireReviewerTx(e.ctx, tx, 0, math),
		}
		for name, err := range denied {
			if !errors.Is(err, permissions.ErrDenied) {
				t.Errorf("%s: want denied, got %v", name, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, admin); err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(e.ctx, e.db, func(tx pgx.Tx) error {
		if err := permissions.RequireTx(e.ctx, tx, admin, permissions.Administrator); !errors.Is(err, permissions.ErrDenied) {
			t.Errorf("disabled account kept administrator: %v", err)
		}
		if err := permissions.RequireReviewerTx(e.ctx, tx, admin, math); !errors.Is(err, permissions.ErrDenied) {
			t.Errorf("disabled account kept reviewer: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Two administrators revoking each other's administrator at the same time:
// the FOR SHARE lock on each actor's own grant means at most one succeeds and
// an administrator always remains. The loser is denied (it ran after the
// winner committed) or aborted as a deadlock (40P01).
func TestConcurrentAdministratorsCannotRevokeEachOther(t *testing.T) {
	e := newEnv(t)
	deadlocks := 0
	for i := range 10 {
		a := e.user("admin-a" + strconv.Itoa(i))
		b := e.user("admin-b" + strconv.Itoa(i))
		for _, u := range []int64{a, b} {
			if _, err := e.store.Grant(e.ctx, permissions.Operator, u, permissions.Administrator); err != nil {
				t.Fatal(err)
			}
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j, pair := range [][2]int64{{a, b}, {b, a}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[j] = e.store.Revoke(e.ctx, permissions.UserActor(pair[0]), pair[1], permissions.Administrator)
			}()
		}
		close(start)
		wg.Wait()

		ok := 0
		for _, err := range errs {
			var pgErr *pgconn.PgError
			switch {
			case err == nil:
				ok++
			case errors.As(err, &pgErr) && pgErr.Code == "40P01":
				deadlocks++
			case errors.Is(err, permissions.ErrDenied):
			default:
				t.Fatalf("iteration %d: unexpected error %v", i, err)
			}
		}
		if ok != 1 {
			t.Fatalf("iteration %d: %d revocations succeeded (%v), want exactly 1", i, ok, errs)
		}
		remaining := 0
		for _, u := range []int64{a, b} {
			if e.set(u).Has(permissions.Administrator) {
				remaining++
			}
		}
		if remaining != 1 {
			t.Fatalf("iteration %d: %d administrators remain, want 1", i, remaining)
		}
	}
	t.Logf("%d of 10 races ended in a deadlock abort", deadlocks)
}

// Grant and audit times are both the transaction time.
func TestAuditTimeMatchesGrantTime(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")
	u := e.user("u")
	if _, err := e.store.Grant(e.ctx, permissions.Operator, u, permissions.Author); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GrantReviewer(e.ctx, permissions.Operator, u, math); err != nil {
		t.Fatal(err)
	}
	var mismatches int
	err := e.db.QueryRow(e.ctx, `
		SELECT count(*) FROM permission_audit a
		  LEFT JOIN user_permissions p ON a.subject_id IS NULL AND p.user_id = a.target_user_id AND p.permission = a.permission
		  LEFT JOIN reviewer_assignments r ON r.user_id = a.target_user_id AND r.subject_id = a.subject_id
		 WHERE a.target_user_id = $1 AND coalesce(p.granted_at, r.granted_at) IS DISTINCT FROM a.occurred_at`, u).Scan(&mismatches)
	if err != nil {
		t.Fatal(err)
	}
	if mismatches != 0 {
		t.Fatalf("%d audit rows disagree with their grant time", mismatches)
	}
	if log, _ := e.store.Audit(e.ctx, u); len(log) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(log))
	}
}

func TestRevokeUnknownTargets(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")
	admin := e.userOfType("admin", permissions.Admin)
	u := e.user("u")
	for _, actor := range []permissions.Actor{permissions.Operator, permissions.UserActor(admin)} {
		if _, err := e.store.Revoke(e.ctx, actor, 999999, permissions.Author); !errors.Is(err, permissions.ErrUnknownUser) {
			t.Errorf("revoke unknown user: %v", err)
		}
		if _, err := e.store.RevokeReviewer(e.ctx, actor, 999999, math); !errors.Is(err, permissions.ErrUnknownUser) {
			t.Errorf("revoke reviewer of unknown user: %v", err)
		}
		if _, err := e.store.RevokeReviewer(e.ctx, actor, u, 999999); !errors.Is(err, permissions.ErrUnknownSubject) {
			t.Errorf("revoke reviewer of unknown subject: %v", err)
		}
		// An absent grant on a known user and subject stays a no-op.
		if changed, err := e.store.Revoke(e.ctx, actor, u, permissions.Publisher); changed || err != nil {
			t.Errorf("revoke absent grant: changed=%v err=%v", changed, err)
		}
		if changed, err := e.store.RevokeReviewer(e.ctx, actor, u, math); changed || err != nil {
			t.Errorf("revoke absent reviewer: changed=%v err=%v", changed, err)
		}
	}
	// A non-administrator learns nothing about which users exist.
	if _, err := e.store.Revoke(e.ctx, permissions.UserActor(u), 999999, permissions.Author); !errors.Is(err, permissions.ErrDenied) {
		t.Errorf("non-administrator revoke of unknown user: %v", err)
	}
}
