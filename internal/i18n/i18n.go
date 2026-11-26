// Package i18n holds the interface locale (LMS-B09, LMS_PRODUCT_SPEC §10):
// French (the default) and Arabic, the message catalogs of the
// server-rendered pages, and how a request's locale is chosen.
//
// The interface locale is independent of content language: a French lesson
// stays French (lang="fr" dir="ltr") inside an Arabic interface, and an
// Arabic lesson stays right to left inside a French one.
//
// Choosing the locale (docs/DECISIONS.md "Interface localization"):
//
//   - Pages that depend on the session (the reader, the Studio shell) read
//     the CookieName cookie. They are private, no-store and already vary on
//     Cookie. A GET with ?lang=fr or ?lang=ar sets the cookie and redirects
//     to the same URL without the parameter (Middleware), so the language
//     switch is a plain link that keeps the route.
//   - Public pages never read cookies (they are shared-cacheable): their
//     locale is the URL's ?lang=ar parameter (absent means French), and
//     their links carry it. Each locale is a separate URL with its own ETag,
//     so no Vary header is needed.
//
// The cookie is not HttpOnly because the Studio (a single-page application)
// reads and writes it; it holds nothing but "fr" or "ar".
package i18n

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Locale is an interface locale.
type Locale string

const (
	FR Locale = "fr"
	AR Locale = "ar"
	// Default is the locale without a choice.
	Default = FR
)

// Locales lists the supported locales.
var Locales = []Locale{FR, AR}

// CookieName stores the browser's choice. The __Host- prefix makes browsers
// require Secure, Path=/ and no Domain, as for the session cookie.
const CookieName = "__Host-learnoo_lang"

// Param is the query parameter that selects a locale.
const Param = "lang"

// cookieMaxAge keeps the choice for a year.
const cookieMaxAge = 365 * 24 * 60 * 60

// Parse accepts "fr" and "ar".
func Parse(s string) (Locale, bool) {
	switch Locale(s) {
	case FR, AR:
		return Locale(s), true
	}
	return "", false
}

// Dir is the base direction of the locale.
func (l Locale) Dir() string {
	if l == AR {
		return "rtl"
	}
	return "ltr"
}

// Other is the locale the language switch offers.
func (l Locale) Other() Locale {
	if l == AR {
		return FR
	}
	return AR
}

// String returns the BCP 47 tag.
func (l Locale) String() string { return string(l) }

// Name is the locale's own name, shown on the language switch.
func (l Locale) Name() string {
	if l == AR {
		return "العربية"
	}
	return "Français"
}

type ctxKey struct{}

// With attaches a locale to ctx.
func With(ctx context.Context, l Locale) context.Context { return context.WithValue(ctx, ctxKey{}, l) }

// From returns the locale attached by With, else the cookie's, else Default.
func From(r *http.Request) Locale {
	if l, ok := r.Context().Value(ctxKey{}).(Locale); ok {
		return l
	}
	return FromCookie(r)
}

// FromCookie returns the cookie's locale, or Default.
func FromCookie(r *http.Request) Locale {
	if c, err := r.Cookie(CookieName); err == nil {
		if l, ok := Parse(c.Value); ok {
			return l
		}
	}
	return Default
}

// SetCookie stores the choice for a year.
func SetCookie(w http.ResponseWriter, l Locale) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: string(l), Path: "/", MaxAge: cookieMaxAge,
		Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

// Middleware serves session-dependent pages. A GET or HEAD with ?lang=
// stores a valid choice in the cookie and redirects (303) to the same URL
// without the parameter; an invalid value is dropped without a cookie.
// Otherwise the cookie's locale is attached to the request (From).
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			q := r.URL.Query()
			if vals, present := q[Param]; present {
				if l, ok := Parse(vals[0]); ok && len(vals) == 1 {
					SetCookie(w, l)
				}
				q.Del(Param)
				http.Redirect(w, r, localTarget(r.URL.Path, q), http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(With(r.Context(), FromCookie(r))))
	})
}

