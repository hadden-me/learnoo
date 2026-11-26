package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A warm-up that cannot get all its connections must fail promptly and
// release the ones it got, so the pool still closes (it used to hang the
// test cleanup until the 10-minute test timeout).
func TestAcquireAllReleasesOnFailure(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(URL(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := database.OpenConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	defer func() {
		go func() { pool.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Error("pool.Close did not return: a connection is still held")
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = acquireAll(ctx, pool, 2)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquireAll beyond MaxConns = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("acquireAll took %v to fail", d)
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d connections still acquired after failure", n)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := acquireAll(ctx, pool, 1); err != nil {
		t.Fatalf("acquireAll after failure: %v", err)
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("%d connections still acquired after success", n)
	}
}
