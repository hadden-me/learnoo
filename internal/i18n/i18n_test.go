package i18n

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var placeholder = regexp.MustCompile(`\{(\d+|n)\}`)

// baseKey strips a plural category.
func baseKey(k string) string { b, _, _ := strings.Cut(k, "|"); return b }

func placeholders(s string) []string {
	out := placeholder.FindAllString(s, -1)
	slices.Sort(out)
	return slices.Compact(out)
}

// TestCatalogParity: every message exists in both locales, plural messages
// have every category the locale needs (and no other form), no message is
// empty, and both locales use the same numbered placeholders.
func TestCatalogParity(t *testing.T) {
	bases := map[Locale]map[string]bool{}
	for _, l := range Locales {
		bases[l] = map[string]bool{}
		for k, v := range catalogs[l] {
			bases[l][baseKey(k)] = true
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s: %s is empty", l, k)
			}
			if b, cat, plural := strings.Cut(k, "|"); plural {
				if !slices.Contains(PluralCategories[l], cat) {
					t.Errorf("%s: %s has category %q, not used by the locale", l, b, cat)
				}
			}
		}
	}
	for _, l := range Locales {
		for _, o := range Locales {
			for k := range bases[l] {
				if !bases[o][k] {
					t.Errorf("%s is in %s but missing in %s", k, l, o)
				}
			}
		}
	}
	for k := range bases[FR] {
		_, singular := catalogs[FR][k]
		if singular {
			for _, l := range Locales {
				if _, ok := catalogs[l][k]; !ok {
					t.Errorf("%s: %s is plural in one locale only", l, k)
				}
			}
			if a, b := placeholders(catalogs[FR][k]), placeholders(catalogs[AR][k]); !slices.Equal(a, b) {
				t.Errorf("%s: placeholders fr %v, ar %v", k, a, b)
			}
			continue
		}
		for _, l := range Locales {
			for _, cat := range PluralCategories[l] {
				if _, ok := catalogs[l][k+"|"+cat]; !ok {
					t.Errorf("%s: plural %s lacks %q", l, k, cat)
				}
			}
		}
	}
}

func TestPluralCategory(t *testing.T) {
	for n, want := range map[int]string{0: "zero", 1: "one", 2: "two", 3: "few", 10: "few", 11: "many", 99: "many", 100: "other", 102: "other", 103: "few", 111: "many", 1000: "other"} {
		if got := PluralCategory(AR, n); got != want {
			t.Errorf("ar %d: %s, want %s", n, got, want)
		}
	}
	for n, want := range map[int]string{0: "one", 1: "one", 2: "other", 50: "other"} {
		if got := PluralCategory(FR, n); got != want {
			t.Errorf("fr %d: %s, want %s", n, got, want)
		}
	}
	if got := N(FR, "count.lessons", 3); got != "3 leçons" {
		t.Errorf("N fr: %q", got)
	}
	if got := N(AR, "count.lessons", 2); got != "درسان" {
		t.Errorf("N ar: %q", got)
	}
	if got := N(AR, "count.lessons", 11); got != "11 درسًا" {
		t.Errorf("N ar many: %q", got)
	}
}

func TestFormat(t *testing.T) {
	show := func(a any) string { return "<" + a.(string) + ">" }
	for text, want := range map[string]string{
		"a {0} b {1}": "a <x> b <y>",
		"{1}{0}":      "<y><x>",
		"{2} {x} {":   "{2} {x} {",
		"none":        "none",
	} {
		if got := Format(text, show, "x", "y"); got != want {
			t.Errorf("Format(%q) = %q, want %q", text, got, want)
		}
	}
	if got := T(AR, "missing.key"); got != "missing.key" {
		t.Errorf("missing key shows %q", got)
	}
}