// localTarget rebuilds a same-site path with query; anything that is not a
// plain local path falls back to "/".
func localTarget(path string, q url.Values) string {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, `\`) {
		return "/"
	}
	u := url.URL{Path: path, RawQuery: q.Encode()}
	return u.RequestURI()
}

// SwitchURL is the language switch's target on a session-dependent page:
// the current URL with ?lang= set to the other locale (Middleware stores
// it and comes back). For a request that is not a GET (a form shown again
// after a POST), fallback names the page to come back to.
func SwitchURL(r *http.Request, l Locale, fallback string) string {
	path, q := r.URL.Path, r.URL.Query()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		u, err := url.Parse(fallback)
		if err != nil {
			u = &url.URL{Path: "/"}
		}
		path, q = u.Path, u.Query()
	}
	q.Set(Param, string(l.Other()))
	return localTarget(path, q)
}

// PublicURL adds the locale to a public path (which may carry a query):
// French is the URL without the parameter, Arabic adds lang=ar.
func PublicURL(l Locale, target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return target
	}
	q := u.Query()
	q.Del(Param)
	if l != Default {
		q.Set(Param, string(l))
	}
	u.RawQuery = q.Encode()
	return u.RequestURI()
}

// PublicLocale reads a public page's ?lang= parameter: absent is French;
// ok is false for an empty, repeated or unknown value (a malformed link).
func PublicLocale(r *http.Request) (Locale, bool) {
	vals, present := r.URL.Query()[Param]
	if !present {
		return Default, true
	}
	if len(vals) != 1 {
		return Default, false
	}
	l, ok := Parse(vals[0])
	if !ok {
		return Default, false
	}
	return l, true
}

// ---- messages ----

// catalogs are the message catalogs (messages_fr.go, messages_ar.go). A
// plural message is stored as "key|category" for each CLDR category the
// locale uses (PluralCategories).
var catalogs = map[Locale]map[string]string{FR: fr, AR: ar}

// Catalog returns a copy of the locale's catalog (for tests).
func Catalog(l Locale) map[string]string {
	out := make(map[string]string, len(catalogs[l]))
	for k, v := range catalogs[l] {
		out[k] = v
	}
	return out
}

// PluralCategories are the CLDR plural categories each locale needs.
var PluralCategories = map[Locale][]string{
	FR: {"one", "other"},
	AR: {"zero", "one", "two", "few", "many", "other"},
}

// PluralCategory is the CLDR cardinal category of n (integers only).
func PluralCategory(l Locale, n int) string {
	if n < 0 {
		n = -n
	}
	if l == AR {
		switch m := n % 100; {
		case n == 0:
			return "zero"
		case n == 1:
			return "one"
		case n == 2:
			return "two"
		case m >= 3 && m <= 10:
			return "few"
		case m >= 11 && m <= 99:
			return "many"
		}
		return "other"
	}
	if n == 0 || n == 1 {
		return "one"
	}
	return "other"
}

// Message returns the raw catalog text of key. A missing key is a defect
// caught by the tests; it shows the key rather than failing the page.
func Message(l Locale, key string) string {
	if s, ok := catalogs[l][key]; ok {
		return s
	}
	if s, ok := catalogs[Default][key]; ok {
		return s
	}
	return key
}

// T returns key's text with {0}, {1}… replaced by args (fmt.Sprint).
func T(l Locale, key string, args ...any) string {
	return Format(Message(l, key), func(a any) string { return fmt.Sprint(a) }, args...)
}

// N returns the plural form of key for n, with {n} replaced by n and
// {0}, {1}… by args.
func N(l Locale, key string, n int, args ...any) string {
	return strings.ReplaceAll(T(l, PluralKey(l, key, n), args...), "{n}", strconv.Itoa(n))
}

// PluralKey is the catalog key of key's form for n.
func PluralKey(l Locale, key string, n int) string { return key + "|" + PluralCategory(l, n) }

// Format replaces {0}, {1}… in text by show(args[i]); unknown or
// out-of-range placeholders are left as they are.
func Format(text string, show func(any) string, args ...any) string {
	if len(args) == 0 || !strings.Contains(text, "{") {
		return text
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(text, '{')
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		j := strings.IndexByte(text[i:], '}')
		if j < 0 {
			b.WriteString(text)
			return b.String()
		}
		n, err := strconv.Atoi(text[i+1 : i+j])
		b.WriteString(text[:i])
		if err == nil && n >= 0 && n < len(args) {
			b.WriteString(show(args[n]))
		} else {
			b.WriteString(text[i : i+j+1])
		}
		text = text[i+j+1:]
	}
}
