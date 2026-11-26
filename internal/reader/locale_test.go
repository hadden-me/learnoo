package reader

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/i18n"
)

var (
	// templateKey finds {{t "key"}} and {{tn "key" n}} in templates.
	templateKey = regexp.MustCompile(`\{\{-?\s*\(?\s*tn?\s+"([^"]+)"`)
	nestedKey   = regexp.MustCompile(`\(tn?\s+"([^"]+)"`)
	// goKey finds string literals shaped like message keys in Go sources.
	goKey = regexp.MustCompile(`"([a-z]+\.[A-Za-z0-9_.]+)"`)
)

// messageSources returns the template and Go sources that use the server
// catalog: this package and the Studio shell (internal/studioweb).
func messageSources(t *testing.T) (templates, goFiles map[string]string) {
	t.Helper()
	templates, goFiles = map[string]string{}, map[string]string{}
	entries, err := fs.ReadDir(files, "templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := fs.ReadFile(files, "templates/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		templates[e.Name()] = string(b)
	}
	for _, dir := range []string{".", "../studioweb"} {
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			goFiles[name] = string(b)
		}
	}
	return templates, goFiles
}

// TestMessageKeysUsed is the static check of key use: every key a template
// or a handler names exists in the catalog (in both locales, by
// i18n.TestCatalogParity), and every catalog key is used somewhere, so the
// catalogs hold no dead text.
func TestMessageKeysUsed(t *testing.T) {
	templates, goFiles := messageSources(t)
	catalog := map[string]bool{}
	families := map[string]bool{}
	for k := range i18n.Catalog(i18n.FR) {
		base, _, _ := strings.Cut(k, "|")
		catalog[base] = true
		family, _, _ := strings.Cut(base, ".")
		families[family] = true
	}
	used := map[string]bool{}
	for name, src := range templates {
		for _, re := range []*regexp.Regexp{templateKey, nestedKey} {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				used[m[1]] = true
				if !catalog[m[1]] {
					t.Errorf("%s uses unknown message %q", name, m[1])
				}
			}
		}
	}
	for name, src := range goFiles {
		for _, m := range goKey.FindAllStringSubmatch(src, -1) {
			family, _, _ := strings.Cut(m[1], ".")
			if !families[family] {
				continue // not a message key (a file name, a log key)
			}
			used[m[1]] = true
			if !catalog[m[1]] {
				t.Errorf("%s uses unknown message %q", name, m[1])
			}
		}
	}
	// "breadcrumb" has no dot: templates use it directly.
	for k := range catalog {
		if !used[k] && !strings.Contains(strings.Join(mapValues(templates), ""), `"`+k+`"`) {
			t.Errorf("message %q is never used", k)
		}
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// TestLocaleMarkup renders pages in each locale: <html lang dir> follow
// the interface locale, the switch keeps the route, and no catalog key or
// raw placeholder reaches the page.
func TestLocaleMarkup(t *testing.T) {
	h := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, tc := range []struct {
		l       i18n.Locale
		html    string
		text    string
		switch_ string
	}{
		{i18n.FR, `<html lang="fr" dir="ltr">`, "Se connecter", `href="/login?lang=ar&amp;next=%2Flearn" hreflang="ar" lang="ar" dir="rtl">العربية</a>`},
		{i18n.AR, `<html lang="ar" dir="rtl">`, "تسجيل الدخول", `href="/login?lang=fr&amp;next=%2Flearn" hreflang="fr" lang="fr" dir="ltr">Français</a>`},
	} {
		r := httptest.NewRequest(http.MethodGet, "/login?next=%2Flearn", nil)
		r = r.WithContext(i18n.With(r.Context(), tc.l))
		w := httptest.NewRecorder()
		h.render(w, r, http.StatusOK, "login", view{Title: msg(r, "login.title"), Data: loginData{Next: "/learn"}})
		body := w.Body.String()
		for _, want := range []string{tc.html, tc.text, tc.switch_, `<main id="contenu" tabindex="-1">`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q in %s", tc.l, want, body)
			}
		}
		if strings.Contains(body, "{0}") || strings.Contains(body, "login.") {
			t.Errorf("%s: placeholder or key in %s", tc.l, body)
		}

		// A form answered after a POST switches back to the page it
		// belongs to.
		post := httptest.NewRequest(http.MethodPost, "/learn/lessons/x/report", nil)
		post = post.WithContext(i18n.With(post.Context(), tc.l))
		w = httptest.NewRecorder()
		h.render(w, post, http.StatusOK, "error", view{Title: "x", Heading: "x", Self: "/learn/lessons/x"})
		if want := `href="/learn/lessons/x?lang=` + string(tc.l.Other()) + `"`; !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: POST switch: missing %q", tc.l, want)
		}
	}
}

// TestPublicLocaleLinks: on public pages every link keeps the URL locale
// and the switch names the same page in the other locale.
func TestPublicLocaleLinks(t *testing.T) {
	h := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r := httptest.NewRequest(http.MethodGet, "/courses/x?lang=ar", nil)
	r = r.WithContext(i18n.With(r.Context(), i18n.AR))
	w := httptest.NewRecorder()
	h.publicNotFound(w, r, "error.courseUnavailable", "pub.courseUnavailableText")
	body := w.Body.String()
	for _, want := range []string{`<html lang="ar" dir="rtl">`, `href="/?lang=ar"`, `href="/catalogue?lang=ar"`, `href="/login?lang=ar"`,
		`href="/courses/x" hreflang="fr" lang="fr" dir="ltr">Français</a>`, "الدورة غير متاحة"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, `href="/"`) || strings.Contains(body, `href="/catalogue"`) {
		t.Errorf("a link drops the locale: %s", body)
	}
}

// TestCatalogueNameMarkup: a name in the other language than the
// interface is isolated with its language; codes are isolated in Arabic.
func TestCatalogueNameMarkup(t *testing.T) {
	h := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	fr, ar := "Mathématiques", "رياضيات"
	for _, tc := range []struct {
		l    i18n.Locale
		subj Subject
		want string
	}{
		{i18n.FR, Subject{Code: "math", Names: namesOf(&fr, &ar)}, "<h1>Mathématiques</h1>"},
		{i18n.AR, Subject{Code: "math", Names: namesOf(&fr, &ar)}, "<h1>رياضيات</h1>"},
		{i18n.AR, Subject{Code: "math", Names: namesOf(&fr, nil)}, `<h1><bdi lang="fr" dir="ltr">Mathématiques</bdi></h1>`},
		{i18n.FR, Subject{Code: "math", Names: namesOf(nil, &ar)}, `<h1><bdi lang="ar" dir="rtl">رياضيات</bdi></h1>`},
		{i18n.FR, Subject{Code: "math"}, "<h1>math</h1>"},
		{i18n.AR, Subject{Code: "math"}, `<h1><bdi dir="ltr">math</bdi></h1>`},
	} {
		r := httptest.NewRequest(http.MethodGet, "/learn/subjects/math", nil)
		r = r.WithContext(i18n.With(r.Context(), tc.l))
		w := httptest.NewRecorder()
		h.render(w, r, http.StatusOK, "subject", view{Title: "x", Data: subjectData{Subject: tc.subj}})
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s %+v: missing %q in %s", tc.l, tc.subj.Names, tc.want, w.Body)
		}
	}
}

func namesOf(fr, ar *string) catalogue.Names { return catalogue.Names{Fr: fr, Ar: ar} }
