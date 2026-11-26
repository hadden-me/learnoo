package curriculum_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsUpTo returns the embedded migrations whose version is at most v.
func migrationsUpTo(t *testing.T, v string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		name := en.Name()
		if !strings.HasSuffix(name, ".sql") || name[:4] > v {
			continue
		}
		data, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &fstest.MapFile{Data: data}
	}
	return out
}

// cLocaleDatabase creates a fresh UTF-8 database with the C locale (the
// pattern of internal/reader/fold_test.go), dropped when the test ends.
func cLocaleDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := dbtest.URL(t)
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "learnoo_curriculum_c_" + hex.EncodeToString(b[:])
	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+
		" TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		t.Fatalf("create C database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 2
	pool, err := database.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Upgrading a populated 0012 database (catalogue names and order set through
// audited changes) keeps every subject and chapter ID, code, name, order and
// audit row, places nothing into any edition and retires nothing.
func TestUpgradeFrom0012KeepsCatalogue(t *testing.T) {
	for name, open := range map[string]func(*testing.T) *pgxpool.Pool{
		"default locale": func(t *testing.T) *pgxpool.Pool { return dbtest.EmptySchema(t) },
		"C locale":       cLocaleDatabase,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := open(t)
			if _, err := database.Migrate(ctx, pool, migrationsUpTo(t, "0012")); err != nil {
				t.Fatal(err)
			}
			subjects := map[string]int64{}
			chapters := map[int64]string{}
			for _, code := range []string{"test-zeta", "test-alpha", "test-mid"} {
				subjects[code] = dbtest.AddSubject(t, pool, code)
				for _, ch := range []string{"ch-02", "ch-01"} {
					chapters[dbtest.AddChapter(t, pool, subjects[code], ch)] = code + "/" + ch
				}
			}
			// Names and order set as the 0012 operator commands do: the audit
			// row of the same transaction, then the change. (The catalogue
			// store already reads 0013's retired_at, so it is not used here.)
			change := func(audit string, auditArgs []any, update string, updateArgs ...any) {
				t.Helper()
				if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, audit, auditArgs...); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, update, updateArgs...)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			zeta, alpha, mid := subjects["test-zeta"], subjects["test-alpha"], subjects["test-mid"]
			change(`INSERT INTO catalogue_audit (operator_label, action, subject_id, old_name_fr, new_name_fr, old_name_ar, new_name_ar)
				VALUES ('test-operator@test-host', 'rename_subject', $1, NULL, 'TEST Mathématiques', NULL, 'رياضيات')`, []any{zeta},
				`UPDATE subjects SET name_fr = 'TEST Mathématiques', name_ar = 'رياضيات' WHERE id = $1`, zeta)
			change(`INSERT INTO catalogue_audit (operator_label, action, subject_id, old_sort_order, new_sort_order)
				VALUES ('test-operator@test-host', 'reorder_subject', $1, 0, 7)`, []any{alpha},
				`UPDATE subjects SET sort_order = 7 WHERE id = $1`, alpha)
			var ch int64
			for id, name := range chapters {
				if name == "test-mid/ch-02" {
					ch = id
				}
			}
			change(`INSERT INTO catalogue_audit (operator_label, action, subject_id, chapter_id, old_name_fr, new_name_fr, old_name_ar, new_name_ar)
				VALUES ('test-operator@test-host', 'rename_chapter', $1, $2, NULL, 'TEST Suites', NULL, NULL)`, []any{mid, ch},
				`UPDATE chapters SET name_fr = 'TEST Suites' WHERE id = $1`, ch)
			change(`INSERT INTO catalogue_audit (operator_label, action, subject_id, chapter_id, old_sort_order, new_sort_order)
				VALUES ('test-operator@test-host', 'reorder_chapter', $1, $2, 0, 3)`, []any{mid, ch},
				`UPDATE chapters SET sort_order = 3 WHERE id = $1`, ch)
			snapshot := func() string {
				var s string
				if err := pool.QueryRow(ctx, `
					SELECT (SELECT string_agg(concat_ws(':', id, code, name_fr, name_ar, sort_order, created_at), ',' ORDER BY id) FROM subjects) || '|' ||
					       (SELECT string_agg(concat_ws(':', id, subject_id, code, name_fr, name_ar, sort_order, created_at), ',' ORDER BY id) FROM chapters) || '|' ||
					       (SELECT string_agg(concat_ws(':', id, action, subject_id, chapter_id, occurred_at, xact_id, operator_label,
					                         old_name_fr, new_name_fr, old_name_ar, new_name_ar, old_sort_order, new_sort_order), ',' ORDER BY id) FROM catalogue_audit)`).Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			before := snapshot()
			if !strings.Contains(before, "رياضيات") || !strings.Contains(before, "TEST Suites") || !strings.Contains(before, ":7:") {
				t.Fatalf("names and order not set before the upgrade: %s", before)
			}
			if _, err := database.Migrate(ctx, pool, migrations.FS); err != nil {
				t.Fatal(err)
			}
			if after := snapshot(); after != before {
				t.Fatalf("catalogue changed by the upgrade:\nbefore %s\nafter  %s", before, after)
			}
			var retired, placed, levels int
			if err := pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM subjects WHERE retired_at IS NOT NULL) + (SELECT count(*) FROM chapters WHERE retired_at IS NOT NULL),
				(SELECT count(*) FROM curriculum_assignment_chapters) + (SELECT count(*) FROM curriculum_assignments),
				(SELECT count(*) FROM education_levels) + (SELECT count(*) FROM curriculum_audit)`).Scan(&retired, &placed, &levels); err != nil {
				t.Fatal(err)
			}
			if retired+placed+levels != 0 {
				t.Fatalf("upgrade retired %d, placed %d, seeded %d", retired, placed, levels)
			}

			// The upgraded database works: levels list in byte order of their
			// codes at equal display order, whatever the locale.
			store := curriculum.NewStore(pool)
			for _, code := range []string{"test-b", "test-a-z", "test-a"} {
				if _, err := store.CreateLevel(ctx, testOperator, curriculum.Entry{Code: code, NameFR: "TEST " + code, NameAR: "اختبار"}); err != nil {
					t.Fatal(err)
				}
			}
			ls, err := store.Levels(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, l := range ls {
				got = append(got, l.Code)
			}
			if strings.Join(got, ",") != "test-a,test-a-z,test-b" {
				t.Fatalf("levels = %v", got)
			}
		})
	}
}
