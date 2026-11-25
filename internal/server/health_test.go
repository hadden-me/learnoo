package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/server"
)

// Liveness and readiness probes (LRN-010).
func TestHealthAndReadiness(t *testing.T) {
	ctx := context.Background()
	migrated := dbtest.Migrated(t)
	// A second pool on the same schema that the test can close.
	cfg := migrated.Config().Copy()
	pool, err := database.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	svc, err := auth.New(pool, auth.DefaultConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ms, err := media.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()
	h := server.New(pool, svc, ms, slog.New(slog.NewTextHandler(io.Discard, nil)))
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://learnoo.test"+path, nil))
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control %q", path, cc)
		}
		return w
	}
	wantOK := func(path string) {
		t.Helper()
		if w := get(path); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"status":"ok"}` {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	wantUnavailable := func(path string) {
		t.Helper()
		w := get(path)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		body := w.Body.String()
		for _, leak := range []string{"database", "media", dir, "permission", "closed"} {
			if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
				t.Fatalf("%s reveals %q: %s", path, leak, body)
			}
		}
	}
	wantOK("/healthz")
	wantOK("/readyz")
	if n := countTempFiles(t, dir); n != 0 {
		t.Fatalf("readiness probe left %d files", n)
	}

	if os.Geteuid() != 0 { // root ignores directory permissions
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		wantUnavailable("/readyz")
		wantOK("/healthz")
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		wantOK("/readyz")
	}

	pool.Close()
	wantUnavailable("/readyz")
	wantOK("/healthz") // liveness does not depend on the database
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "http://learnoo.test/readyz", nil))
	if w.Code == http.StatusOK {
		t.Fatal("POST /readyz accepted")
	}
}

func countTempFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
