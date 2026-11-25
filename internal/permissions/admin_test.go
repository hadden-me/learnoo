package permissions_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5"
)

// Iterations of each concurrency test; every iteration starts the racing
// goroutines together behind a barrier.
const raceIterations = 40

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// hash is computed once: hashing inside racing goroutines would spread
// them out and hide the race.
var hash = func() string {
	h, err := auth.HashPassword(testPassword, testArgon2)
	if err != nil {
		panic(err)
	}
	return h
}()

// warm opens n pooled connections so racing goroutines do not queue on
// connection setup.
func (e *env) warm(n int) {
	e.t.Helper()
	var conns []interface{ Release() }
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

func (e *env) activeAdmins() int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM user_permissions p JOIN users u ON u.id = p.user_id
		WHERE p.permission = 'administrator' AND u.disabled_at IS NULL`)
}

func (e *env) bootstrap(name string) (int64, error) {
	return e.store.Bootstrap(e.ctx, operator, func(tx pgx.Tx) (int64, error) {
		u, err := users.Create(e.ctx, tx, name+"@example.test", name, hash)
		return u.ID, err
	})
}

func TestBootstrapCreatesExactlyOneAdministrator(t *testing.T) {
	e := newEnv(t)
	id, err := e.bootstrap("first-admin")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.set(id).Global(); !slices.Equal(got, []permissions.Permission{permissions.Author, permissions.Publisher, permissions.Administrator}) {
		t.Fatalf("bootstrap grants = %v", got)
	}
	log, err := e.store.Audit(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var audited []permissions.Permission
	for _, a := range log {
		if a.ActorID != nil || a.OperatorLabel != testOperatorLabel || a.Action != "grant" || a.SubjectID != nil || a.TargetID != id || a.OccurredAt.IsZero() {
			t.Errorf("bootstrap audit row: %+v", a)
		}
		audited = append(audited, a.Permission)
	}
	if !slices.Equal(audited, []permissions.Permission{permissions.Administrator, permissions.Publisher, permissions.Author}) {
		t.Fatalf("bootstrap audit = %v", audited)
	}
	if n := e.count(`SELECT count(*) FROM user_permissions`); n != 3 {
		t.Fatalf("%d permission rows, want 3", n)
	}
	if n := e.count(`SELECT count(*) FROM permission_audit`); n != 3 {
		t.Fatalf("%d audit rows, want 3", n)
	}

	// A second bootstrap changes nothing, not even the account.
	if _, err := e.bootstrap("second-admin"); !errors.Is(err, permissions.ErrAdministratorExists) {
		t.Fatalf("second bootstrap: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("%d users after refused bootstrap", n)
	}
	if n := e.count(`SELECT count(*) FROM permission_audit`); n != 3 {
		t.Fatalf("%d audit rows after refused bootstrap", n)
	}
}

func TestBootstrapRollsBackOnFailure(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("simulated failure")
	_, err := e.store.Bootstrap(e.ctx, operator, func(tx pgx.Tx) (int64, error) {
		if _, err := users.Create(e.ctx, tx, "half@example.test", "half", hash); err != nil {
			return 0, err
		}
		return 0, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	// An unknown id from create fails on the foreign key and rolls back too.
	if _, err := e.store.Bootstrap(e.ctx, operator, func(pgx.Tx) (int64, error) { return 999999, nil }); !errors.Is(err, permissions.ErrUnknownUser) {
		t.Fatalf("unknown id: %v", err)
	}
	// Only an operator may bootstrap; a user actor is denied before create runs.
	if _, err := e.store.Bootstrap(e.ctx, permissions.UserActor(1), func(pgx.Tx) (int64, error) {
		t.Fatal("create ran for a user actor")
		return 0, nil
	}); !errors.Is(err, permissions.ErrDenied) {
		t.Fatalf("user actor: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM users`) + e.count(`SELECT count(*) FROM permission_audit`); n != 0 {
		t.Fatal("failed bootstrap left rows behind")
	}
}

func TestBootstrapIgnoresDisabledAdministrators(t *testing.T) {
	e := newEnv(t)
	old := e.userOfType("old-admin", permissions.Admin)
	if _, err := e.db.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bootstrap("new-admin"); err != nil {
		t.Fatalf("bootstrap with only a disabled administrator: %v", err)
	}
}

