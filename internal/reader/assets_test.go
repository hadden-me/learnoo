package reader

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testStatic(katexCSS string) (fstest.MapFS, fstest.MapFS) {
	readerFS := fstest.MapFS{"reader.css": {Data: []byte("main{}")}}
	renderingFS := fstest.MapFS{
		"tokens.css":                          {Data: []byte(":root{}")},
		"site.css":                            {Data: []byte("body{}")},
		"math.js":                             {Data: []byte("(()=>{})()")},
		"landing.css":                         {Data: []byte("not public")},
		"complex-plane.svg":                   {Data: []byte("<svg/>")},
		"vendor/katex/katex.min.css":          {Data: []byte(katexCSS)},
		"vendor/katex/katex.min.js":           {Data: []byte("katex")},
		"vendor/katex/fonts/KaTeX_Main.woff2": {Data: []byte("woff2")},
		"vendor/katex/fonts/KaTeX_Main.ttf":   {Data: []byte("ttf")},
		"vendor/katex/LICENSE":                {Data: []byte("MIT")},
	}
	return readerFS, renderingFS
}

func hash16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func TestManifestVersions(t *testing.T) {
	m, err := buildManifest(testStatic(`.katex{src:url(fonts/KaTeX_Main.woff2)}`))
	if err != nil {
		t.Fatal(err)
	}
	// Single files: their own digest.
	for name, content := range map[string]string{"reader.css": "main{}", "site.css": "body{}", "tokens.css": ":root{}", "math.js": "(()=>{})()"} {
		if got := m[name].version; got != hash16(content) {
			t.Errorf("%s: version %q, want %q", name, got, hash16(content))
		}
		if got, _ := m.url(name); got != "/assets/"+hash16(content)+"/"+name {
			t.Errorf("%s: url %q", name, got)
		}
	}
	// KaTeX: one version for the directory, so the stylesheet's relative
	// font URLs resolve under it.
	css, _ := m.url("vendor/katex/katex.min.css")
	font, _ := m.url("vendor/katex/fonts/KaTeX_Main.woff2")
	v := m["vendor/katex/katex.min.css"].version
	if !versionPattern.MatchString(v) || css != "/assets/"+v+"/vendor/katex/katex.min.css" || font != "/assets/"+v+"/vendor/katex/fonts/KaTeX_Main.woff2" {
		t.Fatalf("katex urls %q %q", css, font)
	}
	for _, name := range []string{"vendor/katex/katex.min.js", "vendor/katex/fonts/KaTeX_Main.ttf"} {
		if m[name].version != v {
			t.Errorf("%s: version %q, want the directory's %q", name, m[name].version, v)
		}
	}
	if m["vendor/katex/fonts/KaTeX_Main.woff2"].ctype != "font/woff2" || m["site.css"].ctype != "text/css; charset=utf-8" || m["math.js"].ctype != "text/javascript; charset=utf-8" {
		t.Error("content types")
	}
	// Not fingerprinted: lesson media, renderer-only files, the license.
	for _, name := range []string{"complex-plane.svg", "landing.css", "vendor/katex/LICENSE"} {
		if _, ok := m[name]; ok {
			t.Errorf("%s is fingerprinted", name)
		}
	}
	if _, err := m.url("complex-plane.svg"); err == nil {
		t.Error("url of lesson media")
	}

	// Changing any KaTeX file changes the directory's version, and only it.
	m2, err := buildManifest(testStatic(`.katex{src:url(fonts/KaTeX_Main.woff2)} `))
	if err != nil {
		t.Fatal(err)
	}
	if m2["vendor/katex/fonts/KaTeX_Main.woff2"].version == v {
		t.Error("katex version unchanged after a change")
	}
	if m2["site.css"].version != m["site.css"].version {
		t.Error("site.css version changed")
	}
}

func TestManifestWithoutKatex(t *testing.T) {
	readerFS, renderingFS := testStatic("")
	for name := range renderingFS {
		if strings.HasPrefix(name, "vendor/") {
			delete(renderingFS, name)
		}
	}
	m, err := buildManifest(readerFS, renderingFS)
	if err != nil {
		t.Fatal(err)
	}
	// Not copied: pages fall back to the unversioned URL (404, as before).
	if u, err := m.url("vendor/katex/katex.min.js"); err != nil || u != "/static/vendor/katex/katex.min.js" {
		t.Fatalf("%q %v", u, err)
	}
	delete(renderingFS, "site.css")
	if _, err := buildManifest(readerFS, renderingFS); err == nil {
		t.Fatal("missing site.css accepted")
	}
}

// The embedded build has the required assets.
func TestEmbeddedManifest(t *testing.T) {
	m, err := embeddedAssets()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reader.css", "tokens.css", "site.css", "math.js"} {
		if _, ok := m[name]; !ok {
			t.Errorf("%s missing", name)
		}
	}
}

func TestServeAsset(t *testing.T) {
	m, err := buildManifest(testStatic(`.katex{}`))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{assets: m}
	mux := http.NewServeMux()
	mux.Handle("GET "+AssetPrefix+"{version}/{path...}", http.HandlerFunc(h.serveAsset))
	get := func(method, path string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		w.Header().Set("Vary", "Cookie") // as internal/server sets it
		mux.ServeHTTP(w, r)
		return w
	}
	css, _ := m.url("site.css")
	font, _ := m.url("vendor/katex/fonts/KaTeX_Main.woff2")
	for _, tc := range []struct{ url, ctype, body string }{{css, "text/css; charset=utf-8", "body{}"}, {font, "font/woff2", "woff2"}} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := get(method, tc.url)
			if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != AssetCacheControl || w.Header().Get("Content-Type") != tc.ctype ||
				w.Header().Get("Vary") != "" || w.Header().Get("Set-Cookie") != "" || w.Header().Get("ETag") == "" {
				t.Errorf("%s %s: %d %v", method, tc.url, w.Code, w.Header())
			}
			if method == http.MethodGet && w.Body.String() != tc.body {
				t.Errorf("%s: body %q", tc.url, w.Body)
			}
		}
	}
	if w := get(http.MethodGet, css, "If-None-Match", m["site.css"].etag); w.Code != http.StatusNotModified {
		t.Errorf("revalidation: %d", w.Code)
	}
	old := "/assets/" + hash16("body{old}") + "/site.css"
	other := "/assets/" + m["reader.css"].version + "/site.css"
	for _, path := range []string{old, other, "/assets/" + m["site.css"].version + "/landing.css",
		"/assets/" + strings.ToUpper(m["site.css"].version) + "/site.css", "/assets/" + m["site.css"].version + "x/site.css",
		"/assets/" + m["site.css"].version + "/complex-plane.svg", "/assets/" + m["site.css"].version + "/",
		"/assets/" + m["vendor/katex/katex.min.css"].version + "/vendor/katex/LICENSE",
		"/assets/" + m["vendor/katex/katex.min.css"].version + "/vendor/katex/fonts"} {
		w := get(http.MethodGet, path)
		if w.Code != http.StatusNotFound || w.Header().Get("Cache-Control") == AssetCacheControl {
			t.Errorf("%s: %d %q", path, w.Code, w.Header().Get("Cache-Control"))
		}
	}
}
