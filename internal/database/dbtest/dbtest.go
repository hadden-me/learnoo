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

	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5"
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
