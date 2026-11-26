// Package dbtest gives database-backed tests an isolated, migrated schema.
//
// Set LEARNOO_TEST_DATABASE_URL to a disposable PostgreSQL database. Without
// it the tests are skipped, unless LEARNOO_REQUIRE_DB_TESTS=1 (as in CI), in
// which case they fail so a misconfigured pipeline cannot pass silently.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvURL names the variable holding the test database URL.
const EnvURL = "LEARNOO_TEST_DATABASE_URL"

// URL returns the test database URL or skips/fails the test.
func URL(t testing.TB) string {
	t.Helper()
	url := os.Getenv(EnvURL)
	if url == "" {
		if os.Getenv("LEARNOO_REQUIRE_DB_TESTS") == "1" {
			t.Fatalf("%s is not set but LEARNOO_REQUIRE_DB_TESTS=1", EnvURL)
		}
		t.Skipf("set %s to run database-backed tests", EnvURL)
	}
	return url
}

// EmptySchema returns a pool whose search_path is a fresh, empty schema that
// is dropped when the test ends. No migrations are applied.
func EmptySchema(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := URL(t)
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "test_" + hex.EncodeToString(b[:])

	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 8
	pool, err := database.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	return pool
}

// WarmTimeout bounds how long Warm waits for its connections.
const WarmTimeout = 30 * time.Second

// Warm opens n pooled connections at once and releases them, so racing
// goroutines do not queue on connection setup. It fails the test if the
// connections cannot be had within WarmTimeout (for example when the shared
// database is at max_connections, or n exceeds the pool's MaxConns), and it
// never keeps a connection: a held one would make the pool's Close in the
// test cleanup wait forever.
func Warm(t testing.TB, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), WarmTimeout)
	defer cancel()
	if err := acquireAll(ctx, pool, n); err != nil {
		t.Fatalf("warm %d connections: %v", n, err)
	}
}

// acquireAll acquires n connections and releases every one it acquired
// before returning, on success and on failure.
func acquireAll(ctx context.Context, pool *pgxpool.Pool, n int) error {
	conns := make([]*pgxpool.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			c.Release()
		}
	}()
	for range n {
		c, err := pool.Acquire(ctx)
		if err != nil {
			return err
		}
		conns = append(conns, c)
	}
	return nil
}

// Migrated returns an isolated schema with every migration applied.
func Migrated(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := EmptySchema(t)
	if _, err := database.Migrate(context.Background(), pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return pool
}

// Querier runs a single-row query (a pool, connection or transaction).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TestOperatorLabel is the operator label of catalogue entries made by tests.
const TestOperatorLabel = "test-operator@test-host"

// SubjectSQL inserts a subject (code $1) with its catalogue_audit row
// (operator label $2) in one statement and returns the subject's ID.
const SubjectSQL = `
	WITH s AS (INSERT INTO subjects (code) VALUES ($1) RETURNING id)
	INSERT INTO catalogue_audit (operator_label, action, subject_id)
	SELECT $2, 'add_subject', id FROM s RETURNING subject_id`

// ChapterSQL inserts a chapter (subject $1, code $2) with its
// catalogue_audit row (operator label $3) and returns the chapter's ID.
const ChapterSQL = `
	WITH c AS (INSERT INTO chapters (subject_id, code) VALUES ($1, $2) RETURNING id, subject_id)
	INSERT INTO catalogue_audit (operator_label, action, subject_id, chapter_id)
	SELECT $3, 'add_chapter', subject_id, id FROM c RETURNING chapter_id`

// AddSubject inserts a subject with its catalogue_audit row in one statement,
// as the database requires (migrations/0010), and returns its ID. Tests that
// need the operator command path use internal/catalogue instead.
func AddSubject(t testing.TB, q Querier, code string) int64 {
	t.Helper()
	var id int64
	err := q.QueryRow(context.Background(), SubjectSQL, code, TestOperatorLabel).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// AddChapter inserts a chapter of subject with its catalogue_audit row and
// returns its ID.
func AddChapter(t testing.TB, q Querier, subject int64, code string) int64 {
	t.Helper()
	var id int64
	err := q.QueryRow(context.Background(), ChapterSQL, subject, code, TestOperatorLabel).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Executor runs a statement (a pool, connection or transaction).
type Executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SubmitTx moves draft revisionID into review with direct SQL inside tx,
// writing first the submission_audit row the database requires in the same
// transaction (migrations/0011), as lessons.Store.Submit does but without
// its permission, version and content checks. tx must be a transaction:
// the audit row is checked against the status at commit.
func SubmitTx(ctx context.Context, tx Executor, revisionID int64) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO submission_audit (lesson_id, revision_id, actor_id)
		SELECT lesson_id, id, author_id FROM lesson_revisions WHERE id = $1`, revisionID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE lesson_revisions
		   SET status = 'in_review', submitted_at = now(), updated_at = now(), version = version + 1
		 WHERE id = $1`, revisionID)
	return err
}

// Submit runs SubmitTx in its own transaction and fails the test on error.
func Submit(t testing.TB, db *pgxpool.Pool, revisionID int64) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error { return SubmitTx(ctx, tx, revisionID) }); err != nil {
		t.Fatalf("submit revision %d: %v", revisionID, err)
	}
}
