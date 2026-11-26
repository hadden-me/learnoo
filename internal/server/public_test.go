package server_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/reader"
)

// Public homepage, catalogue, course overviews and covers (LMS-B07) through
// the real server handler. TEST users, curriculum entries, lessons and
// courses only.

type publicEnv struct {
	readerFixture
	cs      *courses.Store
	cur     *curriculum.Store
	section int64
}

func newPublic(t *testing.T) publicEnv {
	f := newReader(t)
	fr, ar := "TEST Mathématiques", "TEST رياضيات"
	if _, _, err := catalogue.NewStore(f.db).SetSubjectNames(context.Background(), testOperator, "test-math", &fr, &ar); err != nil {
		t.Fatal(err)
	}
	return publicFrom(f)
}

// publicFrom adds a TEST level and section to f.
func publicFrom(f readerFixture) publicEnv {
	f.t.Helper()
	p := publicEnv{readerFixture: f, cs: courses.NewStore(f.db), cur: curriculum.NewStore(f.db)}
	ctx := context.Background()
	admin := permissions.UserActor(f.admin.id)
	lv, err := p.cur.CreateLevel(ctx, admin, curriculum.Entry{Code: "test-bac", NameFR: "TEST Baccalauréat", NameAR: "TEST باكالوريا"})
	if err != nil {
		f.t.Fatal(err)
	}
	sec, err := p.cur.CreateSection(ctx, admin, lv.ID, curriculum.Entry{Code: "test-maths", NameFR: "TEST Section mathématiques", NameAR: "TEST شعبة الرياضيات"})
	if err != nil {
		f.t.Fatal(err)
	}
	p.section = sec.ID
	return p
}

// seedBrowserCatalogue gives the Playwright reader fixture
// (TestBrowserReaderServer) a current TEST edition placing chapter and a
// published TEST course with a cover whose step is lesson.
func seedBrowserCatalogue(f readerFixture, chapter int64, lesson published) {
	f.t.Helper()
	p := publicFrom(f)
	_, asg := p.edition("test-2026", "TEST Édition 2026", true, nil, chapter)
	cover := p.uploadAs(p.author, 21)
	p.publishedCourse(courseSpec{title: "TEST Cours de nombres complexes", summary: "TEST Un parcours court sur les nombres complexes.",
		assignment: asg, cover: &cover, steps: []published{lesson}})
}

// edition creates an edition assigning the fixture subject with chapters;
// with current set it is made current (superseding expectedCurrent).
func (p publicEnv) edition(code, label string, current bool, expectedCurrent *int64, chapters ...int64) (edition, assignment int64) {
	p.t.Helper()
	ctx := context.Background()
	admin := permissions.UserActor(p.admin.id)
	ed, err := p.cur.CreateEdition(ctx, admin, p.section, curriculum.NewEdition{Code: code, LabelFR: label, LabelAR: "TEST طبعة"})
	if err != nil {
		p.t.Fatal(err)
	}
	ed, asg, err := p.cur.AddAssignment(ctx, admin, ed.ID, ed.Version, p.subject)
	if err != nil {
		p.t.Fatal(err)
	}
	if len(chapters) > 0 {
		if ed, err = p.cur.SetChapters(ctx, admin, ed.ID, asg.ID, ed.Version, chapters); err != nil {
			p.t.Fatal(err)
		}
	}
	if current {
		if _, _, err = p.cur.MakeCurrent(ctx, admin, ed.ID, ed.Version, expectedCurrent); err != nil {
			p.t.Fatal(err)
		}
	}
	return ed.ID, asg.ID
}

type courseSpec struct {
	title, summary, language string
	assignment               int64
	cover                    *int64
	steps                    []published
}

