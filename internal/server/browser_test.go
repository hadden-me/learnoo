package server_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/server"
)

// TestBrowserReaderServer is a fixture server, not a test. It runs only when
// LEARNOO_BROWSER_READER_ADDR is set (by playwright.reader.config.mjs): it
// seeds TEST lessons into an isolated schema of the test database, writes a
// one-time student invitation token to LEARNOO_BROWSER_READER_TOKEN_FILE and
// serves the application on that loopback address until SIGTERM/SIGINT, then
// drops the schema. With LEARNOO_BROWSER_STUDIO_DIR (the built web/studio/dist)
// it also serves the Studio at /studio, as `serve` does with
// LEARNOO_STUDIO_DIR. No real accounts, lessons or credentials are involved.
func TestBrowserReaderServer(t *testing.T) {
	addr := os.Getenv("LEARNOO_BROWSER_READER_ADDR")
	if addr == "" {
		t.Skip("set LEARNOO_BROWSER_READER_ADDR to serve the Playwright reader fixture")
	}
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		t.Fatalf("LEARNOO_BROWSER_READER_ADDR %q must be a loopback address", addr)
	}
	tokenFile := os.Getenv("LEARNOO_BROWSER_READER_TOKEN_FILE")
	if tokenFile == "" {
		t.Fatal("LEARNOO_BROWSER_READER_TOKEN_FILE is not set")
	}
	var opts []server.Option
	if dir := os.Getenv("LEARNOO_BROWSER_STUDIO_DIR"); dir != "" {
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatalf("LEARNOO_BROWSER_STUDIO_DIR: %v (build it: cd web/studio && npm run build)", err)
		}
		defer root.Close()
		if _, err := root.Stat("index.html"); err != nil {
			t.Fatalf("LEARNOO_BROWSER_STUDIO_DIR has no index.html (build it: cd web/studio && npm run build): %v", err)
		}
		opts = append(opts, server.WithStudio(root.FS()))
	}
	f := newReaderOptions(t, nil, opts...)
	// A second TEST author, for the Studio's account-switch test (another
	// account signing in on the same browser must never receive a stale tab's writes).
	second := f.newUser("test-author-two", f.subject, permissions.Author)
	f.setName(second.id, "TEST Auteur Deux")
	// Studio accounts for the editorial journey (tests/browser-reader/
	// editorial.spec.mjs), all simulated and labelled TEST, with the test
	// password: test-author, test-reviewer (reviewer of test-math),
	// test-publisher and test-admin come from newReaderOptions; added here
	// are a second reviewer of test-math, a reviewer of another subject only,
	// and an author who also reviews test-math (self-review is refused).
	f.setName(f.publisher.id, "TEST Éditrice Sonia")
	f.setName(f.admin.id, "TEST Admin Lina")
	other := f.newSubject("test-physique")
	f.setName(f.newUser("test-reviewer-b", f.subject, permissions.Reviewer).id, "TEST Relectrice Nadia")
	f.setName(f.newUser("test-reviewer-physique", other, permissions.Reviewer).id, "TEST Relecteur Physique")
	f.setName(f.newUser("test-author-reviewer", f.subject, permissions.Author, permissions.Reviewer).id, "TEST Autrice relectrice")
	ch := f.chapter(f.subject, "test-chapter")
	f.publish(f.approved(0, richDoc("TEST Équations et nombres complexes"), "fr", &ch))
	f.publish(f.approved(0, readerDoc("TEST درس تجريبي", "ar",
		para(text("نص تجريبي يحتوي على صيغة "), map[string]any{"type": "math", "text": `\sqrt{a^2 + b^2}`}),
		map[string]any{"type": "displayMath", "text": `\lim_{n \to +\infty} \left(1 + \frac{1}{n}\right)^n = e`},
	), "ar", &ch))
	token, _, err := f.inv.Issue(context.Background(), "browser-student@example.test", "test-operator@test-host", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(tokenFile) })

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	srv := &http.Server{Addr: addr, Handler: f.h, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}
}