func TestConcurrentBootstrapHasOneWinner(t *testing.T) {
	for i := range 10 {
		e := newEnv(t)
		const n = 6
		e.warm(n)
		start := make(chan struct{})
		errs := make([]error, n)
		var wg sync.WaitGroup
		for g := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[g] = e.bootstrap(fmt.Sprintf("admin-%d", g))
			}()
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, permissions.ErrAdministratorExists):
				t.Fatalf("iteration %d: unexpected error %v", i, err)
			}
		}
		if wins != 1 || e.count(`SELECT count(*) FROM users`) != 1 || e.activeAdmins() != 1 ||
			e.count(`SELECT count(*) FROM permission_audit`) != 3 {
			t.Fatalf("iteration %d: %d winners, %d users, %d admins", i, wins, e.count(`SELECT count(*) FROM users`), e.activeAdmins())
		}
	}
}

func TestLastAdministratorCannotBeRevoked(t *testing.T) {
	e := newEnv(t)
	admin := e.userOfType("admin", permissions.Admin)
	before := e.count(`SELECT count(*) FROM permission_audit`)

	for name, actor := range map[string]permissions.Actor{"operator": operator, "self": permissions.UserActor(admin)} {
		if _, err := e.store.Revoke(e.ctx, actor, admin, permissions.Administrator); !errors.Is(err, permissions.ErrLastAdministrator) {
			t.Fatalf("%s revoked the last administrator: %v", name, err)
		}
	}
	if e.activeAdmins() != 1 || e.count(`SELECT count(*) FROM permission_audit`) != before {
		t.Fatal("refused revocation changed something")
	}
	// Other permissions of the last administrator can still be revoked.
	if changed, err := e.store.Revoke(e.ctx, operator, admin, permissions.Publisher); err != nil || !changed {
		t.Fatalf("revoke publisher: %v %v", changed, err)
	}

	// A disabled administrator does not count as active.
	disabled := e.userOfType("disabled-admin", permissions.Admin)
	if _, err := e.db.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Revoke(e.ctx, operator, admin, permissions.Administrator); !errors.Is(err, permissions.ErrLastAdministrator) {
		t.Fatalf("disabled administrator counted as active: %v", err)
	}
	// Revoking the disabled one's grant does not reduce the active count.
	if changed, err := e.store.Revoke(e.ctx, operator, disabled, permissions.Administrator); err != nil || !changed {
		t.Fatalf("revoke from disabled administrator: %v %v", changed, err)
	}

	// With a second active administrator, either may be revoked, by an
	// administrator user actor too.
	second := e.userOfType("second", permissions.Admin)
	if changed, err := e.store.Revoke(e.ctx, permissions.UserActor(second), admin, permissions.Administrator); err != nil || !changed {
		t.Fatalf("revoke with two administrators: %v %v", changed, err)
	}
	if _, err := e.store.Revoke(e.ctx, permissions.UserActor(second), second, permissions.Administrator); !errors.Is(err, permissions.ErrLastAdministrator) {
		t.Fatalf("self-revocation of the last administrator: %v", err)
	}
	// Revoking from a non-administrator is an ordinary no-op.
	student := e.user("student")
	if changed, err := e.store.Revoke(e.ctx, operator, student, permissions.Administrator); err != nil || changed {
		t.Fatalf("no-op revoke: %v %v", changed, err)
	}
	if e.activeAdmins() != 1 {
		t.Fatalf("%d active administrators", e.activeAdmins())
	}
}