func (p publicEnv) draft(rev courses.Revision, s courseSpec) courses.Revision {
	p.t.Helper()
	d := courses.Draft{Title: s.title, Summary: s.summary, Objectives: "TEST objectifs du cours\nSavoir calculer.",
		Prerequisites: "TEST prérequis du cours", AssignmentIDs: []int64{s.assignment}}
	if s.cover != nil {
		alt := "TEST description de la couverture"
		d.CoverMediaID, d.CoverAlt = s.cover, &alt
	}
	m := courses.ModuleInput{Title: "TEST Module " + s.title}
	for i, l := range s.steps {
		m.Steps = append(m.Steps, courses.StepInput{LessonID: l.lessonID, RevisionID: l.revisionID, Title: "TEST Étape " + string(rune('A'+i)) + " " + s.title})
	}
	d.Modules = []courses.ModuleInput{m}
	rev, err := p.cs.SaveDraft(context.Background(), p.author.id, rev.ID, rev.Version, d)
	if err != nil {
		p.t.Fatal(err)
	}
	return rev
}

// approvedCourse creates a course (or, with courseID, a successor draft),
// saves s and takes it through submission and approval.
func (p publicEnv) approvedCourse(courseID int64, s courseSpec) (courses.Course, courses.Revision) {
	p.t.Helper()
	ctx := context.Background()
	var c courses.Course
	var rev courses.Revision
	var err error
	if courseID == 0 {
		lang := s.language
		if lang == "" {
			lang = "fr"
		}
		c, rev, err = p.cs.CreateCourse(ctx, p.author.id, p.subject, lang)
	} else {
		rev, err = p.cs.CreateDraft(ctx, p.author.id, courseID)
		c.ID = courseID
	}
	if err != nil {
		p.t.Fatal(err)
	}
	rev = p.draft(rev, s)
	if rev, err = p.cs.Submit(ctx, p.author.id, rev.ID, rev.Version); err != nil {
		p.t.Fatal(err)
	}
	if rev, err = p.cs.Approve(ctx, p.reviewer.id, rev.ID, ""); err != nil {
		p.t.Fatal(err)
	}
	if err := p.db.QueryRow(ctx, `SELECT public_id::text FROM courses WHERE id = $1`, rev.CourseID).Scan(&c.PublicID); err != nil {
		p.t.Fatal(err)
	}
	c.ID = rev.CourseID
	return c, rev
}

func (p publicEnv) publishCourse(rev courses.Revision, expected *int64) {
	p.t.Helper()
	if _, err := p.cs.Publish(context.Background(), p.publisher.id, rev.ID, expected); err != nil {
		p.t.Fatal(err)
	}
}

func (p publicEnv) publishedCourse(s courseSpec) (courses.Course, courses.Revision) {
	p.t.Helper()
	c, rev := p.approvedCourse(0, s)
	p.publishCourse(rev, nil)
	return c, rev
}

// public fetches a public page as u and checks the public cache rule: no
// session dependence, so no Vary and no cookie.
func (p publicEnv) public(u apiUser, path string, extra ...reqOpt) *httptest.ResponseRecorder {
	p.t.Helper()
	w := p.page(u, http.MethodGet, path, nil, extra...)
	checkPublicHeaders(p.t, "GET "+path, w)
	return w
}

