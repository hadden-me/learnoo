package server_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/i18n"
	"github.com/hadden-me/learnoo/internal/reader"
)

// Interface localization (LMS-B09) through the real server handler. TEST
// accounts, curriculum entries, lessons and courses only.

func withLang(l string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: i18n.CookieName, Value: l}) }
}

// withQuery adds a query parameter to path.
func withQuery(path, param string) string {
	if strings.Contains(path, "?") {
		return path + "&" + param
	}
	return path + "?" + param
}

// TestPublicPagesPerLocale: the locale of a public page is its URL. Each
// locale is its own cacheable representation (public, no-cache, its own
// ETag, no Vary, no cookie), identical for every viewer whatever their
// session or language cookie, and every link keeps the locale.
func TestPublicPagesPerLocale(t *testing.T) {
	p := newPublic(t)
	ch := p.chapter(p.subject, "ch-a")
	lesson := p.publish(p.approved(0, readerDoc("TEST Titre leçon A", "fr"), "fr", &ch))
	_, asg := p.edition("test-2026", "TEST Édition 2026", true, nil, ch)
	c, _ := p.publishedCourse(courseSpec{title: "TEST Cours public", summary: "TEST résumé", assignment: asg, steps: []published{lesson}})

	forged := apiUser{cookie: strings.Repeat("A", 43)}
	pages := []string{"/", "/catalogue", "/catalogue/test-bac/test-maths", "/catalogue/test-bac/test-maths/test-math", "/courses/" + c.PublicID}
	for _, path := range pages {
		fr := p.public(apiUser{}, path)
		ar := p.public(apiUser{}, withQuery(path, "lang=ar"))
		wantPage(t, path, fr, http.StatusOK, `<html lang="fr" dir="ltr">`, `hreflang="ar" lang="ar" dir="rtl">العربية</a>`)
		wantPage(t, path+" ar", ar, http.StatusOK, `<html lang="ar" dir="rtl">`, `hreflang="fr" lang="fr" dir="ltr">Français</a>`,
			`href="/?lang=ar"`, `href="/catalogue?lang=ar"`, `href="/login?lang=ar"`, "الفهرس")
		wantAbsent(t, path+" ar", ar, `href="/catalogue">`, `href="/login">`, "Parcourir le catalogue", "Se connecter</a>")
		if fr.Header().Get("ETag") == ar.Header().Get("ETag") {
			t.Errorf("%s: same ETag for both locales", path)
		}
		// The switch on each page names the same page in the other locale.
		if !strings.Contains(fr.Body.String(), `href="`+strings.ReplaceAll(withQuery(path, "lang=ar"), "&", "&amp;")+`" hreflang="ar"`) {
			t.Errorf("%s: switch does not keep the route: %s", path, fr.Body)
		}
		// Neither the session nor the language cookie changes the bytes.
		for _, u := range []apiUser{forged, p.student, p.admin} {
			for l, w := range map[string]*httptest.ResponseRecorder{"fr": fr, "ar": ar} {
				target := path
				if l == "ar" {
					target = withQuery(path, "lang=ar")
				}
				for _, cookie := range []string{"fr", "ar"} {
					got := p.public(u, target, withLang(cookie))
					if !bytes.Equal(got.Body.Bytes(), w.Body.Bytes()) {
						t.Errorf("%s (%s): body depends on the viewer or the %s cookie", path, l, cookie)
					}
				}
			}
		}
		// Revalidation per locale.
		nm := p.page(apiUser{}, http.MethodGet, withQuery(path, "lang=ar"), nil, withHeader("If-None-Match", ar.Header().Get("ETag")))
		if nm.Code != http.StatusNotModified {
			t.Errorf("%s ar: revalidation %d", path, nm.Code)
		}
		checkPublicHeaders(t, "revalidate ar "+path, nm)
	}

	// Arabic interface, French content: names follow the interface, the
	// course text keeps its own language and direction.
	home := p.public(apiUser{}, "/?lang=ar")
	wantPage(t, "home ar", home, http.StatusOK, "TEST باكالوريا › TEST شعبة الرياضيات", "TEST طبعة (ساري)",
		`<a href="/catalogue/test-bac/test-maths/test-math?lang=ar">TEST رياضيات</a>: دورة واحدة، درس واحد`,
		`href="/login?lang=ar&amp;next=%2Fstudio"`)
	course := p.public(apiUser{}, "/courses/"+c.PublicID+"?lang=ar")
	wantPage(t, "course ar", course, http.StatusOK, `<title lang="fr">TEST Cours public · Learnoo</title>`,
		`<h1 id="course-title" lang="fr" dir="ltr">TEST Cours public</h1>`, "<dd>الفرنسية</dd>",
		`href="/login?lang=ar&amp;next=%2Flearn%2Fcourses%2F`+c.PublicID+`"`,
		`<a href="/catalogue/test-bac/test-maths/test-math?lang=ar">TEST باكالوريا › TEST شعبة الرياضيات</a>`)

	// Malformed locales are malformed links: the no-store 404 notice.
	for _, path := range []string{"/?lang=en", "/catalogue?lang=", "/catalogue?lang=ar&lang=fr", "/courses/" + c.PublicID + "?lang=AR"} {
		w := p.public(apiUser{}, path)
		wantPage(t, path, w, http.StatusNotFound)
		wantAbsent(t, path, w, "TEST Cours public")
	}
}

