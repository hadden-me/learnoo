package accounts_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/accounts"
	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Simulated users only.
var testArgon2 = auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}

const testPassword = "simulated-password-1"

const raceIterations = 40

// testOperatorLabel labels operator, the operator actor used by these tests.
const testOperatorLabel = "test"

var operator = func() permissions.Actor {
	a, err := permissions.OperatorAs(testOperatorLabel)
	if err != nil {
		panic(err)
	}
	return a
}()

type env struct {
	t     *testing.T
	ctx   context.Context
	db    *pgxpool.Pool
	perms *permissions.Store
	accts *accounts.Store
	auth  *auth.Service
}

func newEnv(t *testing.T) *env {
	db := dbtest.Migrated(t)
	cfg := auth.DefaultConfig
	cfg.Argon2 = testArgon2
	svc, err := auth.New(db, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: context.Background(), db: db, perms: permissions.NewStore(db), accts: accounts.NewStore(db), auth: svc}
}

func (e *env) user(name string, t permissions.AccountType) int64 {
	e.t.Helper()
	hash, err := auth.HashPassword(testPassword, testArgon2)
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := users.Create(e.ctx, e.db, name+"@example.test", name, hash)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.perms.GrantDefaults(e.ctx, operator, u.ID, t); err != nil {
		e.t.Fatal(err)
	}
	return u.ID
}

func (e *env) login(name string) string {
	e.t.Helper()
	token, _, err := e.auth.Login(e.ctx, name+"@example.test", testPassword, "192.0.2.1", "")
	if err != nil {
		e.t.Fatal(err)
	}
	return token
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) activeAdmins() int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM user_permissions p JOIN users u ON u.id = p.user_id
		WHERE p.permission = 'administrator' AND u.disabled_at IS NULL`)
}

func (e *env) disabled(id int64) bool {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM users WHERE id = $1 AND disabled_at IS NOT NULL`, id) == 1
}