func checkPublicHeaders(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code >= 500 {
		t.Fatalf("%s: %d %s", what, w.Code, w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != reader.PublicCacheControl {
		t.Errorf("%s: Cache-Control %q, want %q", what, cc, reader.PublicCacheControl)
	}
	if v := w.Header().Values("Vary"); len(v) != 0 {
		t.Errorf("%s: Vary %q", what, v)
	}
	if c := w.Header().Values("Set-Cookie"); len(c) != 0 {
		t.Errorf("%s: Set-Cookie %q", what, c)
	}
	if w.Code == http.StatusOK && w.Header().Get("ETag") == "" {
		t.Errorf("%s: no ETag", what)
	}
}

// viewerMarkers are strings that would reveal the viewer on a public page.
var viewerMarkers = []string{"csrf_token", "Se déconnecter", "/logout", "Mes leçons", "TEST Élève", "Bonjour"}

func TestPublicCatalogueAndCourseOverview(t *testing.T) {
	p := newPublic(t)
	ctx := context.Background()
	chA := p.chapter(p.subject, "ch-a")
	chB := p.chapter(p.subject, "ch-b")
	chD := p.chapter(p.subject, "ch-d") // in no edition
	chE := p.chapter(p.subject, "ch-e") // only in a draft edition
	fr, ar := "TEST Suites", "TEST المتتاليات"
	if _, _, _, err := catalogue.NewStore(p.db).SetChapterNames(ctx, testOperator, "test-math", "ch-a", &fr, &ar); err != nil {
		t.Fatal(err)
	}
	lA := p.publish(p.approved(0, readerDoc("TEST Titre leçon A", "fr"), "fr", &chA))
	lB := p.publish(p.approved(0, readerDoc("TEST Titre leçon B", "fr"), "fr", &chB))
	lAr := p.publish(p.approved(0, readerDoc("TEST عنوان درس", "ar"), "ar", &chA))
	p.publishNew("TEST Titre leçon sans chapitre", "fr", nil)
	p.publishNew("TEST Titre leçon hors programme", "fr", &chD)
	p.publishNew("TEST Titre leçon brouillon", "fr", &chE)
	// An unpublished lesson in a placed chapter is never listed.
	p.approved(0, readerDoc("TEST Titre leçon non publiée", "fr"), "fr", &chA)

	// Edition 2025 is current while course 1 is placed in it, then 2026
	// supersedes it; a draft edition lists chE.
	ed2025, asg2025 := p.edition("test-2025", "TEST Édition 2025", true, nil, chA)
	cover := p.uploadAs(p.author, 7)
	c1, _ := p.publishedCourse(courseSpec{title: "TEST Cours 2025", summary: "TEST résumé 2025", assignment: asg2025, steps: []published{lA}})
	_, asg2026 := p.edition("test-2026", "TEST Édition 2026", true, &ed2025, chA, chB)
	p.edition("test-draft", "TEST Édition brouillon", false, nil, chE)
	c2, _ := p.publishedCourse(courseSpec{title: "TEST Cours 2026", summary: "TEST résumé 2026\nSur deux lignes.", assignment: asg2026,
		cover: &cover, steps: []published{lA, lB}})
	c3, _ := p.publishedCourse(courseSpec{title: "TEST دورة", summary: "TEST ملخص", language: "ar", assignment: asg2026, steps: []published{lAr}})
	// Approved, never published: never listed or shown.
	cHidden, _ := p.approvedCourse(0, courseSpec{title: "TEST Cours non publié", summary: "TEST secret", assignment: asg2026, steps: []published{lB}})

	forged := apiUser{cookie: strings.Repeat("A", 43)}
	viewers := []apiUser{{}, forged, p.student, p.admin}
	pages := []string{"/", "/catalogue", "/catalogue/test-bac/test-maths", "/catalogue/test-bac/test-maths?edition=test-2025",
		"/catalogue/test-bac/test-maths/test-math", "/catalogue/test-bac/test-maths/test-math?edition=test-2025",
		"/courses/" + c1.PublicID, "/courses/" + c2.PublicID, "/courses/" + c3.PublicID}
	bodies := map[string]*httptest.ResponseRecorder{}
	for _, path := range pages {
		var first []byte
		for i, u := range viewers {
			w := p.public(u, path)
			wantPage(t, path, w, http.StatusOK)
			wantAbsent(t, path, w, viewerMarkers...)
			wantAbsent(t, path, w, "Texte de test", "Objectif de test", "TEST Cours non publié", "TEST secret",
				"TEST Titre leçon non publiée", "test-bac<", "inscription</a>", "S’inscrire", "/static/landing", "renderer")
			if i == 0 {
				first = w.Body.Bytes()
				bodies[path] = w
			} else if !bytes.Equal(first, w.Body.Bytes()) {
				t.Errorf("%s: body differs between viewers", path)
			}
			// Every use revalidates: a matching ETag gets 304, for anyone.
			nm := p.page(u, http.MethodGet, path, nil, withHeader("If-None-Match", w.Header().Get("ETag")))
			if nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
				t.Errorf("%s: revalidation %d", path, nm.Code)
			}
			checkPublicHeaders(t, "revalidate "+path, nm)
		}
		if h := p.page(apiUser{}, http.MethodHead, path, nil); h.Code != http.StatusOK || h.Body.Len() != 0 {
			t.Errorf("HEAD %s: %d", path, h.Code)
		}
	}

	// Homepage: the service, the curriculum actually available, the
	// review process, contribution, honest invitation-only access.
	wantPage(t, "home", bodies["/"], http.StatusOK, `<a class="action primary" href="/catalogue">`, `<a class="action" href="/login">Se connecter</a>`,
		`href="/login?next=%2Fstudio"`, "TEST Baccalauréat › TEST Section mathématiques", "TEST Édition 2026 (en vigueur)",
		`<a href="/catalogue/test-bac/test-maths/test-math">TEST Mathématiques</a> : 2 cours, 3 leçons`, "sur invitation", "Comment les contenus sont relus", "Contribuer")
	wantAbsent(t, "home", bodies["/"], "TEST Édition 2025", "TEST Édition brouillon")

	// Catalogue index: sections with their current edition; unplaced
	// lessons (no chapter, a chapter in no edition or only in a draft one).
	wantPage(t, "catalogue", bodies["/catalogue"], http.StatusOK, `<h2>TEST Baccalauréat</h2>`,
		`<a href="/catalogue/test-bac/test-maths">TEST Section mathématiques</a>`, "TEST Édition 2026 (en vigueur)",
		"Leçons pas encore placées dans une édition du programme", "TEST Titre leçon sans chapitre", "TEST Titre leçon hors programme", "TEST Titre leçon brouillon")
	wantAbsent(t, "catalogue", bodies["/catalogue"], "TEST Titre leçon A", "TEST Titre leçon B")

	// Section: current edition by default, subjects by name with real
	// counts, earlier editions behind an explicit labelled link; drafts
	// never.
	sec := bodies["/catalogue/test-bac/test-maths"]
	wantPage(t, "section", sec, http.StatusOK, "<h1>TEST Section mathématiques</h1>", "Édition : TEST Édition 2026 (en vigueur)",
		`<a href="/catalogue/test-bac/test-maths/test-math">TEST Mathématiques</a><span class="context">2 cours, 3 leçons</span>`,
		"Éditions antérieures", `<a href="/catalogue/test-bac/test-maths?edition=test-2025">TEST Édition 2025</a><span class="context">remplacée</span>`)
	wantAbsent(t, "section", sec, "test-draft", "Édition antérieure :")
	old := bodies["/catalogue/test-bac/test-maths?edition=test-2025"]
	wantPage(t, "earlier section", old, http.StatusOK, "Édition antérieure : TEST Édition 2025 (remplacée).",
		`<a href="/catalogue/test-bac/test-maths">Voir l’édition en vigueur : TEST Édition 2026</a>`,
		`<a href="/catalogue/test-bac/test-maths/test-math?edition=test-2025">TEST Mathématiques</a><span class="context">1 cours, 2 leçons</span>`)

	// Subject in the current edition: courses and lessons listed
	// separately and labelled; lessons by the assignment's chapters.
	subj := bodies["/catalogue/test-bac/test-maths/test-math"]
	wantInOrder(t, "subject", subj.Body.String(), "<h1>TEST Mathématiques</h1>", "Édition : TEST Édition 2026 (en vigueur)",
		`<h2 id="courses-heading">Cours</h2>`, `<span class="kind">Cours</span><a href="/courses/`+c2.PublicID+`" lang="fr" dir="ltr">TEST Cours 2026</a>`,
		`<h2 id="lessons-heading">Leçons</h2>`, "<h3>TEST Suites</h3>", `<span class="kind lesson">Leçon</span><a href="/learn/lessons/`+lA.publicID+`" lang="fr" dir="ltr">TEST Titre leçon A</a>`,
		"<h3>ch-b</h3>", "TEST Titre leçon B", "Leçons pas encore placées", "TEST Titre leçon brouillon")
	wantPage(t, "subject", subj, http.StatusOK, `<a href="/courses/`+c3.PublicID+`" lang="ar" dir="rtl">TEST دورة</a>`,
		`<p class="summary multiline" lang="fr" dir="ltr">TEST résumé 2026`)
	wantAbsent(t, "subject", subj, "TEST Cours 2025", cHidden.PublicID)
	subjOld := bodies["/catalogue/test-bac/test-maths/test-math?edition=test-2025"]
	wantPage(t, "earlier subject", subjOld, http.StatusOK, "Édition antérieure : TEST Édition 2025 (remplacée).", "TEST Cours 2025", "TEST Titre leçon A",
		`<a href="/catalogue/test-bac/test-maths?edition=test-2025">TEST Section mathématiques</a>`)
	wantAbsent(t, "earlier subject", subjOld, "TEST Cours 2026", "TEST Titre leçon B")

	// Course overview: reviewed metadata, placement with the edition
	// label, outline of module and course-authored step titles only,
	// credit, language, cover, and a sign-in link carrying the return path.
	co := bodies["/courses/"+c2.PublicID]
	wantPage(t, "course", co, http.StatusOK, `<h1 id="course-title" lang="fr" dir="ltr">TEST Cours 2026</h1>`,
		`<img src="/courses/`+c2.PublicID+`/cover" alt="TEST description de la couverture" lang="fr">`,
		"TEST résumé 2026\nSur deux lignes.", "TEST objectifs du cours\nSavoir calculer.", "TEST prérequis du cours",
		"<dd>Français</dd>", `<a href="/catalogue/test-bac/test-maths/test-math">TEST Baccalauréat › TEST Section mathématiques</a> · TEST Édition 2026 (en vigueur)`,
		"TEST Module TEST Cours 2026", "TEST Étape A TEST Cours 2026", "TEST Étape B TEST Cours 2026",
		`Conçu par <bdi class="author">TEST Autrice Amel</bdi>`, `relue et approuvée par <bdi class="reviewer">TEST Relecteur Karim</bdi>`,
		`href="/login?next=%2Flearn%2Fcourses%2F`+c2.PublicID+`"`)
	wantAbsent(t, "course", co, "TEST Titre leçon", "temporairement indisponible", lA.publicID, lB.publicID)
	wantPage(t, "earlier course", bodies["/courses/"+c1.PublicID], http.StatusOK,
		`<a href="/catalogue/test-bac/test-maths/test-math?edition=test-2025">TEST Baccalauréat › TEST Section mathématiques</a> · TEST Édition 2025 (remplacée)`)
	wantAbsent(t, "earlier course", bodies["/courses/"+c1.PublicID], "<img")
	wantPage(t, "arabic course", bodies["/courses/"+c3.PublicID], http.StatusOK, `<title lang="ar">TEST دورة · Learnoo</title>`,
		`<h1 id="course-title" lang="ar" dir="rtl">TEST دورة</h1>`, `<span lang="ar" dir="rtl">TEST Étape A TEST دورة</span>`, "<dd>Arabe</dd>")

	// Not found: malformed, unknown, drafts and unassigned subjects,
	// ambiguous edition parameters.
	for _, path := range []string{"/catalogue/test-bac/nope", "/catalogue/TEST/test-maths", "/catalogue/test-bac/test-maths?edition=test-draft",
		"/catalogue/test-bac/test-maths?edition=nope", "/catalogue/test-bac/test-maths?edition=", "/catalogue/test-bac/test-maths?edition=test-2025&edition=test-2026",
		"/catalogue/test-bac/test-maths/test-physique", "/catalogue/test-bac/test-maths/test-math?edition=test-draft",
		"/courses/" + cHidden.PublicID, "/courses/00000000-0000-0000-0000-000000000000", "/courses/not-a-uuid"} {
		w := p.public(apiUser{}, path)
		wantPage(t, path, w, http.StatusNotFound)
		wantAbsent(t, path, w, "TEST Cours", "TEST secret")
	}
}

func TestPublicCourseUnavailableAndCover(t *testing.T) {
	p := newPublic(t)
	ch := p.chapter(p.subject, "ch-a")
	_, asg := p.edition("test-2026", "TEST Édition 2026", true, nil, ch)
	lA := p.publish(p.approved(0, readerDoc("TEST Titre leçon A", "fr"), "fr", &ch))
	lB := p.publish(p.approved(0, readerDoc("TEST Titre leçon B", "fr"), "fr", &ch))
	cover := p.uploadAs(p.author, 11)
	c, rev1 := p.publishedCourse(courseSpec{title: "TEST Cours version 1", summary: "TEST résumé v1", assignment: asg, cover: &cover, steps: []published{lA, lB}})
	coursePath, coverPath := "/courses/"+c.PublicID, "/courses/"+c.PublicID+"/cover"
	subjPath := "/catalogue/test-bac/test-maths/test-math"

	// The cover of a readable course: public, revalidated, for anyone.
	var etag string
	for _, u := range []apiUser{{}, p.student} {
		w := p.public(u, coverPath)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), pngBytes(t, 11)) {
			t.Fatalf("cover: %d %q", w.Code, w.Header().Get("Content-Type"))
		}
		etag = w.Header().Get("ETag")
	}
	if w := p.public(apiUser{}, coverPath, withHeader("If-None-Match", etag)); w.Code != http.StatusNotModified {
		t.Fatalf("cover revalidation: %d", w.Code)
	}
	for _, path := range []string{"/courses/not-a-uuid/cover", "/courses/00000000-0000-0000-0000-000000000000/cover"} {
		if w := p.public(apiUser{}, path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, w.Code)
		}
	}

	// A successor in review and a later approved candidate change
	// nothing; publishing it replaces the old revision, without fallback.
	_, rev2 := p.approvedCourse(c.ID, courseSpec{title: "TEST Cours version 2", summary: "TEST résumé v2", assignment: asg, steps: []published{lA, lB}})
	w := p.public(apiUser{}, coursePath)
	wantPage(t, "candidate pending", w, http.StatusOK, "TEST Cours version 1")
	wantAbsent(t, "candidate pending", w, "TEST Cours version 2")
	p.publishCourse(rev2, &rev1.ID)
	w = p.public(apiUser{}, coursePath)
	wantPage(t, "republished", w, http.StatusOK, "TEST Cours version 2")
	wantAbsent(t, "republished", w, "TEST Cours version 1", "TEST résumé v1", "<img")
	// The new revision has no cover; the earlier revision's is not served.
	if w := p.public(apiUser{}, coverPath, withHeader("If-None-Match", etag)); w.Code != http.StatusNotFound {
		t.Fatalf("earlier revision's cover: %d", w.Code)
	}

	// A withdrawn pinned lesson revision and an unpublished lesson: the
	// step keeps its course-authored title with a notice; the lesson title
	// is never shown; the overview stays readable.
	if _, err := p.rs.Withdraw(context.Background(), p.admin.id, lA.revisionID, "TEST withdrawal", &lA.revisionID); err != nil {
		t.Fatal(err)
	}
	p.unpublish(lB.lessonID, lB.revisionID)
	w = p.public(apiUser{}, coursePath)
	wantPage(t, "unavailable steps", w, http.StatusOK, "TEST Étape A TEST Cours version 2</span> <span class=\"unavailable\">Leçon temporairement indisponible</span>",
		"TEST Étape B TEST Cours version 2</span> <span class=\"unavailable\">Leçon temporairement indisponible</span>")
	wantAbsent(t, "unavailable steps", w, "TEST Titre leçon", "Texte de test")
	wantAbsent(t, "subject without the lessons", p.public(apiUser{}, subjPath), "TEST Titre leçon")

	// Suspension hides the course everywhere at once: overview, catalogue,
	// counts; a revalidation gets 404, not 304.
	before := p.public(apiUser{}, coursePath).Header().Get("ETag")
	if _, err := p.cs.Suspend(context.Background(), p.admin.id, c.ID, &rev2.ID, "TEST suspension"); err != nil {
		t.Fatal(err)
	}
	for _, u := range []apiUser{{}, p.student, p.admin} {
		w = p.public(u, coursePath, withHeader("If-None-Match", before))
		wantPage(t, "suspended", w, http.StatusNotFound, "Cours indisponible", "Ce cours n’existe pas ou n’est pas disponible pour le moment.")
		wantAbsent(t, "suspended", w, "TEST Cours version", "TEST Étape", "TEST Module", "TEST résumé", "TEST Autrice")
		wantAbsent(t, "suspended", w, viewerMarkers...)
	}
	w = p.public(apiUser{}, subjPath)
	wantPage(t, "catalogue after suspension", w, http.StatusOK, "Aucun cours relu n’est encore publié pour cette matière dans cette édition.")
	wantAbsent(t, "catalogue after suspension", w, "TEST Cours version")
	wantPage(t, "section after suspension", p.public(apiUser{}, "/catalogue/test-bac/test-maths"), http.StatusOK, "Aucun contenu relu pour l’instant")

	// An archived course (published with a cover first): no overview, no
	// cover, even for a request that had a valid ETag.
	other, orev := p.publishedCourse(courseSpec{title: "TEST Cours archivé", summary: "TEST résumé archivé", assignment: asg, cover: &cover,
		steps: []published{p.publishNew("TEST Titre leçon C", "fr", &ch)}})
	ocover := "/courses/" + other.PublicID + "/cover"
	oetag := p.public(apiUser{}, ocover).Header().Get("ETag")
	if _, err := p.cs.Archive(context.Background(), p.admin.id, other.ID, &orev.ID, "TEST archive"); err != nil {
		t.Fatal(err)
	}
	wantAbsent(t, "archived", p.public(apiUser{}, "/courses/"+other.PublicID), "TEST Cours archivé", "TEST résumé archivé")
	for _, path := range []string{ocover, coverPath} {
		if w := p.public(p.student, path, withHeader("If-None-Match", oetag)); w.Code != http.StatusNotFound {
			t.Errorf("%s after archive/suspension: %d", path, w.Code)
		}
	}

	// A draft course with a cover: nothing public.
	draft, drev, err := p.cs.CreateCourse(context.Background(), p.author.id, p.subject, "fr")
	if err != nil {
		t.Fatal(err)
	}
	p.draft(drev, courseSpec{title: "TEST Cours brouillon", assignment: asg, cover: &cover, steps: []published{lB}})
	wantAbsent(t, "draft course", p.public(apiUser{}, "/courses/"+draft.PublicID), "TEST Cours brouillon")
	if w := p.public(apiUser{}, "/courses/"+draft.PublicID+"/cover"); w.Code != http.StatusNotFound {
		t.Fatalf("draft cover: %d", w.Code)
	}
}

