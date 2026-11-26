package studioweb

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestFingerprintedNames(t *testing.T) {
	for name, want := range map[string]bool{
		"assets/index-Tq3x_9Ab.js":         true,
		"assets/index-BkLy2-0c.css":        true,
		"assets/KaTeX_Main-Ab12cd34.woff2": true,
		"assets/index-TEST.js":             false, // hash too short
		"assets/index.js":                  false,
		"index.html":                       false,
		"assets/sub/index-Tq3x_9Ab.js":     false,
		"other/index-Tq3x_9Ab.js":          false,
		"assets/index-Tq3x_9Ab":            false,
	} {
		if got := fingerprinted.MatchString(name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

// Every file Vite emits under assets/ is fingerprinted, so none is served
// with no-store by accident (and none named otherwise would be cached).
func TestBuiltAssetsAreFingerprinted(t *testing.T) {
	names, err := filepath.Glob("../../web/studio/dist/assets/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Skip("web/studio/dist is not built")
	}
	for _, name := range names {
		if rel := "assets/" + filepath.Base(name); !fingerprinted.MatchString(rel) {
			t.Errorf("%s is not named like a fingerprinted file", rel)
		}
	}
}

// The built bundle must work under CSP: no inline script or event handler,
// and every asset under the /studio/ base. Skipped until the bundle is built
// (cd web/studio && npm run build).
func TestBuiltIndexFitsTheCSP(t *testing.T) {
	data, err := os.ReadFile("../../web/studio/dist/index.html")
	if os.IsNotExist(err) {
		t.Skip("web/studio/dist is not built")
	}
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	scripts := regexp.MustCompile(`<script\b[^>]*>`).FindAllString(html, -1)
	if len(scripts) == 0 {
		t.Fatal("no script tag")
	}
	external := regexp.MustCompile(`^<script type="module" crossorigin src="/studio/assets/[A-Za-z0-9._-]+\.js"></script>$`)
	for _, m := range regexp.MustCompile(`<script\b[^>]*>[^<]*</script>`).FindAllString(html, -1) {
		if !external.MatchString(m) {
			t.Errorf("script is not an external module under /studio/assets/: %s", m)
		}
	}
	if regexp.MustCompile(`(?i)\son[a-z]+=|\sstyle=|<style\b|javascript:`).MatchString(html) {
		t.Error("index.html has an inline handler, style or javascript: URL")
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if !regexp.MustCompile(`^/studio/assets/`).MatchString(m[1]) {
			t.Errorf("asset outside /studio/assets/: %s", m[1])
		}
	}
}

func TestWithMeta(t *testing.T) {
	h := &Handler{Config{Meta: map[string]string{"b": `/x?a=1&b="2"`, "a": "/y"}}}
	got := string(h.withMeta([]byte(`<html><head><title>t</title></head><body></body></html>`)))
	want := `<html><head><title>t</title><meta name="a" content="/y"><meta name="b" content="/x?a=1&amp;b=&#34;2&#34;"></head><body></body></html>`
	if got != want {
		t.Fatalf("got %s", got)
	}
	if got := string(h.withMeta([]byte("no head"))); got != "no head" {
		t.Fatalf("without </head>: %s", got)
	}
}