func (e *env) warm(n int) {
	e.t.Helper()
	var conns []*pgxpool.Conn
	for range n {
		c, err := e.db.Acquire(e.ctx)
		if err != nil {
			e.t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}

func TestDisableRevokesSessionsAndIsAudited(t *testing.T) {
	e := newEnv(t)
	e.user("admin", permissions.Admin)
	teacher := e.user("teacher", permissions.Teacher)
	other := e.user("other", permissions.Student)
	t1, t2 := e.login("teacher"), e.login("teacher")
	otherToken := e.login("other")

	changed, err := e.accts.Disable(e.ctx, operator, teacher)
	if err != nil || !changed {
		t.Fatalf("disable: %v %v", changed, err)
	}
	if !e.disabled(teacher) {
		t.Fatal("not disabled")
	}
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = $1`, teacher); n != 0 {
		t.Fatalf("%d sessions left", n)
	}
	for _, tok := range []string{t1, t2} {
		if _, err := e.auth.Lookup(e.ctx, tok); err == nil {
			t.Fatal("revoked session still valid")
		}
	}
	if _, err := e.auth.Lookup(e.ctx, otherToken); err != nil {
		t.Fatalf("other user's session revoked: %v", err)
	}
	if _, _, err := e.auth.Login(e.ctx, "teacher@example.test", testPassword, "192.0.2.1", ""); err == nil {
		t.Fatal("disabled account signed in")
	}
	if err := e.perms.Require(e.ctx, teacher, permissions.Author); !errors.Is(err, permissions.ErrDenied) {
		t.Fatal("disabled account kept author")
	}

	// Repeats are no-ops without audit rows.
	if changed, err := e.accts.Disable(e.ctx, operator, teacher); err != nil || changed {
		t.Fatalf("repeat disable: %v %v", changed, err)
	}
	if changed, err := e.accts.Enable(e.ctx, operator, teacher); err != nil || !changed {
		t.Fatalf("enable: %v %v", changed, err)
	}
	if changed, err := e.accts.Enable(e.ctx, operator, other); err != nil || changed {
		t.Fatalf("enable active account: %v %v", changed, err)
	}
	if e.disabled(teacher) {
		t.Fatal("still disabled")
	}
	// Grants survive disabling and apply again once enabled.
	if err := e.perms.Require(e.ctx, teacher, permissions.Author); err != nil {
		t.Fatalf("enabled account lost author: %v", err)
	}
	e.login("teacher")

	log, err := e.accts.Audit(e.ctx, teacher)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 || log[0].Action != "disable" || log[1].Action != "enable" {
		t.Fatalf("audit = %+v", log)
	}
	for _, a := range log {
		if a.ActorID != nil || a.OperatorLabel != testOperatorLabel || a.TargetID != teacher || a.OccurredAt.IsZero() {
			t.Fatalf("audit row: %+v", a)
		}
	}
	if n := e.count(`SELECT count(*) FROM account_status_audit`); n != 2 {
		t.Fatalf("%d status audit rows, want 2", n)
	}
}

func TestOnlyAdministratorsChangeAccountStatus(t *testing.T) {
	e := newEnv(t)
	admin := e.user("admin", permissions.Admin)
	teacher := e.user("teacher", permissions.Teacher)
	student := e.user("student", permissions.Student)
	e.login("student")
	for name, actor := range map[string]int64{"teacher": teacher, "student": student, "zero": 0} {
		for _, target := range []int64{student, admin} {
			if _, err := e.accts.Disable(e.ctx, permissions.UserActor(actor), target); !errors.Is(err, permissions.ErrDenied) {
				t.Errorf("%s disabled %d: %v", name, target, err)
			}
		}
	}
	if e.disabled(student) || e.count(`SELECT count(*) FROM sessions`) != 1 || e.count(`SELECT count(*) FROM account_status_audit`) != 0 {
		t.Fatal("denied disable changed something")
	}
	// An active administrator may, and is recorded as the actor.
	if _, err := e.accts.Disable(e.ctx, permissions.UserActor(admin), student); err != nil {
		t.Fatal(err)
	}
	log, _ := e.accts.Audit(e.ctx, student)
	if len(log) != 1 || log[0].ActorID == nil || *log[0].ActorID != admin || log[0].OperatorLabel != "" {
		t.Fatalf("audit = %+v", log)
	}
	// A disabled administrator cannot.
	second := e.user("second", permissions.Admin)
	if _, err := e.accts.Disable(e.ctx, permissions.UserActor(admin), second); err != nil {
		t.Fatal(err)
	}
	if _, err := e.accts.Enable(e.ctx, permissions.UserActor(second), second); !errors.Is(err, permissions.ErrDenied) {
		t.Fatalf("disabled administrator re-enabled itself: %v", err)
	}
	if _, err := e.accts.Disable(e.ctx, operator, 999999); !errors.Is(err, permissions.ErrUnknownUser) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestLastAdministratorCannotBeDisabled(t *testing.T) {
	e := newEnv(t)
	admin := e.user("admin", permissions.Admin)
	e.login("admin")
	for name, actor := range map[string]permissions.Actor{"operator": operator, "self": permissions.UserActor(admin)} {
		if _, err := e.accts.Disable(e.ctx, actor, admin); !errors.Is(err, permissions.ErrLastAdministrator) {
			t.Fatalf("%s disabled the last administrator: %v", name, err)
		}
	}
	if e.disabled(admin) || e.count(`SELECT count(*) FROM sessions`) != 1 || e.count(`SELECT count(*) FROM account_status_audit`) != 0 {
		t.Fatal("refused disable changed something")
	}
	// A disabled administrator does not count as active.
	old := e.user("old", permissions.Admin)
	if _, err := e.accts.Disable(e.ctx, operator, old); err != nil {
		t.Fatal(err)
	}
	if _, err := e.accts.Disable(e.ctx, operator, admin); !errors.Is(err, permissions.ErrLastAdministrator) {
		t.Fatalf("disabled administrator counted as active: %v", err)
	}
	if _, err := e.perms.Revoke(e.ctx, operator, admin, permissions.Administrator); !errors.Is(err, permissions.ErrLastAdministrator) {
		t.Fatalf("revoke with only a disabled second administrator: %v", err)
	}
	// Re-enabling restores a second active administrator.
	if _, err := e.accts.Enable(e.ctx, operator, old); err != nil {
		t.Fatal(err)
	}
	if _, err := e.accts.Disable(e.ctx, operator, admin); err != nil {
		t.Fatalf("disable with two administrators: %v", err)
	}
	if e.activeAdmins() != 1 {
		t.Fatalf("%d active administrators", e.activeAdmins())
	}
}

// race runs a and b together behind a barrier and checks exactly one
// succeeded and the other was refused with one of refusals (by default
// permissions.ErrLastAdministrator), leaving one active administrator. Any
// other error, a deadlock (SQLSTATE 40P01) included, fails the test.
func (e *env) race(name string, i int, a, b func() error, refusals ...error) {
	e.t.Helper()
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for g, f := range []func() error{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[g] = f()
		}()
	}
	if len(refusals) == 0 {
		refusals = []error{permissions.ErrLastAdministrator}
	}
	close(start)
	wg.Wait()
	ok, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case slices.ContainsFunc(refusals, func(r error) bool { return errors.Is(err, r) }):
			refused++
		default:
			e.t.Fatalf("%s iteration %d: unexpected error %v", name, i, err)
		}
	}
	if ok != 1 || refused != 1 || e.activeAdmins() != 1 {
		e.t.Fatalf("%s iteration %d: ok=%d refused=%d admins=%d", name, i, ok, refused, e.activeAdmins())
	}
}

func TestConcurrentRemovalsLeaveOneAdministrator(t *testing.T) {
	e := newEnv(t)
	a := e.user("admin-a", permissions.Admin)
	b := e.user("admin-b", permissions.Admin)
	e.warm(2)
	ctx := context.Background()
	restore := func() {
		for _, id := range []int64{a, b} {
			if _, err := e.accts.Enable(e.ctx, operator, id); err != nil {
				t.Fatal(err)
			}
			if _, err := e.perms.Grant(e.ctx, operator, id, permissions.Administrator); err != nil {
				t.Fatal(err)
			}
		}
	}
	disable := func(id int64) func() error {
		return func() error { _, err := e.accts.Disable(ctx, operator, id); return err }
	}
	revoke := func(id int64) func() error {
		return func() error {
			_, err := e.perms.Revoke(ctx, operator, id, permissions.Administrator)
			return err
		}
	}
	selfDisable := func(id int64) func() error {
		return func() error { _, err := e.accts.Disable(ctx, permissions.UserActor(id), id); return err }
	}
	// Each administrator acting on the other: whichever commits second
	// finds its own account disabled or its grant revoked and is denied.
	crossDisable := func(actor, target int64) func() error {
		return func() error {
			_, err := e.accts.Disable(ctx, permissions.UserActor(actor), target)
			return err
		}
	}
	crossRevoke := func(actor, target int64) func() error {
		return func() error {
			_, err := e.perms.Revoke(ctx, permissions.UserActor(actor), target, permissions.Administrator)
			return err
		}
	}
	cases := []struct {
		name     string
		a, b     func() error
		refusals []error
	}{
		{"disable+disable", disable(a), disable(b), nil},
		{"disable+revoke", disable(a), revoke(b), nil},
		{"revoke+disable", revoke(a), disable(b), nil},
		{"self-disable+self-disable", selfDisable(a), selfDisable(b), nil},
		{"cross-disable", crossDisable(a, b), crossDisable(b, a), []error{permissions.ErrDenied}},
		{"cross-disable+revoke", crossDisable(a, b), crossRevoke(b, a), []error{permissions.ErrDenied}},
	}
	for _, c := range cases {
		for i := range raceIterations {
			e.race(c.name, i, c.a, c.b, c.refusals...)
			restore()
		}
	}
}

// Operator tools are audited with their label and no actor_id; signed-in
// administrators with their id and no label.
func TestStatusAuditRecordsOperatorLabel(t *testing.T) {
	e := newEnv(t)
	admin := e.user("admin", permissions.Admin)
	student := e.user("student", permissions.Student)
	named, err := permissions.OperatorAs("  simulated-op@simulated-host ")
	if err != nil {
		t.Fatal(err)
	}
	local, err := permissions.OperatorAs(permissions.LocalOperatorLabel())
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		do        func() (bool, error)
		wantActor bool
		wantLabel string
	}{
		{func() (bool, error) { return e.accts.Disable(e.ctx, named, student) }, false, "simulated-op@simulated-host"},
		{func() (bool, error) { return e.accts.Enable(e.ctx, permissions.UserActor(admin), student) }, true, ""},
		{func() (bool, error) { return e.accts.Disable(e.ctx, local, student) }, false, permissions.LocalOperatorLabel()},
		{func() (bool, error) { return e.accts.Enable(e.ctx, named, student) }, false, "simulated-op@simulated-host"},
	}
	for i, st := range steps {
		if changed, err := st.do(); !changed || err != nil {
			t.Fatalf("step %d: changed=%v err=%v", i, changed, err)
		}
	}
	log, err := e.accts.Audit(e.ctx, student)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != len(steps) {
		t.Fatalf("audit rows = %d, want %d", len(log), len(steps))
	}
	for i, st := range steps {
		a := log[i]
		if (a.ActorID != nil) != st.wantActor || a.OperatorLabel != st.wantLabel {
			t.Errorf("row %d: actor=%v label=%q, want actor=%v label=%q", i, a.ActorID, a.OperatorLabel, st.wantActor, st.wantLabel)
		}
	}
	if n := e.count(`SELECT count(*) FROM account_status_audit WHERE actor_id IS NOT NULL AND operator_label IS NOT NULL`); n != 0 {
		t.Errorf("%d user-actor rows carry an operator label", n)
	}
}

// The database requires exactly one of actor_id and operator_label, and
// validates the label as permission_audit does.
func TestStatusAuditActorConstraint(t *testing.T) {
	e := newEnv(t)
	u := e.user("u", permissions.Student)
	insert := `INSERT INTO account_status_audit (occurred_at, actor_id, operator_label, target_user_id, action) VALUES (now(), $1, $2, $3, 'disable')`
	var none *int64
	for name, args := range map[string][]any{
		"actor and label":     {u, "op@host", u},
		"neither":             {none, nil, u},
		"empty label":         {none, "", u},
		"padded label":        {none, " op@host", u},
		"control char":        {none, "op\nx", u},
		"c1 control":          {none, "op\u0085x", u},
		"rtl override":        {none, "alice@host\u202Etsoh@eve", u},
		"zero width space":    {none, "op\u200Bx", u},
		"isolate":             {none, "op\u2066x", u},
		"byte order mark":     {none, "op\uFEFFx", u},
		"soft hyphen":         {none, "op\u00ADx", u},
		"line separator":      {none, "op\u2028x", u},
		"paragraph separator": {none, "op\u2029x", u},
		"tag character":       {none, "op\U000E0041x", u},
		"too long":            {none, strings.Repeat("a", 201), u},
	} {
		if _, err := e.db.Exec(e.ctx, insert, args...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, args := range map[string][]any{
		"user actor":     {u, nil, u},
		"operator":       {none, strings.Repeat("a", 200), u},
		"accented label": {none, "élève@hôte", u},
		"arabic label":   {none, "علي@host", u},
	} {
		if _, err := e.db.Exec(e.ctx, insert, args...); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
}

func TestStatusAuditIsAppendOnly(t *testing.T) {
	e := newEnv(t)
	e.user("admin", permissions.Admin)
	student := e.user("student", permissions.Student)
	if _, err := e.accts.Disable(e.ctx, operator, student); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE account_status_audit SET action = 'enable'`,
		`DELETE FROM account_status_audit`,
		`TRUNCATE account_status_audit`,
		`INSERT INTO account_status_audit (occurred_at, target_user_id, action) VALUES (now(), 1, 'delete')`,
	} {
		if _, err := e.db.Exec(e.ctx, sql); err == nil {
			t.Errorf("%s succeeded", sql)
		}
	}
	if n := e.count(`SELECT count(*) FROM account_status_audit`); n != 1 {
		t.Fatalf("%d rows", n)
	}
}

// An administrator's grant that races the disabling of that administrator
// must not commit after the disable: authorize locks the actor's users row,
// so the grant waits for the disable and is then denied.
func TestGrantRacingActorDisableIsDenied(t *testing.T) {
	e := newEnv(t)
	actor := e.user("admin", permissions.Admin)
	e.user("other-admin", permissions.Admin)
	target := e.user("student", permissions.Student)

	// The disable, held open: the same row update accounts.Disable makes.
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, actor); err != nil {
		t.Fatal(err)
	}
	var holder int32
	if err := tx.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.perms.Grant(context.Background(), permissions.UserActor(actor), target, permissions.Author)
		done <- err
	}()
	blocked := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !blocked; {
		select {
		case err := <-done:
			t.Fatalf("grant finished while the actor's disable was uncommitted: %v", err)
		default:
		}
		blocked = e.count(`SELECT count(*) FROM pg_stat_activity WHERE pg_blocking_pids(pid) @> ARRAY[$1::int]`, holder) > 0
		time.Sleep(5 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("grant never waited for the actor's row")
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, permissions.ErrDenied) {
		t.Fatalf("grant by a just-disabled administrator: %v", err)
	}
	if err := e.perms.Require(e.ctx, target, permissions.Author); !errors.Is(err, permissions.ErrDenied) {
		t.Fatal("grant committed after the actor was disabled")
	}
}