func TestPublicSignInReturnPath(t *testing.T) {
	f := newReader(t)
	course := "/learn/courses/0b0e2b4e-6f1a-4c55-9d5c-2f0d7d3c9a11"
	if got := reader.SafeNext(course); got != course {
		t.Fatalf("SafeNext(%q) = %q", course, got)
	}
	// The sign-in page keeps a safe local return path and refuses others.
	cases := map[string]string{
		course:                          course,
		"//evil.example/learn":          "/learn",
		"https://evil.example/learn":    "/learn",
		"/learn/../studio":              "/learn",
		"/learn/courses/x?next=//evil":  "/learn",
		`/\evil.example`:                "/learn",
		"/courses/" + course[15:]:       "/learn",
		"/learn/courses/%2F%2Fevil.com": "/learn",
		"javascript:alert(1)":           "/learn",
	}
	for next, want := range cases {
		w := f.get(apiUser{}, "/login?next="+url.QueryEscape(next))
		wantPage(t, "login next "+next, w, http.StatusOK, `<input type="hidden" name="next" value="`+want+`">`)
		form := url.Values{"email": {"test-student@example.test"}, "password": {testPassword}, "next": {next}}
		w = f.page(apiUser{}, http.MethodPost, "/login", form, withAddr("203.0.113.77"))
		wantRedirect(t, "login submit "+next, w, want)
	}
}