func TestMiddleware(t *testing.T) {
	var seen Locale
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = From(r) }))
	serve := func(method, target string, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
		}
		w := httptest.NewRecorder()
		seen = ""
		h.ServeHTTP(w, r)
		return w
	}
	// A valid choice sets the cookie and comes back to the same route,
	// keeping the rest of the query.
	w := serve(http.MethodGet, "/learn/search?q=suites&lang=ar", "")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/learn/search?q=suites" {
		t.Fatalf("switch: %d %q", w.Code, w.Header().Get("Location"))
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != CookieName || c[0].Value != "ar" || !c[0].Secure || c[0].HttpOnly || c[0].Path != "/" || c[0].SameSite != http.SameSiteLaxMode || c[0].MaxAge <= 0 {
		t.Fatalf("cookie: %+v", c)
	}
	// Invalid or repeated values are dropped without a cookie.
	for _, target := range []string{"/learn?lang=en", "/learn?lang=", "/learn?lang=ar&lang=fr"} {
		w := serve(http.MethodGet, target, "")
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/learn" || len(w.Result().Cookies()) != 0 {
			t.Errorf("%s: %d %q %v", target, w.Code, w.Header().Get("Location"), w.Result().Cookies())
		}
	}
	// A path that is not plainly local never becomes an off-site redirect.
	if got := localTarget("//evil.test/x", nil); got != "/" {
		t.Errorf("localTarget: %q", got)
	}
	// Without the parameter: the cookie's locale, French by default or for
	// an unknown value; POST is never redirected.
	for cookie, want := range map[string]Locale{"": FR, "ar": AR, "fr": FR, "en": FR} {
		if w := serve(http.MethodGet, "/learn", cookie); w.Code != http.StatusOK || seen != want {
			t.Errorf("cookie %q: %d %s", cookie, w.Code, seen)
		}
	}
	if w := serve(http.MethodPost, "/learn/preferences?lang=ar", "ar"); w.Code != http.StatusOK || seen != AR {
		t.Errorf("POST: %d %s", w.Code, seen)
	}
}

func TestSwitchAndPublicURLs(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/learn/courses/x/lessons/y?a=1", nil)
	if got := SwitchURL(r, FR, "/learn"); got != "/learn/courses/x/lessons/y?a=1&lang=ar" {
		t.Errorf("SwitchURL fr: %q", got)
	}
	if got := SwitchURL(r, AR, "/learn"); got != "/learn/courses/x/lessons/y?a=1&lang=fr" {
		t.Errorf("SwitchURL ar: %q", got)
	}
	post := httptest.NewRequest(http.MethodPost, "/learn/lessons/y/report", nil)
	if got := SwitchURL(post, FR, "/learn/lessons/y"); got != "/learn/lessons/y?lang=ar" {
		t.Errorf("SwitchURL after POST: %q", got)
	}
	for in, want := range map[string]string{
		"/":                              "/?lang=ar",
		"/catalogue/l/s?edition=e":       "/catalogue/l/s?edition=e&lang=ar",
		"/login?next=%2Flearn%2Fcourses": "/login?lang=ar&next=%2Flearn%2Fcourses",
		"/catalogue?lang=fr":             "/catalogue?lang=ar",
		"https://elsewhere.test/":        "https://elsewhere.test/",
	} {
		if got := PublicURL(AR, in); got != want {
			t.Errorf("PublicURL(ar, %q) = %q, want %q", in, got, want)
		}
	}
	if got := PublicURL(FR, "/catalogue/l/s?edition=e&lang=ar"); got != "/catalogue/l/s?edition=e" {
		t.Errorf("PublicURL(fr): %q", got)
	}
	for target, want := range map[string]struct {
		l  Locale
		ok bool
	}{"/": {FR, true}, "/?lang=ar": {AR, true}, "/?lang=fr": {FR, true}, "/?lang=": {FR, false}, "/?lang=en": {FR, false}, "/?lang=ar&lang=ar": {FR, false}} {
		l, ok := PublicLocale(httptest.NewRequest(http.MethodGet, target, nil))
		if l != want.l || ok != want.ok {
			t.Errorf("PublicLocale(%s) = %s %v", target, l, ok)
		}
	}
}