// Status and audit times are the database transaction time: a disable's
// audit row has occurred_at equal to the disabled_at it set, and an enable's
// equals the updated_at written in the same transaction.
func TestAuditTimeMatchesStatusTime(t *testing.T) {
	e := newEnv(t)
	e.user("admin", permissions.Admin)
	u := e.user("student", permissions.Student)
	times := func() (disabledAt, updatedAt *time.Time) {
		t.Helper()
		if err := e.db.QueryRow(e.ctx, `SELECT disabled_at, updated_at FROM users WHERE id = $1`, u).Scan(&disabledAt, &updatedAt); err != nil {
			t.Fatal(err)
		}
		return disabledAt, updatedAt
	}
	if _, err := e.accts.Disable(e.ctx, operator, u); err != nil {
		t.Fatal(err)
	}
	disabledAt, updatedAt := times()
	if _, err := e.accts.Enable(e.ctx, operator, u); err != nil {
		t.Fatal(err)
	}
	enabledDisabledAt, enabledAt := times()
	log, err := e.accts.Audit(e.ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 || log[0].Action != "disable" || log[1].Action != "enable" {
		t.Fatalf("audit = %+v", log)
	}
	if disabledAt == nil || !log[0].OccurredAt.Equal(*disabledAt) || !updatedAt.Equal(*disabledAt) {
		t.Errorf("disable: occurred_at %v, disabled_at %v, updated_at %v; want all equal", log[0].OccurredAt, disabledAt, updatedAt)
	}
	if enabledDisabledAt != nil || !log[1].OccurredAt.Equal(*enabledAt) {
		t.Errorf("enable: occurred_at %v, updated_at %v, disabled_at %v; want occurred_at = updated_at and disabled_at NULL", log[1].OccurredAt, enabledAt, enabledDisabledAt)
	}
	if !log[1].OccurredAt.After(log[0].OccurredAt) {
		t.Errorf("enable at %v not after disable at %v", log[1].OccurredAt, log[0].OccurredAt)
	}
}
