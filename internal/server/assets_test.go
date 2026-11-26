package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/reader"
)

// Fingerprinted public assets (issue #88): cacheable by anyone, identical
// for every requester; everything that depends on the session stays
// private, no-store.

var assetRef = regexp.MustCompile(`(?:href|src)="(/(?:assets|static)/[^"]+)"`)

// assetRefs returns the stylesheet and script URLs a page references.
func assetRefs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var refs []string
	for _, m := range assetRef.FindAllStringSubmatch(w.Body.String(), -1) {
		if !strings.HasSuffix(m[1], ".svg") { // lesson media, not an asset
			refs = append(refs, m[1])
		}
	}
	return refs
}

// wantPublicAsset checks a fingerprinted asset response.
func wantPublicAsset(t *testing.T, what string, w *httptest.ResponseRecorder, ctype string) {
	t.Helper()
	h := w.Header()
	if w.Code != http.StatusOK || h.Get("Cache-Control") != reader.AssetCacheControl || h.Get("Content-Type") != ctype ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Vary") != "" || h.Get("Set-Cookie") != "" || h.Get("ETag") == "" {
		t.Fatalf("%s: %d %v", what, w.Code, h)
	}
}

// wantPrivate checks a response that must never be stored.
func wantPrivate(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	cc := w.Header().Get("Cache-Control")
	if !strings.Contains(cc, "no-store") || strings.Contains(cc, "public") || strings.Contains(cc, "max-age") {
		t.Errorf("%s: Cache-Control %q", what, cc)
	}
	if w.Header().Get("Vary") != "Cookie" {
		t.Errorf("%s: Vary %q", what, w.Header().Get("Vary"))
	}
}

func TestPagesReferenceFingerprintedAssets(t *testing.T) {
	f := newReader(t)
	p := f.publishNew("TEST Leçon d’actifs", "fr", nil)
	katex := strings.HasPrefix(mustAssetURL(t, "vendor/katex/katex.min.css"), reader.AssetPrefix)
	if !katex {
		t.Log("KaTeX is not copied (node scripts/copy-katex.mjs): its fingerprinted URLs are not checked")
	}
	pages := map[string]*httptest.ResponseRecorder{
		"login":  f.get(apiUser{}, "/login"),
		"lesson": f.get(f.student, lessonPath(p.publicID)),
	}
	want := map[string][]string{
		"login":  {"tokens.css", "site.css", "reader.css"},
		"lesson": {"tokens.css", "site.css", "reader.css", "vendor/katex/katex.min.css", "vendor/katex/katex.min.js", "math.js"},
	}
	for name, w := range pages {
		refs := assetRefs(t, w)
		var expected []string
		for _, asset := range want[name] {
			expected = append(expected, mustAssetURL(t, asset))
		}
		if strings.Join(refs, " ") != strings.Join(expected, " ") {
			t.Fatalf("%s references %v, want %v", name, refs, expected)
		}
		for _, ref := range refs {
			if !katex && strings.Contains(ref, "/vendor/katex/") {
				continue
			}
			if !strings.HasPrefix(ref, reader.AssetPrefix) {
				t.Errorf("%s: unversioned %s", name, ref)
			}
		}
	}

	// Each asset is the same for anyone: signed out, two students, a
	// forged cookie. No session is read, so none is cleared.
	forged := apiUser{cookie: strings.Repeat("A", 43)}
	second := f.invitedStudent("test-student-two@example.test", "TEST Élève Deux")
	for _, ref := range assetRefs(t, pages["lesson"]) {
		if !strings.HasPrefix(ref, reader.AssetPrefix) {
			continue
		}
		ctype := "text/css; charset=utf-8"
		if strings.HasSuffix(ref, ".js") {
			ctype = "text/javascript; charset=utf-8"
		}
		var body string
		for i, u := range []apiUser{{}, f.student, second, forged} {
			w := f.page(u, http.MethodGet, ref, nil)
			wantPublicAsset(t, ref, w, ctype)
			if i == 0 {
				body = w.Body.String()
			} else if w.Body.String() != body {
				t.Fatalf("%s differs between requesters", ref)
			}
		}
		if h := f.page(apiUser{}, http.MethodHead, ref, nil); h.Code != http.StatusOK || h.Body.Len() != 0 || h.Header().Get("Cache-Control") != reader.AssetCacheControl {
			t.Errorf("HEAD %s: %d", ref, h.Code)
		}
		// Another version of the same name (an older build) is 404, not
		// cacheable and not redirected.
		version := strings.SplitN(strings.TrimPrefix(ref, reader.AssetPrefix), "/", 2)[0]
		for _, other := range []string{"0000000000000000", "ffffffffffffffff", strings.ToUpper(version), version[:15]} {
			if other == version {
				continue
			}
			w := f.page(apiUser{}, http.MethodGet, strings.Replace(ref, version, other, 1), nil)
			wantStatus(t, "old version of "+ref, w, http.StatusNotFound, "not_found")
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("old version of %s: Cache-Control %q", ref, cc)
			}
		}
	}

	// KaTeX's stylesheet loads its fonts by relative URL: they resolve
	// under the same version.
	if katex {
		css := mustAssetURL(t, "vendor/katex/katex.min.css")
		w := f.page(apiUser{}, http.MethodGet, css, nil)
		fonts := regexp.MustCompile(`url\((fonts/[^)]+\.woff2)\)`).FindAllStringSubmatch(w.Body.String(), -1)
		if len(fonts) == 0 {
			t.Fatal("no font in katex.min.css")
		}
		base, _ := url.Parse("http://learnoo.test" + css)
		for _, m := range fonts {
			ref, _ := url.Parse(m[1])
			wantPublicAsset(t, m[1], f.page(apiUser{}, http.MethodGet, base.ResolveReference(ref).Path, nil), "font/woff2")
		}
	}

	// Lesson media, unknown names and non-public files are never served
	// under /assets/, whatever the version.
	v := strings.SplitN(strings.TrimPrefix(mustAssetURL(t, "site.css"), reader.AssetPrefix), "/", 2)[0]
	for _, path := range []string{"/assets/" + v + "/complex-plane.svg", "/assets/" + v + "/landing.html", "/assets/" + v + "/unknown.css",
		"/assets/" + v + "/../go.mod", "/assets/" + v + "/%2e%2e/page.html", "/assets/" + v, "/assets/", "/assets/" + v + "/vendor/katex/LICENSE"} {
		for _, u := range []apiUser{{}, f.student} {
			if w := f.page(u, http.MethodGet, path, nil); w.Code == http.StatusOK || w.Header().Get("Cache-Control") == reader.AssetCacheControl {
				t.Errorf("%s: %d %q", path, w.Code, w.Header().Get("Cache-Control"))
			}
		}
	}
}

