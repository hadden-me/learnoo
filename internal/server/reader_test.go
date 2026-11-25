package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/invitations"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/reader"
	"github.com/hadden-me/learnoo/internal/rendering"
	"github.com/hadden-me/learnoo/internal/review"
)

// Student reader (LRN-008) through the real server handler. Every lesson is
// a clearly labelled TEST fixture document; names are simulated.

type readerFixture struct {
	*env
	subject   int64
	author    apiUser
	reviewer  apiUser
	publisher apiUser
	admin     apiUser
	student   apiUser
	ls        *lessons.Store
	rs        *review.Store
	inv       *invitations.Store
}

func newReader(t *testing.T) readerFixture {
	e := newEnv(t)
	subject := e.newSubject("test-math")
	f := readerFixture{env: e, subject: subject,
		author:    e.newUser("test-author", subject, permissions.Author),
		reviewer:  e.newUser("test-reviewer", subject, permissions.Reviewer),
		publisher: e.newUser("test-publisher", subject, permissions.Publisher),
		admin:     e.newUser("test-admin", subject, permissions.Administrator),
		ls:        lessons.NewStore(e.db, nil),
		rs:        review.NewStore(e.db),
		inv:       invitations.NewStore(e.db, nil),
	}
	f.setName(f.author.id, "TEST Autrice Amel")
	f.setName(f.reviewer.id, "TEST Relecteur Karim")
	f.student = f.invitedStudent("test-student@example.test", "TEST Élève")
	return f
}

func (f readerFixture) setName(id int64, name string) {
	f.t.Helper()
	if _, err := f.db.Exec(context.Background(), `UPDATE users SET display_name = $2 WHERE id = $1`, id, name); err != nil {
		f.t.Fatal(err)
	}
}

