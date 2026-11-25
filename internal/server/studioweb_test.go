package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/server"
	"github.com/hadden-me/learnoo/internal/studioweb"
)

// A stand-in for the Vite build (web/studio/dist) with a symbolic link and
// a secret outside the bundle directory.
const studioIndex = `<!doctype html><html lang="fr"><head><script type="module" crossorigin src="/studio/assets/index-TEST.js"></script></head><body><div id="root"></div></body></html>`

func studioBundle(t *testing.T) *os.Root {
	t.Helper()
	base := t.TempDir()
	dist := filepath.Join(base, "dist")
	write := func(name, data string) {
		t.Helper()
		p := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("dist/index.html", studioIndex)
	write("dist/assets/index-TEST.js", `console.log("TEST studio")`)
	write("dist/assets/index-TEST.css", `body{color:black}`)
	write("dist/.hidden.txt", "TEST hidden")
	write("dist/assets/sub.d/x.txt", "TEST nested")
	write("secret.txt", "TEST SECRET outside the bundle")
	if err := os.Symlink(filepath.Join(base, "secret.txt"), filepath.Join(dist, "assets", "leak.js")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dist)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func newStudioReader(t *testing.T) readerFixture {
	return newReaderOptions(t, nil, server.WithStudio(studioBundle(t).FS()))
}

func (f readerFixture) studio(u apiUser, method, path string) *httptest.ResponseRecorder {
	f.t.Helper()
	w := f.page(u, method, path, nil)
	h := w.Header()
	if cc := h.Get("Cache-Control"); cc != studioweb.CacheControl {
		f.t.Errorf("%s %s: Cache-Control %q", method, path, cc)
	}
	if v := h.Get("Vary"); v != "Cookie" {
		f.t.Errorf("%s %s: Vary %q", method, path, v)
	}
	if csp := h.Get("Content-Security-Policy"); csp != studioweb.CSP {
		f.t.Errorf("%s %s: CSP %q", method, path, csp)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" {
		f.t.Errorf("%s %s: missing nosniff or frame denial: %v", method, path, h)
	}
	if strings.Contains(w.Body.String(), "SECRET") || strings.Contains(w.Body.String(), "TEST hidden") {
		f.t.Fatalf("%s %s leaked a file outside the bundle: %s", method, path, w.Body)
	}
	return w
}

func TestStudioCSPIsStrict(t *testing.T) {
	for _, want := range []string{"default-src 'none'", "script-src 'self';", "frame-ancestors 'none'", "base-uri 'none'", "connect-src 'self'"} {
		if !strings.Contains(studioweb.CSP, want) {
			t.Errorf("CSP lacks %q", want)
		}
	}
	for _, bad := range []string{"'unsafe-eval'", "script-src 'self' 'unsafe-inline'", "style-src 'self' 'unsafe-inline'", "*", "data:", "http:"} {
		if strings.Contains(studioweb.CSP, bad) {
			t.Errorf("CSP contains %q", bad)
		}
	}
}

func TestStudioRequiresASession(t *testing.T) {
	f := newStudioReader(t)
	for path, next := range map[string]string{
		"/studio":            "/studio",
		"/studio/":           "/studio",
		"/studio/lessons/12": "/studio/lessons/12",
		"/studio/lessons/x/": "/studio",
	} {
		w := f.studio(apiUser{}, http.MethodGet, path)
		wantRedirect(t, "anonymous "+path, w, "/login?next="+url.QueryEscape(next))
	}
	for _, path := range []string{"/studio/index.html", "/studio/assets/index-TEST.js", "/studio/assets/missing.js"} {
		wantStatus(t, "anonymous "+path, f.studio(apiUser{}, http.MethodGet, path), http.StatusNotFound, "not_found")
	}
	// Signing in through the reader's form returns to the Studio.
	f.newUser("test-author-login", f.subject, permissions.Author)
	w := f.page(apiUser{}, http.MethodPost, "/login", url.Values{
		"email": {"test-author-login@example.test"}, "password": {testPassword}, "next": {"/studio/lessons/12"},
	}, withAddr("203.0.113.40"))
	wantRedirect(t, "login with next", w, "/studio/lessons/12")
	w = f.page(apiUser{}, http.MethodGet, "/login?next="+url.QueryEscape("/studio"), nil)
	wantPage(t, "login page keeps next", w, http.StatusOK, `value="/studio"`)
}

func TestStudioDeniesUsersWithoutStaffAccess(t *testing.T) {
	f := newStudioReader(t)
	nobody := f.newUser("test-no-permission", 0)
	for name, u := range map[string]apiUser{"student": f.student, "teacher without permissions": nobody} {
		w := f.studio(u, http.MethodGet, "/studio")
		wantPage(t, name, w, http.StatusForbidden, "Accès réservé")
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type %q", name, ct)
		}
		wantAbsent(t, name, w, "index-TEST.js")
		wantStatus(t, name+" asset", f.studio(u, http.MethodGet, "/studio/assets/index-TEST.js"), http.StatusNotFound, "not_found")
	}
	// Revoking the last permission closes the Studio on the next request.
	ps := permissions.NewStore(f.db)
	if _, err := ps.Revoke(t.Context(), testOperator, f.author.id, permissions.Author); err != nil {
		t.Fatal(err)
	}
	wantPage(t, "revoked author", f.studio(f.author, http.MethodGet, "/studio"), http.StatusForbidden)
}

func TestStudioServesTheBundleToStaff(t *testing.T) {
	f := newStudioReader(t)
	for name, u := range map[string]apiUser{"author": f.author, "reviewer": f.reviewer, "publisher": f.publisher, "admin": f.admin} {
		w := f.studio(u, http.MethodGet, "/studio")
		if w.Code != http.StatusOK || w.Body.String() != studioIndex {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", name, ct)
		}
		if w.Header().Get("Last-Modified") != "" || w.Header().Get("ETag") != "" {
			t.Errorf("%s: validators sent: %v", name, w.Header())
		}
	}
	a := f.author
	// SPA fallback: client routes get index.html.
	for _, path := range []string{"/studio/", "/studio/lessons", "/studio/lessons/12/edit", "/studio/assets"} {
		w := f.studio(a, http.MethodGet, path)
		if w.Code != http.StatusOK || w.Body.String() != studioIndex {
			t.Errorf("fallback %s: %d %s", path, w.Code, w.Body)
		}
	}
	w := f.studio(a, http.MethodGet, "/studio/assets/index-TEST.js")
	if w.Code != http.StatusOK || w.Body.String() != `console.log("TEST studio")` || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("script: %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if w := f.studio(a, http.MethodGet, "/studio/assets/index-TEST.css"); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("stylesheet: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := f.studio(a, http.MethodHead, "/studio/assets/index-TEST.js"); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %q", w.Code, w.Body)
	}
	// Unknown files, hidden files, directories and paths leaving the bundle
	// (including a symbolic link inside it) are 404s.
	for _, path := range []string{
		"/studio/assets/missing.js", "/studio/.hidden.txt", "/studio/assets/sub.d",
		"/studio/assets/leak.js", "/studio/assets/..%2f..%2fsecret.txt", "/studio/%2e%2e/secret.txt",
		"/studio/assets%2f..%2f..%2fsecret.txt", `/studio/assets\..\..\secret.txt`,
	} {
		w := f.page(a, http.MethodGet, path, nil)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMovedPermanently {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "SECRET") || strings.Contains(w.Body.String(), "TEST hidden") {
			t.Errorf("%s leaked: %s", path, w.Body)
		}
	}
	// A plain ".." is cleaned by the router into a redirect, never served.
	if w := f.page(a, http.MethodGet, "/studio/../secret.txt", nil); w.Code == http.StatusOK {
		t.Errorf("dot-dot path served: %s", w.Body)
	}
	if w := f.page(a, http.MethodPost, "/studio", nil); w.Code == http.StatusOK {
		t.Errorf("POST /studio: %d", w.Code)
	}
}

func TestStudioIsNotRoutedWithoutABundle(t *testing.T) {
	f := newReader(t)
	if w := f.page(f.author, http.MethodGet, "/studio", nil); w.Code != http.StatusNotFound {
		t.Fatalf("GET /studio without a bundle: %d", w.Code)
	}
}
