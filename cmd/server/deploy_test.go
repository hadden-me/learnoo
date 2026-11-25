package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Container-related configuration (LRN-010).

func TestNonLoopbackNeedsExplicitOptIn(t *testing.T) {
	t.Setenv("LEARNOO_DATABASE_URL", "")
	t.Setenv("LEARNOO_MEDIA_DIR", "")
	t.Setenv("LEARNOO_ADDR", "0.0.0.0:8090")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for value, want := range map[string]string{
		"":     "not a loopback address",
		"0":    "not a loopback address",
		"true": "not a loopback address",
		"1":    "LEARNOO_MEDIA_DIR is not set", // passed the address check
	} {
		t.Setenv("LEARNOO_ALLOW_NON_LOOPBACK", value)
		err := run([]string{"serve"}, strings.NewReader(""), io.Discard, log)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LEARNOO_ALLOW_NON_LOOPBACK=%q: got %v, want %q", value, err, want)
		}
	}
}

// schemaURL points the commands at a fresh schema through search_path.
func schemaURL(t *testing.T, migrated bool) string {
	t.Helper()
	var pool *pgxpool.Pool
	if migrated {
		pool = dbtest.Migrated(t)
	} else {
		pool = dbtest.EmptySchema(t)
	}
	var schema string
	if err := pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	url := dbtest.URL(t)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "search_path=" + schema
}

func TestDatabasePasswordFile(t *testing.T) {
	t.Setenv("LEARNOO_DATABASE_URL", schemaURL(t, true))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	newline := filepath.Join(dir, "newline")
	good := filepath.Join(dir, "good")
	for name, body := range map[string]string{empty: "", newline: "\n", good: "simulated-db-password\n"} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for file, want := range map[string]string{
		filepath.Join(dir, "missing"): "LEARNOO_DATABASE_PASSWORD_FILE",
		empty:                         "is empty",
		newline:                       "is empty",
	} {
		t.Setenv("LEARNOO_DATABASE_PASSWORD_FILE", file)
		err := run([]string{"migrate", "-check"}, nil, io.Discard, log)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", filepath.Base(file), err, want)
		}
		if err != nil && strings.Contains(err.Error(), "simulated-db-password") {
			t.Fatal("error echoes the password")
		}
	}
	// The test server uses trust authentication, so this only shows the
	// file is accepted; password checking is PostgreSQL's.
	t.Setenv("LEARNOO_DATABASE_PASSWORD_FILE", good)
	if err := run([]string{"migrate", "-check"}, nil, io.Discard, log); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRestoreCommand(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("LEARNOO_DATABASE_PASSWORD_FILE", "")
	t.Setenv("LEARNOO_DATABASE_URL", schemaURL(t, true))
	media := t.TempDir()
	t.Setenv("LEARNOO_MEDIA_DIR", media)
	var out bytes.Buffer
	if err := run([]string{"verify-restore"}, nil, &out, log); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "verification passed") {
		t.Fatalf("output:\n%s", out.String())
	}

	if err := os.WriteFile(filepath.Join(media, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"verify-restore"}, nil, &out, log); err != nil {
		t.Fatalf("unexpected file must only be reported: %v", err)
	}
	if !strings.Contains(out.String(), "unexpected: stray") {
		t.Fatalf("output:\n%s", out.String())
	}
	if err := run([]string{"verify-restore", "-strict"}, nil, io.Discard, log); err == nil {
		t.Fatal("strict mode passed with an unexpected file")
	}

	t.Setenv("LEARNOO_MEDIA_DIR", filepath.Join(media, "absent"))
	if err := run([]string{"verify-restore"}, nil, io.Discard, log); err == nil || !strings.Contains(err.Error(), "not an existing directory") {
		t.Fatalf("missing media dir: %v", err)
	}

	t.Setenv("LEARNOO_MEDIA_DIR", media)
	t.Setenv("LEARNOO_DATABASE_URL", schemaURL(t, false))
	out.Reset()
	if err := run([]string{"verify-restore"}, nil, &out, log); err == nil || !strings.Contains(out.String(), "pending migrations") {
		t.Fatalf("unmigrated database: %v\n%s", err, out.String())
	}
}

func TestProbe(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" && r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	t.Setenv("LEARNOO_ADDR", "0.0.0.0:"+port) // wildcard is probed on loopback
	for _, args := range [][]string{{"probe"}, {"probe", "-path", "/healthz"}} {
		if err := run(args, nil, io.Discard, log); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	status.Store(http.StatusServiceUnavailable)
	if err := run([]string{"probe"}, nil, io.Discard, log); err == nil {
		t.Fatal("probe passed on 503")
	}
	if err := run([]string{"probe", "-path", "/login"}, nil, io.Discard, log); err == nil {
		t.Fatal("probe accepted another path")
	}
}
