package studioweb

import (
	"os"
	"regexp"
	"testing"
)

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