// mustAssetURL is the URL pages use for the public asset name.
func mustAssetURL(t *testing.T, name string) string {
	t.Helper()
	u, err := reader.New(reader.Config{}).AssetURL(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Pages, APIs, previews and media stay private, no-store for every
// requester, and one student's page is never another's. The legacy
// unversioned /static/ URLs keep working, uncached.
func TestAuthenticatedResponsesStayPrivate(t *testing.T) {
	f := newReader(t)
	p := f.publishNew("TEST Leçon privée", "fr", nil)
	second := f.invitedStudent("test-student-two@example.test", "TEST Élève Deux")
	users := map[string]apiUser{"signed out": {}, "student": f.student, "second student": second}
	paths := []string{"/login", "/learn", "/learn/search?q=TEST", "/learn/subjects/test-math", lessonPath(p.publicID),
		lessonPath(p.publicID) + "/media/1", "/static/site.css", "/static/vendor/katex/katex.min.css", "/static/complex-plane.svg"}
	bodies := map[string]string{}
	for who, u := range users {
		for _, path := range paths {
			w := f.get(u, path)
			wantPrivate(t, who+" "+path, w)
			if path == "/learn" {
				bodies[who] = w.Body.String()
			}
		}
		wantPrivate(t, who+" session API", f.api(u, http.MethodGet, "/api/auth/session", nil))
	}
	if !strings.Contains(bodies["student"], "TEST Élève") || strings.Contains(bodies["student"], "TEST Élève Deux") || !strings.Contains(bodies["second student"], "TEST Élève Deux") {
		t.Fatal("dashboards do not belong to their own student")
	}
	if w := f.get(apiUser{}, "/static/site.css"); w.Code != http.StatusOK || w.Header().Get("Cache-Control") != reader.CacheControl {
		t.Errorf("legacy /static/site.css: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	for _, u := range []apiUser{f.author, f.reviewer} {
		w := f.api(u, http.MethodPost, "/api/preview", map[string]json.RawMessage{"content": readerDoc("TEST Aperçu privé", "fr")})
		if w.Code != http.StatusOK {
			t.Fatalf("preview: %d", w.Code)
		}
		wantPrivate(t, "preview", w)
	}
	wantPrivate(t, "anonymous preview", f.api(apiUser{}, http.MethodPost, "/api/preview", map[string]json.RawMessage{"content": readerDoc("TEST Aperçu", "fr")}))
}