// page sends a browser-like request: form-encoded body for POST.
func (e *env) page(u apiUser, method, path string, form url.Values, extra ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, "http://learnoo.test"+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, "http://learnoo.test"+path, nil)
	}
	r.RemoteAddr = "192.0.2.20:40000"
	if u.cookie != "" {
		r.AddCookie(&http.Cookie{Name: "__Host-learnoo_session", Value: u.cookie})
	}
	for _, o := range extra {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// checkReaderHeaders asserts the cache rule on reader responses.
func checkReaderHeaders(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if cc := w.Header().Get("Cache-Control"); cc != reader.CacheControl {
		t.Errorf("%s: Cache-Control %q, want %q", what, cc, reader.CacheControl)
	}
	if v := w.Header().Get("Vary"); v != "Cookie" {
		t.Errorf("%s: Vary %q", what, v)
	}
}

// invitedStudent goes through the whole invitation flow over HTTP and
// returns the signed-in student.
func (f readerFixture) invitedStudent(email, name string) apiUser {
	f.t.Helper()
	token, _, err := f.inv.Issue(context.Background(), email, "test-operator@test-host", time.Hour)
	if err != nil {
		f.t.Fatal(err)
	}
	w := f.page(apiUser{}, http.MethodPost, "/invite", url.Values{
		"token": {token}, "display_name": {name}, "password": {testPassword}, "password_confirm": {testPassword},
	}, withAddr("203.0.113.9"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/learn" {
		f.t.Fatalf("accept invitation: %d %s", w.Code, w.Body)
	}
	cookie := sessionCookie(f.t, w).Value
	s := f.api(apiUser{cookie: cookie}, http.MethodGet, "/api/auth/session", nil)
	var b sessionBody
	if err := json.Unmarshal(s.Body.Bytes(), &b); err != nil {
		f.t.Fatal(err)
	}
	return apiUser{id: b.User.ID, cookie: cookie, csrf: b.CSRFToken}
}

// TEST documents. Every title starts with "TEST".
func readerDoc(title, lang string, blocks ...any) []byte {
	if len(blocks) == 0 {
		blocks = []any{map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": "Texte de test."}}}}
	}
	objective := "Objectif de test."
	if lang == "ar" {
		objective = "هدف تجريبي."
	}
	b, _ := json.Marshal(map[string]any{
		"schemaVersion": 1, "title": title, "language": lang, "status": "unreviewed",
		"objective": objective, "blocks": blocks,
	})
	return b
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
func para(children ...any) map[string]any {
	return map[string]any{"type": "paragraph", "children": children}
}

// richDoc covers mixed direction, math, an exercise with its solution, an
// image and a YouTube reference.
func richDoc(title string) []byte {
	arabic := para(text("شرح تجريبي "), map[string]any{"type": "math", "text": `z = 3 + 2i`})
	arabic["lang"], arabic["dir"] = "ar", "rtl"
	return readerDoc(title, "fr",
		para(text("Texte de test avec "), map[string]any{"type": "math", "text": `\frac{1}{2}`}),
		arabic,
		map[string]any{"type": "displayMath", "text": `x^2 + 1 = 0`},
		map[string]any{"type": "image", "src": "/static/complex-plane.svg", "alt": "Image de test"},
		map[string]any{"type": "youtube", "title": "Vidéo de test", "videoId": "TESTvideo01"},
		map[string]any{"type": "exercise", "title": "Exercice de test", "children": []any{
			para(text("Énoncé de test.")),
			map[string]any{"type": "solution", "children": []any{para(text("Solution de test."))}},
		}},
	)
}

type published struct {
	lessonID, revisionID int64
	publicID             string
}

// approved creates a lesson (or, with lessonID, a new draft) and takes it
// through submission and approval, without publishing.
func (f readerFixture) approved(lessonID int64, content []byte, lang string, chapter *int64) lessons.Revision {
	f.t.Helper()
	ctx := context.Background()
	fields := lessons.Fields{Content: content, ChapterID: chapter}
	var rev lessons.Revision
	var err error
	if lessonID == 0 {
		rev, err = f.ls.CreateLesson(ctx, f.author.id, lessons.NewLesson{SubjectID: f.subject, Language: lang, Fields: fields})
	} else {
		rev, err = f.ls.CreateDraft(ctx, f.author.id, lessonID, fields)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	if rev, err = f.ls.Submit(ctx, f.author.id, rev.ID, rev.Version); err != nil {
		f.t.Fatal(err)
	}
	if rev, err = f.rs.Approve(ctx, f.reviewer.id, rev.ID, ""); err != nil {
		f.t.Fatal(err)
	}
	return rev
}

func (f readerFixture) publish(rev lessons.Revision) published {
	f.t.Helper()
	if _, err := f.rs.Publish(context.Background(), f.publisher.id, rev.ID); err != nil {
		f.t.Fatal(err)
	}
	var p published
	p.lessonID, p.revisionID = rev.LessonID, rev.ID
	if err := f.db.QueryRow(context.Background(), `SELECT public_id::text FROM lessons WHERE id = $1`, rev.LessonID).Scan(&p.publicID); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// unpublish clears the lesson's published revision through the audited
// LRN-009 emergency unpublication.
func (f readerFixture) unpublish(lessonID, current int64) {
	f.t.Helper()
	if _, err := f.rs.Unpublish(context.Background(), f.admin.id, lessonID, "TEST unpublication", current); err != nil {
		f.t.Fatal(err)
	}
}

func (f readerFixture) publishNew(title, lang string, chapter *int64) published {
	f.t.Helper()
	return f.publish(f.approved(0, readerDoc(title, lang), lang, chapter))
}

func (f readerFixture) publicID(lessonID int64) string {
	f.t.Helper()
	var id string
	if err := f.db.QueryRow(context.Background(), `SELECT public_id::text FROM lessons WHERE id = $1`, lessonID).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func lessonPath(id string) string { return "/learn/lessons/" + id }

func (f readerFixture) get(u apiUser, path string) *httptest.ResponseRecorder {
	f.t.Helper()
	w := f.page(u, http.MethodGet, path, nil)
	checkReaderHeaders(f.t, "GET "+path, w)
	return w
}

func (f readerFixture) post(u apiUser, path string, form url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if u.csrf != "" && form.Get("csrf_token") == "" {
		form.Set("csrf_token", u.csrf)
	}
	w := f.page(u, http.MethodPost, path, form)
	checkReaderHeaders(f.t, "POST "+path, w)
	return w
}

func wantPage(t *testing.T, what string, w *httptest.ResponseRecorder, status int, contains ...string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", what, w.Code, status, w.Body)
	}
	for _, s := range contains {
		if !strings.Contains(w.Body.String(), s) {
			t.Errorf("%s: body lacks %q", what, s)
		}
	}
}

func wantAbsent(t *testing.T, what string, w *httptest.ResponseRecorder, absent ...string) {
	t.Helper()
	for _, s := range absent {
		if strings.Contains(w.Body.String(), s) {
			t.Errorf("%s: body contains %q", what, s)
		}
	}
}

func wantRedirect(t *testing.T, what string, w *httptest.ResponseRecorder, location string) {
	t.Helper()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != location {
		t.Fatalf("%s: %d to %q, want 303 to %q", what, w.Code, w.Header().Get("Location"), location)
	}
}

func TestReaderAnonymousRequestsGetNoContent(t *testing.T) {
	f := newReader(t)
	p := f.publish(f.approved(0, richDoc("TEST Secret de lecture"), "fr", nil))
	forged := apiUser{cookie: strings.Repeat("A", 43)}
	for _, u := range []apiUser{{}, forged} {
		for _, path := range []string{"/learn", "/learn/search?q=TEST", "/learn/subjects/test-math", lessonPath(p.publicID),
			lessonPath("00000000-0000-0000-0000-000000000000"), "/learn/subjects/unknown"} {
			w := f.get(u, path)
			next := strings.SplitN(path, "?", 2)[0]
			wantRedirect(t, "anonymous GET "+path, w, "/login?next="+url.QueryEscape(next))
			wantAbsent(t, "anonymous GET "+path, w, "TEST Secret", "Texte de test", "TEST Autrice")
			if h := f.page(u, http.MethodHead, path, nil); h.Code != http.StatusSeeOther {
				t.Errorf("anonymous HEAD %s: %d", path, h.Code)
			}
		}
		for _, path := range []string{lessonPath(p.publicID) + "/bookmark", lessonPath(p.publicID) + "/unbookmark"} {
			w := f.post(u, path, url.Values{"csrf_token": {"guess"}})
			wantPage(t, "anonymous POST "+path, w, http.StatusUnauthorized)
			wantAbsent(t, "anonymous POST "+path, w, "TEST Secret")
		}
		// Lesson media needs a session; unknown assets look the same.
		for _, path := range []string{"/static/complex-plane.svg", "/static/landing.html", "/static/unknown.css"} {
			if w := f.get(u, path); w.Code != http.StatusNotFound {
				t.Errorf("anonymous %s: %d", path, w.Code)
			}
		}
	}
	if n := f.count(`SELECT count(*) FROM bookmarks`); n != 0 {
		t.Fatalf("%d bookmarks written anonymously", n)
	}
	// Generic styles needed by the sign-in page are public.
	for _, path := range []string{"/static/tokens.css", "/static/site.css", "/static/reader.css", "/static/math.js"} {
		if w := f.get(apiUser{}, path); w.Code != http.StatusOK {
			t.Errorf("public asset %s: %d", path, w.Code)
		}
	}
	// Signed in: the media is served; directories and traversal are not.
	if w := f.get(f.student, "/static/complex-plane.svg"); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "image/svg+xml") {
		t.Fatalf("student media: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, path := range []string{"/static/vendor/katex/", "/static/vendor/katex/fonts", "/static/../go.mod", "/static/%2e%2e/page.html", "/static/fixtures/bidi.json"} {
		if w := f.page(f.student, http.MethodGet, path, nil); w.Code == http.StatusOK {
			t.Errorf("student %s: 200", path)
		}
	}
}

func TestReaderShowsOnlyPublishedLessons(t *testing.T) {
	f := newReader(t)
	ctx := context.Background()
	ch := f.chapter(f.subject, "test-chapter")
	visible := f.publishNew("TEST Visible publiée", "fr", &ch)

	draft, err := f.ls.CreateLesson(ctx, f.author.id, lessons.NewLesson{SubjectID: f.subject, Language: "fr",
		Fields: lessons.Fields{Content: readerDoc("TEST Brouillon caché", "fr")}})
	if err != nil {
		t.Fatal(err)
	}
	inReview, err := f.ls.CreateLesson(ctx, f.author.id, lessons.NewLesson{SubjectID: f.subject, Language: "fr",
		Fields: lessons.Fields{Content: readerDoc("TEST En relecture caché", "fr")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ls.Submit(ctx, f.author.id, inReview.ID, inReview.Version); err != nil {
		t.Fatal(err)
	}
	approvedOnly := f.approved(0, readerDoc("TEST Approuvée non publiée", "fr"), "fr", nil)
	unpublished := f.publishNew("TEST Dépubliée cachée", "fr", nil)
	archived := f.publishNew("TEST Archivée cachée", "fr", nil)
	// Audited unpublication (LRN-009). Archival has no code path yet; set
	// it directly.
	f.unpublish(unpublished.lessonID, unpublished.revisionID)
	if _, err := f.db.Exec(ctx, `UPDATE lessons SET archived_at = now() WHERE id = $1`, archived.lessonID); err != nil {
		t.Fatal(err)
	}
	// A subject whose only lesson is a draft is not listed.
	other := f.newSubject("test-empty")
	if _, err := f.ls.CreateLesson(ctx, f.author.id, lessons.NewLesson{SubjectID: other, Language: "fr",
		Fields: lessons.Fields{Content: readerDoc("TEST Autre brouillon", "fr")}}); err != nil {
		t.Fatal(err)
	}

	hidden := []string{"caché", "non publiée", "Autre brouillon", "test-empty"}
	dash := f.get(f.student, "/learn")
	wantPage(t, "dashboard", dash, http.StatusOK, "test-math", "1 leçon publiée")
	wantAbsent(t, "dashboard", dash, hidden...)
	subj := f.get(f.student, "/learn/subjects/test-math")
	wantPage(t, "subject", subj, http.StatusOK, "TEST Visible publiée", "test-chapter", lessonPath(visible.publicID))
	wantAbsent(t, "subject", subj, hidden...)
	wantPage(t, "empty subject", f.get(f.student, "/learn/subjects/test-empty"), http.StatusNotFound)
	wantPage(t, "unknown subject", f.get(f.student, "/learn/subjects/nope"), http.StatusNotFound)
	wantPage(t, "bad subject code", f.get(f.student, "/learn/subjects/NOPE%27"), http.StatusNotFound)

	search := f.get(f.student, "/learn/search?q=TEST")
	wantPage(t, "search", search, http.StatusOK, "TEST Visible publiée", "Résultats (1)")
	wantAbsent(t, "search", search, hidden...)

	wantPage(t, "visible lesson", f.get(f.student, lessonPath(visible.publicID)), http.StatusOK, "TEST Visible publiée")
	for name, lessonID := range map[string]int64{"draft": draft.LessonID, "in review": inReview.LessonID,
		"approved only": approvedOnly.LessonID, "unpublished": unpublished.lessonID, "archived": archived.lessonID} {
		id := f.publicID(lessonID)
		w := f.get(f.student, lessonPath(id))
		wantPage(t, name, w, http.StatusNotFound, "Leçon indisponible")
		wantAbsent(t, name, w, "TEST")
		wantPage(t, "bookmark "+name, f.post(f.student, lessonPath(id)+"/bookmark", nil), http.StatusNotFound)
	}
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid", "00000000-0000-0000-0000-00000000000G",
		strings.ToUpper(visible.publicID), "%27%20OR%201%3D1--"} {
		wantPage(t, "lesson "+id, f.get(f.student, lessonPath(id)), http.StatusNotFound)
	}
	if n := f.count(`SELECT count(*) FROM bookmarks`); n != 0 {
		t.Fatalf("%d bookmarks of unpublished lessons", n)
	}
}

func TestLessonPageRendersCanonicalContentWithCredit(t *testing.T) {
	f := newReader(t)
	ch := f.chapter(f.subject, "test-chapter")
	content := richDoc("TEST Leçon mixte")
	p := f.publish(f.approved(0, content, "fr", &ch))
	w := f.get(f.student, lessonPath(p.publicID))
	wantPage(t, "lesson", w, http.StatusOK,
		`<html lang="fr" dir="ltr">`,
		`<h1 lang="fr" dir="ltr">TEST Leçon mixte</h1>`,
		`<article class="lesson-body" aria-label="Contenu de la leçon" lang="fr" dir="ltr">`,
		`<p lang="ar" dir="rtl">`,
		`<span class="math" dir="ltr" data-display="false">z = 3 &#43; 2i</span>`,
		`<div class="math" dir="ltr" data-display="true">`,
		`<section class="exercise">`, `<details><summary>`, "Solution de test.",
		`data-video-id="TESTvideo01"`, `src="/static/complex-plane.svg"`,
		`/static/vendor/katex/katex.min.js`, `/static/math.js`,
		`Rédigé par <bdi class="author">TEST Autrice Amel</bdi>`,
		`Relu et approuvé par <bdi class="reviewer">TEST Relecteur Karim</bdi>`,
		"test-math", "test-chapter", "Objectif de test.",
	)
	// The body is exactly the canonical renderer's output for the stored snapshot.
	var stored []byte
	if err := f.db.QueryRow(context.Background(), `SELECT content::text FROM lesson_revisions WHERE id = $1`, p.revisionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	doc, err := rendering.Decode(strings.NewReader(compactJSON(t, stored)))
	if err != nil {
		t.Fatal(err)
	}
	body, err := rendering.RenderBlocks(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), `lang="fr" dir="ltr">`+string(body)+`</article>`) {
		t.Fatal("lesson body differs from the canonical renderer output")
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "frame-src https://www.youtube-nocookie.com", "frame-ancestors 'none'", "style-src-attr 'unsafe-inline'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "'unsafe-eval'") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP too broad: %q", csp)
	}
	// The author or reviewer display name is escaped, never markup.
	f.setName(f.author.id, `TEST <script>alert(1)</script>`)
	w = f.get(f.student, lessonPath(p.publicID))
	wantPage(t, "escaped credit", w, http.StatusOK, "TEST &lt;script&gt;alert(1)&lt;/script&gt;")
	wantAbsent(t, "escaped credit", w, "<script>alert(1)")
}

func compactJSON(t *testing.T, b []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestArabicLessonIsRightToLeft(t *testing.T) {
	f := newReader(t)
	p := f.publish(f.approved(0, readerDoc("TEST درس تجريبي", "ar",
		para(text("نص تجريبي "), map[string]any{"type": "math", "text": `a^2`})), "ar", nil))
	w := f.get(f.student, lessonPath(p.publicID))
	wantPage(t, "arabic lesson", w, http.StatusOK,
		`<html lang="fr" dir="ltr">`, // interface language is independent of the lesson's
		`<h1 lang="ar" dir="rtl">TEST درس تجريبي</h1>`,
		`<p class="objective" lang="ar" dir="rtl">هدف تجريبي.</p>`,
		`<article class="lesson-body" aria-label="Contenu de la leçon" lang="ar" dir="rtl">`,
		// An unlabelled paragraph inherits the article's language.
		`lang="ar" dir="rtl"><p><bdi>نص تجريبي </bdi><span class="math" dir="ltr" data-display="false">a^2</span></p>`)
	wantAbsent(t, "arabic lesson", w, `lang=""`, `dir=""`)
	// Lists mark each title with its own language and direction.
	for _, path := range []string{"/learn/subjects/test-math", "/learn/search?q=" + url.QueryEscape("درس")} {
		wantPage(t, path, f.get(f.student, path), http.StatusOK, `lang="ar" dir="rtl">TEST درس تجريبي</a>`)
	}
}

func TestPublishedPointerChangeShowsNewRevisionAndKeepsBookmarks(t *testing.T) {
	f := newReader(t)
	first := f.publishNew("TEST Première version", "fr", nil)
	path := lessonPath(first.publicID)
	wantRedirect(t, "bookmark", f.post(f.student, path+"/bookmark", nil), path)

	second := f.publish(f.approved(first.lessonID, readerDoc("TEST Seconde version", "fr"), "fr", nil))
	if second.publicID != first.publicID {
		t.Fatal("public ID changed with a new revision")
	}
	w := f.get(f.student, path)
	wantPage(t, "after republication", w, http.StatusOK, "TEST Seconde version", "Retirer des signets")
	wantAbsent(t, "after republication", w, "Première version")
	dash := f.get(f.student, "/learn")
	wantPage(t, "dashboard", dash, http.StatusOK, "Mes signets (1)", "TEST Seconde version", path)
	wantAbsent(t, "dashboard", dash, "Première version")
	wantPage(t, "old title search", f.get(f.student, "/learn/search?q=premiere"), http.StatusOK, "Aucune leçon publiée")
	wantPage(t, "new title search", f.get(f.student, "/learn/search?q=seconde"), http.StatusOK, "TEST Seconde version")

	// Unpublished: the bookmark stays, shown as unavailable, and can be removed.
	f.unpublish(first.lessonID, second.revisionID)
	dash = f.get(f.student, "/learn")
	wantPage(t, "dashboard unavailable", dash, http.StatusOK, "Mes signets (1)", "Leçon indisponible pour le moment")
	wantAbsent(t, "dashboard unavailable", dash, "TEST Seconde version", `href="`+path+`"`)
	wantRedirect(t, "remove unavailable", f.post(f.student, path+"/unbookmark", url.Values{"return": {"dashboard"}}), "/learn")
	if n := f.count(`SELECT count(*) FROM bookmarks`); n != 0 {
		t.Fatalf("%d bookmarks left", n)
	}
}

// Withdrawing the published revision (LRN-009) removes the lesson from
// navigation, search and its page, and leaves bookmarks as unavailable;
// restoring an earlier revision brings the lesson back with that content.
func TestWithdrawnRevisionLeavesTheReader(t *testing.T) {
	f := newReader(t)
	ctx := context.Background()
	first := f.publishNew("TEST Version restaurable", "fr", nil)
	second := f.publish(f.approved(first.lessonID, readerDoc("TEST Version retirée", "fr"), "fr", nil))
	path := lessonPath(first.publicID)
	wantRedirect(t, "bookmark", f.post(f.student, path+"/bookmark", nil), path)
	wantPage(t, "before withdrawal", f.get(f.student, path), http.StatusOK, "TEST Version retirée")

	current := second.revisionID
	if _, err := f.rs.Withdraw(ctx, f.admin.id, second.revisionID, "TEST incorrect content", &current); err != nil {
		t.Fatal(err)
	}
	w := f.get(f.student, path)
	wantPage(t, "withdrawn lesson", w, http.StatusNotFound, "Leçon indisponible")
	wantAbsent(t, "withdrawn lesson", w, "TEST Version")
	dash := f.get(f.student, "/learn")
	wantPage(t, "dashboard", dash, http.StatusOK, "Mes signets (1)", "Leçon indisponible pour le moment", "Aucune leçon n’est publiée")
	wantAbsent(t, "dashboard", dash, "TEST Version", `href="`+path+`"`)
	wantPage(t, "subject", f.get(f.student, "/learn/subjects/test-math"), http.StatusNotFound)
	search := f.get(f.student, "/learn/search?q=version")
	wantPage(t, "search", search, http.StatusOK, "Aucune leçon publiée")
	wantPage(t, "bookmark withdrawn", f.post(f.student, path+"/bookmark", nil), http.StatusNotFound)
	if n := f.count(`SELECT count(*) FROM reader_published_lessons`); n != 0 {
		t.Fatalf("view lists %d lessons after withdrawal", n)
	}

	// Defence in depth: inside a transaction where a withdrawal row exists
	// but the pointer still names the revision (0007 refuses this at commit),
	// the view already excludes it.
	third := f.publishNew("TEST Défense en profondeur", "fr", nil)
	tx, err := f.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Roll back on failure, or the schema cleanup would wait on this
	// transaction (harmless after Commit).
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO revision_withdrawals (lesson_id, revision_id, actor_id, reason) VALUES ($1, $2, $3, 'TEST')`,
		third.lessonID, third.revisionID, f.admin.id); err != nil {
		t.Fatal(err)
	}
	var visible int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM reader_published_lessons WHERE lesson_id = $1`, third.lessonID).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatal("view shows a withdrawn revision that is still pointed to")
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("0007 accepted a withdrawn revision that is still published")
	}
	if n := f.count(`SELECT count(*) FROM reader_published_lessons WHERE lesson_id = $1`, third.lessonID); n != 1 {
		t.Fatalf("rolled-back withdrawal changed visibility: %d", n)
	}

	// Restoring the earlier, never-withdrawn revision republishes it.
	if _, err := f.rs.Restore(ctx, f.publisher.id, first.revisionID, nil); err != nil {
		t.Fatal(err)
	}
	w = f.get(f.student, path)
	wantPage(t, "restored", w, http.StatusOK, "TEST Version restaurable", "Retirer des signets")
	wantAbsent(t, "restored", w, "TEST Version retirée")
	wantPage(t, "dashboard restored", f.get(f.student, "/learn"), http.StatusOK, "TEST Version restaurable", `href="`+path+`"`)
}

func TestBookmarks(t *testing.T) {
	f := newReader(t)
	a := f.publishNew("TEST Signet A", "fr", nil)
	b := f.publishNew("TEST Signet B", "fr", nil)
	other := f.invitedStudent("test-student-two@example.test", "TEST Deux")

	// CSRF: missing, wrong, another user's token, wrong content type, cross-site.
	for name, req := range map[string]func() *httptest.ResponseRecorder{
		"missing CSRF": func() *httptest.ResponseRecorder {
			return f.page(f.student, http.MethodPost, lessonPath(a.publicID)+"/bookmark", url.Values{})
		},
		"wrong CSRF": func() *httptest.ResponseRecorder {
			return f.page(f.student, http.MethodPost, lessonPath(a.publicID)+"/bookmark", url.Values{"csrf_token": {"wrong"}})
		},
		"other user's CSRF": func() *httptest.ResponseRecorder {
			return f.page(f.student, http.MethodPost, lessonPath(a.publicID)+"/bookmark", url.Values{"csrf_token": {other.csrf}})
		},
		"CSRF in query only": func() *httptest.ResponseRecorder {
			return f.page(f.student, http.MethodPost, lessonPath(a.publicID)+"/bookmark?csrf_token="+f.student.csrf, url.Values{})
		},
		"JSON body": func() *httptest.ResponseRecorder {
			return f.page(f.student, http.MethodPost, lessonPath(a.publicID)+"/bookmark", nil,
				withHeader("Content-Type", "application/json"))
		},
	} {
		wantPage(t, name, req(), http.StatusForbidden, "Requête refusée")
	}
	w := f.page(f.student, http.MethodPost, lessonPath(a.publicID)+"/bookmark", url.Values{"csrf_token": {f.student.csrf}},
		withHeader("Sec-Fetch-Site", "cross-site"))
	wantStatus(t, "cross-site form post", w, http.StatusForbidden, "cross_origin_rejected")
	if n := f.count(`SELECT count(*) FROM bookmarks`); n != 0 {
		t.Fatalf("%d bookmarks after rejected requests", n)
	}

	wantRedirect(t, "add A", f.post(f.student, lessonPath(a.publicID)+"/bookmark", nil), lessonPath(a.publicID))
	wantRedirect(t, "add A again", f.post(f.student, lessonPath(a.publicID)+"/bookmark", nil), lessonPath(a.publicID))
	wantRedirect(t, "add B", f.post(f.student, lessonPath(b.publicID)+"/bookmark", url.Values{"return": {"dashboard"}}), "/learn")
	dash := f.get(f.student, "/learn")
	wantPage(t, "dashboard", dash, http.StatusOK, "Mes signets (2)", "TEST Signet A", "TEST Signet B")
	if strings.Index(dash.Body.String(), "TEST Signet B") > strings.Index(dash.Body.String(), "TEST Signet A") {
		t.Error("bookmarks are not listed newest first")
	}
	wantPage(t, "lesson shows saved", f.get(f.student, lessonPath(a.publicID)), http.StatusOK, "/unbookmark", "Retirer des signets")
	// Bookmarks are per user.
	wantPage(t, "other dashboard", f.get(other, "/learn"), http.StatusOK, "Mes signets (0)", "Aucun signet")
	wantPage(t, "other lesson", f.get(other, lessonPath(a.publicID)), http.StatusOK, "/bookmark", "Enregistrer")
	wantRedirect(t, "other removes", f.post(other, lessonPath(a.publicID)+"/unbookmark", nil), lessonPath(a.publicID))
	if n := f.count(`SELECT count(*) FROM bookmarks WHERE user_id = $1`, f.student.id); n != 2 {
		t.Fatalf("another user's removal changed bookmarks: %d", n)
	}
	wantRedirect(t, "remove A", f.post(f.student, lessonPath(a.publicID)+"/unbookmark", nil), lessonPath(a.publicID))
	wantRedirect(t, "remove A again", f.post(f.student, lessonPath(a.publicID)+"/unbookmark", nil), lessonPath(a.publicID))
	wantPage(t, "after removal", f.get(f.student, "/learn"), http.StatusOK, "Mes signets (1)")
	wantPage(t, "malformed id", f.post(f.student, "/learn/lessons/nope/bookmark", nil), http.StatusNotFound)
}

func TestTitleSearch(t *testing.T) {
	f := newReader(t)
	f.publishNew("TEST Équations du second degré", "fr", nil)
	f.publishNew("TEST Limites et continuité", "fr", nil)
	f.publishNew("TEST Œuvre 100% fiable", "fr", nil)
	f.publishNew("TEST الدَّوَالّ الأُسِّيَّة", "ar", nil)
	for q, want := range map[string][]string{
		"equations":                 {"Équations"},
		"ÉQUATIONS":                 {"Équations"},
		"second   degre":            {"Équations"},
		"continuite":                {"continuité"},
		"CONTINUITÉ":                {"continuité"},
		"oeuvre":                    {"Œuvre"},
		"100%":                      {"Œuvre"},
		"الدوال":                    {"الدَّوَالّ"},
		"الاسية":                    {"الأُسِّيَّة"},
		"%":                         nil, // too short
		"_%":                        nil, // no LIKE wildcards
		"' OR '1'='1":               nil,
		"'; DROP TABLE lessons; --": nil,
		`\`:                         nil,
		"Brouillon":                 nil,
	} {
		w := f.get(f.student, "/learn/search?q="+url.QueryEscape(q))
		if w.Code != http.StatusOK {
			t.Fatalf("search %q: %d", q, w.Code)
		}
		n := strings.Count(w.Body.String(), `<li class="card">`)
		if n != len(want) {
			t.Errorf("search %q: %d results, want %d", q, n, len(want))
		}
		for _, s := range want {
			if !strings.Contains(w.Body.String(), s) {
				t.Errorf("search %q: missing %q", q, s)
			}
		}
	}
	if n := f.count(`SELECT count(*) FROM lessons`); n != 4 {
		t.Fatalf("lessons table changed: %d", n)
	}
	// Queries that fold to fewer than 2 characters are refused, not matched
	// against every title (strpos with an empty string).
	for name, q := range map[string]string{
		"harakat only":            "\u064b\u064b\u064e",
		"tatweel only":            "\u0640\u0640\u0640",
		"combining accents only":  "\u0301\u0300\u0302",
		"one letter with harakat": "\u0628\u064e\u064b",
		"one letter with accents": "e\u0301\u0301",
	} {
		w := f.get(f.student, "/learn/search?q="+url.QueryEscape(q))
		wantPage(t, name, w, http.StatusOK, "Saisissez entre 2 et 100 caractères")
		if n := strings.Count(w.Body.String(), `<li class="card">`); n != 0 {
			t.Errorf("%s: %d results", name, n)
		}
		if _, err := reader.NewStore(f.db).Search(context.Background(), q); !errors.Is(err, reader.ErrInvalidQuery) {
			t.Errorf("%s: store error %v", name, err)
		}
	}
	wantPage(t, "one character", f.get(f.student, "/learn/search?q=a"), http.StatusOK, "Saisissez entre 2 et 100 caractères")
	wantPage(t, "too long", f.get(f.student, "/learn/search?q="+strings.Repeat("a", 101)), http.StatusOK, "Saisissez entre 2 et 100 caractères")
	wantPage(t, "empty", f.get(f.student, "/learn/search"), http.StatusOK, "Rechercher une leçon")
	// The query is echoed escaped.
	w := f.get(f.student, "/learn/search?q="+url.QueryEscape(`<img src=x onerror=alert(1)>`))
	wantAbsent(t, "echo", w, "<img src=x")
	// Results are bounded.
	results, err := reader.NewStore(f.db).Search(context.Background(), "TEST")
	if err != nil || len(results) != 4 || len(results) > reader.MaxSearchResults {
		t.Fatalf("store search: %d %v", len(results), err)
	}
}

func TestSignInAndOutPages(t *testing.T) {
	f := newReader(t)
	p := f.publishNew("TEST Après connexion", "fr", nil)
	login := f.get(apiUser{}, "/login?next="+url.QueryEscape(lessonPath(p.publicID)))
	wantPage(t, "login page", login, http.StatusOK, `name="next" value="`+lessonPath(p.publicID)+`"`, `autocomplete="current-password"`)
	if csp := login.Header().Get("Content-Security-Policy"); strings.Contains(csp, "script-src") {
		t.Errorf("login page allows scripts: %q", csp)
	}
	form := func(email, pw, next string) url.Values {
		return url.Values{"email": {email}, "password": {pw}, "next": {next}}
	}
	w := f.post(apiUser{}, "/login", form("test-student@example.test", "wrong-password-123", "/learn"))
	wantPage(t, "wrong password", w, http.StatusUnauthorized, "Adresse e-mail ou mot de passe incorrect", `value="test-student@example.test"`)
	w = f.post(apiUser{}, "/login", form("nobody@example.test", testPassword, "/learn"))
	wantPage(t, "unknown email", w, http.StatusUnauthorized, "Adresse e-mail ou mot de passe incorrect")
	wantPage(t, "empty", f.post(apiUser{}, "/login", url.Values{}), http.StatusBadRequest)

	w = f.post(apiUser{}, "/login", form("test-student@example.test", testPassword, lessonPath(p.publicID)))
	wantRedirect(t, "login", w, lessonPath(p.publicID))
	u := apiUser{cookie: sessionCookie(t, w).Value}
	for _, next := range []string{"//evil.test/learn", "https://evil.test", "/learn/../api/review/queue", "/learn?x=1", "/api/auth/session", `/learn\evil`, "/learn//x"} {
		wantRedirect(t, "next "+next, f.get(u, "/login?next="+url.QueryEscape(next)), "/learn")
	}

	// Sign-out needs the session's CSRF token.
	s := f.api(u, http.MethodGet, "/api/auth/session", nil)
	var b sessionBody
	_ = json.Unmarshal(s.Body.Bytes(), &b)
	u.csrf = b.CSRFToken
	wantPage(t, "logout without CSRF", f.page(u, http.MethodPost, "/logout", url.Values{}), http.StatusForbidden)
	before := f.sessionCount()
	wantRedirect(t, "logout", f.post(u, "/logout", nil), "/login")
	if f.sessionCount() != before-1 {
		t.Fatal("session not deleted")
	}
	wantRedirect(t, "after logout", f.get(u, "/learn"), "/login?next=%2Flearn")

	// The sign-in page shares the login throttle.
	for range 5 {
		f.post(apiUser{}, "/login", form("throttled@example.test", "wrong-password-123", "/learn"))
	}
	w = f.post(apiUser{}, "/login", form("throttled@example.test", "wrong-password-123", "/learn"))
	wantPage(t, "throttled", w, http.StatusTooManyRequests, "Trop de tentatives")
	if w.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
}

var hiddenToken = regexp.MustCompile(`name="token" value="([^"]+)"`)

func TestInvitationPages(t *testing.T) {
	f := newReader(t)
	ctx := context.Background()
	token, _, err := f.inv.Issue(ctx, "new-student@example.test", "test-operator@test-host", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w := f.get(apiUser{}, "/invite/"+token)
	wantPage(t, "invite page", w, http.StatusOK, "new-student@example.test", `autocomplete="new-password"`)
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("Referrer-Policy %q", w.Header().Get("Referrer-Policy"))
	}
	if m := hiddenToken.FindStringSubmatch(w.Body.String()); m == nil || m[1] != token {
		t.Fatal("token not carried in the form body")
	}
	accept := func(fields url.Values) *httptest.ResponseRecorder {
		fields.Set("token", token)
		return f.post(apiUser{}, "/invite", fields)
	}
	for name, tc := range map[string]struct {
		fields url.Values
		want   string
	}{
		"short password": {url.Values{"display_name": {"TEST Nouveau"}, "password": {"short"}, "password_confirm": {"short"}}, "au moins 12"},
		"mismatch":       {url.Values{"display_name": {"TEST Nouveau"}, "password": {testPassword}, "password_confirm": {testPassword + "x"}}, "ne correspondent pas"},
		"empty name":     {url.Values{"display_name": {"  "}, "password": {testPassword}, "password_confirm": {testPassword}}, "Saisissez un nom"},
		"bidi name":      {url.Values{"display_name": {"TEST ‮eman"}, "password": {testPassword}, "password_confirm": {testPassword}}, "Saisissez un nom"},
	} {
		w := accept(tc.fields)
		wantPage(t, name, w, http.StatusUnprocessableEntity, tc.want, "new-student@example.test")
		wantAbsent(t, name, w, testPassword)
	}
	if n := f.count(`SELECT count(*) FROM users WHERE email = 'new-student@example.test'`); n != 0 {
		t.Fatal("account created by an invalid submission")
	}
	w = accept(url.Values{"display_name": {"TEST Nouveau"}, "password": {testPassword}, "password_confirm": {testPassword}})
	wantRedirect(t, "accept", w, "/learn")
	student := apiUser{cookie: sessionCookie(t, w).Value}
	wantPage(t, "dashboard", f.get(student, "/learn"), http.StatusOK, "Bonjour <bdi>TEST Nouveau</bdi>")
	// Single use: the link and the form are dead now, with the same message
	// as unknown and expired tokens.
	for name, w := range map[string]*httptest.ResponseRecorder{
		"reused link": f.get(apiUser{}, "/invite/"+token),
		"reused form": accept(url.Values{"display_name": {"TEST Autre"}, "password": {testPassword}, "password_confirm": {testPassword}}),
		"wrong token": f.get(apiUser{}, "/invite/"+strings.Repeat("A", 43)),
		"malformed":   f.get(apiUser{}, "/invite/abc"),
	} {
		wantPage(t, name, w, http.StatusNotFound, "Ce lien d’invitation ne fonctionne plus")
	}
	past := invitations.NewStore(f.db, func() time.Time { return time.Now().Add(-48 * time.Hour) })
	expired, _, err := past.Issue(ctx, "late-student@example.test", "test-operator@test-host", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, "expired link", f.get(apiUser{}, "/invite/"+expired), http.StatusNotFound, "Ce lien d’invitation ne fonctionne plus")
	wantPage(t, "expired form", f.post(apiUser{}, "/invite", url.Values{"token": {expired}, "display_name": {"TEST"},
		"password": {testPassword}, "password_confirm": {testPassword}}), http.StatusNotFound, "Ce lien d’invitation ne fonctionne plus")
	if n := f.count(`SELECT count(*) FROM users WHERE email = 'late-student@example.test'`); n != 0 {
		t.Fatal("expired invitation created an account")
	}
	// A cross-site form post is rejected before anything else.
	w = f.page(apiUser{}, http.MethodPost, "/invite", url.Values{"token": {token}}, withHeader("Sec-Fetch-Site", "cross-site"))
	wantStatus(t, "cross-site invite", w, http.StatusForbidden, "cross_origin_rejected")
}

func TestStudentsHaveNoEditorialAccess(t *testing.T) {
	f := newReader(t)
	if n := f.count(`SELECT (SELECT count(*) FROM user_permissions WHERE user_id = $1) + (SELECT count(*) FROM reviewer_assignments WHERE user_id = $1)`, f.student.id); n != 0 {
		t.Fatalf("student holds %d permissions", n)
	}
	rev := f.approved(0, readerDoc("TEST À publier", "fr"), "fr", nil)
	for _, ep := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/lessons", map[string]any{"subjectId": f.subject, "language": "fr", "content": testDoc("TEST", "fr")}},
		{http.MethodGet, "/api/review/queue", nil},
		{http.MethodPost, publishPath(rev.ID), map[string]any{}},
	} {
		w := f.api(f.student, ep.method, ep.path, ep.body)
		if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
			t.Errorf("student %s %s: %d", ep.method, ep.path, w.Code)
		}
	}
	if n := f.count(`SELECT count(*) FROM publication_audit`); n != 0 {
		t.Fatal("student published")
	}
}

func TestInvitationAttemptsAreThrottled(t *testing.T) {
	f := newReader(t)
	addr := withAddr("203.0.113.77")
	for i := range f.cfg.InvitationLimit {
		if w := f.page(apiUser{}, http.MethodGet, "/invite/"+strings.Repeat("B", 43), nil, addr); w.Code != http.StatusNotFound {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := f.page(apiUser{}, http.MethodPost, "/invite", url.Values{"token": {strings.Repeat("B", 43)}}, addr)
	wantPage(t, "throttled", w, http.StatusTooManyRequests, "Trop de tentatives")
	if w.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// Another address is unaffected; the limit lifts after the window.
	if w := f.page(apiUser{}, http.MethodGet, "/invite/"+strings.Repeat("B", 43), nil, withAddr("203.0.113.78")); w.Code != http.StatusNotFound {
		t.Fatalf("other address: %d", w.Code)
	}
	f.clock.Advance(f.cfg.ThrottleWindow + time.Second)
	if w := f.page(apiUser{}, http.MethodGet, "/invite/"+strings.Repeat("B", 43), nil, addr); w.Code != http.StatusNotFound {
		t.Fatalf("after window: %d", w.Code)
	}
}

// A session lookup that fails (here: the database pool is closed) still
// answers reader routes with the reader's private, no-store headers, and
// leaks nothing. Unsupported methods on reader paths fall through to the
// server's JSON 404, which carries the server-wide no-store.
func TestReaderHeadersOnSessionLookupFailure(t *testing.T) {
	e := newEnv(t)
	w := e.page(apiUser{}, http.MethodPut, "/learn", nil)
	wantStatus(t, "PUT /learn", w, http.StatusNotFound, "not_found")
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("PUT /learn: Cache-Control %q", cc)
	}
	e.db.Close()
	cookie := apiUser{cookie: strings.Repeat("A", 43)}
	for _, path := range []string{"/learn", "/learn/search?q=TEST", "/login", "/static/complex-plane.svg"} {
		w := e.page(cookie, http.MethodGet, path, nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("%s: status %d, want 500", path, w.Code)
		}
		checkReaderHeaders(t, path, w)
	}
}
