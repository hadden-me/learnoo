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
	"testing"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
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
