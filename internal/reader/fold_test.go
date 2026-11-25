package reader_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// localeDatabase creates a fresh UTF-8 database whose collation and
// character classification use the libc locale lc, applies every
// migration, and drops it when the test ends. The test role needs
// CREATEDB (the CI service role is a superuser).
func localeDatabase(t *testing.T, lc string) *pgxpool.Pool {
	t.Helper()
	url := dbtest.URL(t)
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "learnoo_locale_" + hex.EncodeToString(b[:])
	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+
		" TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LC_COLLATE "+quote(lc)+" LC_CTYPE "+quote(lc))
	if err != nil {
		t.Fatalf("create %s database: %v", lc, err)
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
	t.Cleanup(pool.Close) // runs before the DROP above
	if _, err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	var ctype string
	if err := pool.QueryRow(ctx, `SELECT datctype FROM pg_database WHERE datname = current_database()`).Scan(&ctype); err != nil {
		t.Fatal(err)
	}
	if ctype != lc {
		t.Fatalf("database LC_CTYPE = %q, want %q", ctype, lc)
	}
	return pool
}

func quote(s string) string { return "'" + s + "'" }

// Reader search folding (reader_fold, migrations/0008) must give the same
// keys and matches whatever the database locale (issue #67). Each case runs
// in a C database, an en_US.UTF-8 database, and the configured test
// database (whose locale CI does not choose).
func TestReaderFoldIsLocaleIndependent(t *testing.T) {
	folds := map[string]string{
		"Œuvre":               "oeuvre",
		"ŒUVRE":               "oeuvre",
		"œuvre":               "oeuvre",
		"oeuvre":              "oeuvre",
		"OEUVRE":              "oeuvre",
		"Ægée":                "aegee",
		"élève":               "eleve",
		"ÉLÈVE":               "eleve",
		"Équations du second": "equations du second",
		"ÇA ÎLE Ÿ À Ô Ù":      "ca ile y a o u",
		"ØRESUND ÐÞ Ł Đ":      "øresund ðþ ł đ",
		"ΔΣ Ω ς":              "δσ ω σ",
		"الدَّوَالّ الأُسِّيَّة": "الدوال الاسية",
		"الدوال الاسية":          "الدوال الاسية",
		"الـــدوال":              "الدوال",
		// Capitals outside the explicit table stay as they are under every
		// locale (en_US.UTF-8 lower() alone would fold them, C would not).
		"Бета":     "Бета",
		"ẞ Straße": "ẞ straße",
	}
	// Each title must be found by every query listed for it, and by no
	// query listed for another title.
	searches := map[string][]string{
		"Œuvre complète de Molière": {"oeuvre", "OEUVRE", "œuvre", "Œuvre", "ŒUVRE", "complete", "MOLIERE"},
		"L'élève et le maître":      {"eleve", "élève", "ÉLÈVE", "ELEVE", "maitre", "MAÎTRE"},
		"Une oeuvre d'art":          {"oeuvre", "OEUVRE", "œuvre", "Œuvre", "ŒUVRE", "d'art"},
		"الدَّوَالّ الأُسِّيَّة":    {"الدوال", "الدَّوَالّ", "الاسية", "الأسية", "الأُسِّيَّة"},
	}
	dbs := map[string]func(*testing.T) *pgxpool.Pool{
		"C":           func(t *testing.T) *pgxpool.Pool { return localeDatabase(t, "C") },
		"en_US.UTF-8": func(t *testing.T) *pgxpool.Pool { return localeDatabase(t, "en_US.UTF-8") },
		"test database": func(t *testing.T) *pgxpool.Pool {
			return dbtest.Migrated(t)
		},
	}
	for name, open := range dbs {
		t.Run(name, func(t *testing.T) {
			db := open(t)
			ctx := context.Background()
			for in, want := range folds {
				var got string
				if err := db.QueryRow(ctx, `SELECT reader_fold($1)`, in).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("reader_fold(%q) = %q, want %q", in, got, want)
				}
			}
			for title, queries := range searches {
				for other, otherQueries := range searches {
					for _, q := range otherQueries {
						// The predicate of Store.Search (internal/reader/store.go).
						var found bool
						if err := db.QueryRow(ctx, `SELECT strpos(reader_fold($1), reader_fold($2)) > 0`, title, q).Scan(&found); err != nil {
							t.Fatal(err)
						}
						want := other == title || contains(queries, q)
						if found != want {
							t.Errorf("title %q, query %q: found = %v, want %v", title, q, found, want)
						}
					}
				}
			}
		})
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
