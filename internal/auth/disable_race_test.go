package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/accounts"
	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Simulated account only.
const (
	raceEmail    = "race@example.test"
	racePassword = "simulated-password-1"
)

// operator is the operator actor used by these tests.
var operator = func() permissions.Actor {
	a, err := permissions.OperatorAs("test")
	if err != nil {
		panic(err)
	}
	return a
}()

var raceArgon2 = auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}

type raceEnv struct {
	t     *testing.T
	ctx   context.Context
	db    *pgxpool.Pool
	svc   *auth.Service
	accts *accounts.Store
	id    int64
}

func newRaceEnv(t *testing.T) *raceEnv {
	db := dbtest.Migrated(t)
	cfg := auth.DefaultConfig
	cfg.Argon2 = raceArgon2
	svc, err := auth.New(db, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(racePassword, raceArgon2)
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.Create(context.Background(), db, raceEmail, "Simulated Race", hash)
	if err != nil {
		t.Fatal(err)
	}
	return &raceEnv{t: t, ctx: context.Background(), db: db, svc: svc, accts: accounts.NewStore(db), id: u.ID}
}

func (e *raceEnv) sessions() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, e.id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// pid returns the backend pid of the connection running tx.
func (e *raceEnv) pid(tx pgx.Tx) int32 {
	e.t.Helper()
	var pid int32
	if err := tx.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		e.t.Error(err)
	}
	return pid
}

// waitBlockedBy waits until some backend is waiting on a lock held by the
// backend holder. It fails fast if done reports that the other side
// finished without waiting.
func (e *raceEnv) waitBlockedBy(holder int32, done <-chan error) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			e.t.Errorf("finished without waiting for backend %d: %v", holder, err)
			return false
		default:
		}
		var blocked bool
		if err := e.db.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pg_blocking_pids(pid) @> ARRAY[$1::int])`, holder).Scan(&blocked); err != nil {
			e.t.Error(err)
			return false
		}
		if blocked {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Errorf("nothing waited for backend %d", holder)
	return false
}

// A disable commits after Login checked the password but before it creates
// the session: Login must refuse, indistinguishably from a bad password.
func TestDisableBetweenCredentialCheckAndSession(t *testing.T) {
	e := newRaceEnv(t)
	auth.SetLoginHooks(e.svc, func() {
		if _, err := e.accts.Disable(e.ctx, operator, e.id); err != nil {
			t.Error(err)
		}
	}, nil)
	_, _, err := e.svc.Login(e.ctx, raceEmail, racePassword, "192.0.2.1", "")
	if !errors.Is(err, auth.ErrInvalidCredentials) || err.Error() != auth.ErrInvalidCredentials.Error() {
		t.Fatalf("login racing a disable: %v", err)
	}
	if n := e.sessions(); n != 0 {
		t.Fatalf("%d sessions for a disabled account", n)
	}
	auth.SetLoginHooks(e.svc, nil, nil)
	if _, err := e.accts.Enable(e.ctx, operator, e.id); err != nil {
		t.Fatal(err)
	}
	if n := e.sessions(); n != 0 {
		t.Fatalf("enable revived %d sessions", n)
	}
}

// A disable starts while Login's session transaction holds the account row:
// the disable waits, then deletes the new session.
func TestDisableDuringSessionTransaction(t *testing.T) {
	e := newRaceEnv(t)
	done := make(chan error, 1)
	auth.SetLoginHooks(e.svc, nil, func(tx pgx.Tx) {
		holder := e.pid(tx)
		go func() {
			_, err := e.accts.Disable(context.Background(), operator, e.id)
			done <- err
		}()
		if !e.waitBlockedBy(holder, done) {
			t.Error("disable did not wait for the login's row lock")
		}
	})
	token, _, err := e.svc.Login(e.ctx, raceEmail, racePassword, "192.0.2.1", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if t.Failed() {
		t.FailNow()
	}
	if err := <-done; err != nil {
		t.Fatalf("disable: %v", err)
	}
	if n := e.sessions(); n != 0 {
		t.Fatalf("%d sessions survived the disable", n)
	}
	if _, err := e.svc.Lookup(e.ctx, token); err == nil {
		t.Fatal("session usable after disable")
	}
	auth.SetLoginHooks(e.svc, nil, nil)
	if _, err := e.accts.Enable(e.ctx, operator, e.id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Lookup(e.ctx, token); err == nil || e.sessions() != 0 {
		t.Fatal("enable revived the session")
	}
}

// The disable's row update is uncommitted when Login's session transaction
// locks the account row: Login waits for it, then re-reads the disabled row
// and fails like a bad password, leaving no session.
func TestLoginWaitsForUncommittedDisable(t *testing.T) {
	e := newRaceEnv(t)
	holderc := make(chan int32, 1)
	var disable pgx.Tx
	// After the password check, open the disable (the same row update
	// accounts.Disable makes first) and leave it uncommitted.
	auth.SetLoginHooks(e.svc, func() {
		tx, err := e.db.Begin(e.ctx)
		if err != nil {
			t.Error(err)
			holderc <- 0
			return
		}
		disable = tx
		if _, err := tx.Exec(e.ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, e.id); err != nil {
			t.Error(err)
		}
		holderc <- e.pid(tx)
	}, nil)
	done := make(chan error, 1)
	go func() {
		_, _, err := e.svc.Login(context.Background(), raceEmail, racePassword, "192.0.2.1", "")
		done <- err
	}()
	holder := <-holderc
	if holder == 0 || !e.waitBlockedBy(holder, done) {
		if disable != nil {
			_ = disable.Rollback(e.ctx)
		}
		t.FailNow()
	}
	if _, err := disable.Exec(e.ctx, `DELETE FROM sessions WHERE user_id = $1`, e.id); err != nil {
		t.Fatal(err)
	}
	if err := disable.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if !errors.Is(err, auth.ErrInvalidCredentials) || err.Error() != auth.ErrInvalidCredentials.Error() {
		t.Fatalf("login after a racing disable: %v", err)
	}
	if n := e.sessions(); n != 0 {
		t.Fatalf("%d sessions for a disabled account", n)
	}
}

// Enable deletes sessions even if one exists for the disabled account (for
// example, written before this fix or directly in SQL).
func TestEnableDeletesLeftoverSessions(t *testing.T) {
	e := newRaceEnv(t)
	if _, err := e.accts.Disable(e.ctx, operator, e.id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(e.ctx, `INSERT INTO sessions (token_hash, user_id, csrf_token, created_at, last_seen_at, idle_expires_at, absolute_expires_at)
		VALUES (sha256('leftover'), $1, 'csrf', now(), now(), now() + interval '1 hour', now() + interval '1 day')`, e.id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.accts.Enable(e.ctx, operator, e.id); err != nil {
		t.Fatal(err)
	}
	if n := e.sessions(); n != 0 {
		t.Fatalf("enable kept %d sessions", n)
	}
}