func TestConcurrentRevokesLeaveOneAdministrator(t *testing.T) {
	e := newEnv(t)
	a := e.userOfType("admin-a", permissions.Admin)
	b := e.userOfType("admin-b", permissions.Admin)
	e.warm(2)
	actors := map[string]func(self int64) permissions.Actor{
		"operator": func(int64) permissions.Actor { return operator },
		"user":     func(self int64) permissions.Actor { return permissions.UserActor(self) },
	}
	for name, actor := range actors {
		for i := range raceIterations {
			start := make(chan struct{})
			errs := make([]error, 2)
			var wg sync.WaitGroup
			for g, target := range []int64{a, b} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, errs[g] = e.store.Revoke(context.Background(), actor(target), target, permissions.Administrator)
				}()
			}
			close(start)
			wg.Wait()
			ok, refused := 0, 0
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, permissions.ErrLastAdministrator):
					refused++
				default:
					t.Fatalf("%s iteration %d: unexpected error %v", name, i, err)
				}
			}
			if ok != 1 || refused != 1 || e.activeAdmins() != 1 {
				t.Fatalf("%s iteration %d: ok=%d refused=%d admins=%d", name, i, ok, refused, e.activeAdmins())
			}
			for _, id := range []int64{a, b} {
				if _, err := e.store.Grant(e.ctx, operator, id, permissions.Administrator); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestOperatorLookups(t *testing.T) {
	e := newEnv(t)
	math := e.subject("simulated-math")
	e.subject("simulated-physics")
	u := e.userOfType("teacher", permissions.Teacher)
	if id, err := e.store.SubjectID(e.ctx, "simulated-math"); err != nil || id != math {
		t.Fatalf("SubjectID = %d, %v", id, err)
	}
	if _, err := e.store.SubjectID(e.ctx, "missing"); !errors.Is(err, permissions.ErrUnknownSubject) {
		t.Fatalf("unknown code: %v", err)
	}
	if _, err := e.store.GrantReviewer(e.ctx, operator, u, math); err != nil {
		t.Fatal(err)
	}
	// Grants shows stored grants even while the account is disabled.
	if _, err := e.db.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, u); err != nil {
		t.Fatal(err)
	}
	global, subjects, err := e.store.Grants(e.ctx, u)
	if err != nil || !slices.Equal(global, []permissions.Permission{permissions.Author}) || !slices.Equal(subjects, []string{"simulated-math"}) {
		t.Fatalf("Grants = %v %v %v", global, subjects, err)
	}
	log, _ := e.store.Audit(e.ctx, u)
	if last := log[len(log)-1]; last.SubjectCode != "simulated-math" {
		t.Fatalf("audit subject code: %+v", last)
	}
	for _, name := range []string{"author", "reviewer", "publisher", "administrator"} {
		if p, err := permissions.Parse(name); err != nil || string(p) != name {
			t.Errorf("Parse(%q) = %q, %v", name, p, err)
		}
	}
	for _, name := range []string{"", "Author", "superuser", "admin"} {
		if _, err := permissions.Parse(name); err == nil {
			t.Errorf("Parse(%q) accepted", name)
		}
	}
}

// Two administrators revoking each other at once. Each transaction locks its
// own actor's grant (authorize) and deletes the other's, so without the
// administrators lock taken first they deadlock. With it they serialize: one
// revocation succeeds and the other actor, no longer an administrator, is
// denied.
func TestCrossActorRevokesDoNotDeadlock(t *testing.T) {
	e := newEnv(t)
	a := e.userOfType("admin-a", permissions.Admin)
	b := e.userOfType("admin-b", permissions.Admin)
	e.warm(2)
	for i := range raceIterations {
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for g, pair := range [][2]int64{{a, b}, {b, a}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[g] = e.store.Revoke(context.Background(), permissions.UserActor(pair[0]), pair[1], permissions.Administrator)
			}()
		}
		close(start)
		wg.Wait()
		// The loser ran after the winner committed: it is no longer an
		// administrator, so it is denied (never a deadlock, and never the
		// last-administrator refusal, which would mean it was authorized).
		ok, denied := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, permissions.ErrDenied) && !errors.Is(err, permissions.ErrLastAdministrator):
				denied++
			default:
				t.Fatalf("iteration %d: unexpected error %v", i, err)
			}
		}
		if ok != 1 || denied != 1 || e.activeAdmins() != 1 {
			t.Fatalf("iteration %d: %d succeeded, %d active administrators (%v)", i, ok, e.activeAdmins(), errs)
		}
		for _, id := range []int64{a, b} {
			if _, err := e.store.Grant(e.ctx, operator, id, permissions.Administrator); err != nil {
				t.Fatal(err)
			}
		}
	}
}