// TestReaderLocaleCookie: on session pages the choice is the cookie, set by
// ?lang= with a redirect back to the same route; pages stay private.
func TestReaderLocaleCookie(t *testing.T) {
	f := newReader(t)
	lesson := f.publishNew("TEST Titre leçon", "fr", nil)
	s := f.student

	w := f.page(s, http.MethodGet, "/learn/search?q=titre&lang=ar", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/learn/search?q=titre" {
		t.Fatalf("switch: %d %q", w.Code, w.Header().Get("Location"))
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].Name != i18n.CookieName || c[0].Value != "ar" {
		t.Fatalf("switch cookie: %v", c)
	}
	checkReaderHeaders(t, "switch", w)

	home := f.page(s, http.MethodGet, "/learn", nil, withLang("ar"))
	checkReaderHeaders(t, "home ar", home)
	wantPage(t, "home ar", home, http.StatusOK, `<html lang="ar" dir="rtl">`, "<title>دروسي · Learnoo</title>", "مرحبًا <bdi>TEST Élève</bdi>",
		`href="/learn?lang=fr"`, "تسجيل الخروج")
	wantAbsent(t, "home ar", home, "Se déconnecter", "Mes leçons")

	// The lesson keeps its language and direction inside the Arabic
	// interface; formulas stay left to right.
	page := f.page(s, http.MethodGet, "/learn/lessons/"+lesson.publicID, nil, withLang("ar"))
	wantPage(t, "lesson ar", page, http.StatusOK, `<html lang="ar" dir="rtl">`, `<title lang="fr">TEST Titre leçon · Learnoo</title>`,
		`<h1 lang="fr" dir="ltr">TEST Titre leçon</h1>`, `<article class="lesson-body" aria-label="Contenu de la leçon" lang="fr" dir="ltr">`,
		"الإبلاغ عن خطأ", `href="/learn/lessons/`+lesson.publicID+`?lang=fr"`)

	// French remains the default without a cookie, and a forged value
	// falls back to it.
	for _, opt := range []reqOpt{func(*http.Request) {}, withLang("en")} {
		wantPage(t, "home fr", f.page(s, http.MethodGet, "/learn", nil, opt), http.StatusOK, `<html lang="fr" dir="ltr">`, "Mes leçons")
	}

	// Signed out: the sign-in page follows the cookie too, and its switch
	// keeps the return path.
	login := f.page(apiUser{}, http.MethodGet, "/login?next=%2Flearn%2Fsearch", nil, withLang("ar"))
	wantPage(t, "login ar", login, http.StatusOK, `<html lang="ar" dir="rtl">`, "تسجيل الدخول", `href="/login?lang=fr&amp;next=%2Flearn%2Fsearch"`)
	if cc := login.Header().Get("Cache-Control"); cc != reader.CacheControl {
		t.Errorf("login: Cache-Control %q", cc)
	}
	// Errors are localized as well.
	wantPage(t, "404 ar", f.page(s, http.MethodGet, "/learn/lessons/00000000-0000-0000-0000-000000000000", nil, withLang("ar")),
		http.StatusNotFound, "الدرس غير متاح")
}

// TestStudioShellLocale: the Studio shell carries the cookie's language and
// direction, and its 403 page is localized.
func TestStudioShellLocale(t *testing.T) {
	f := newStudioReader(t)
	w := f.studio(f.author, http.MethodGet, "/studio/courses")
	wantPage(t, "studio fr", w, http.StatusOK, `<html lang="fr" dir="ltr">`)
	ar := f.page(f.author, http.MethodGet, "/studio/courses", nil, withLang("ar"))
	wantPage(t, "studio ar", ar, http.StatusOK, `<html lang="ar" dir="rtl">`)
	if cc := ar.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("studio ar: Cache-Control %q", cc)
	}
	sw := f.page(f.author, http.MethodGet, "/studio/courses?lang=ar", nil)
	if sw.Code != http.StatusSeeOther || sw.Header().Get("Location") != "/studio/courses" || len(sw.Result().Cookies()) != 1 {
		t.Errorf("studio switch: %d %q %v", sw.Code, sw.Header().Get("Location"), sw.Result().Cookies())
	}
	denied := f.page(f.student, http.MethodGet, "/studio", nil, withLang("ar"))
	wantPage(t, "studio 403 ar", denied, http.StatusForbidden, `<html lang="ar" dir="rtl">`, "وصول محصور")
}
