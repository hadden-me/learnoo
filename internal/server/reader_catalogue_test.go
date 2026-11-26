package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// Catalogue display names and order in the reader (LRN-014, issue #86).
// Names are TEST fixtures set through the audited operator path, not
// curriculum data.

// wantInOrder fails unless the body contains every string in this order.
func wantInOrder(t *testing.T, what, body string, parts ...string) {
	t.Helper()
	pos := 0
	for _, p := range parts {
		i := strings.Index(body[pos:], p)
		if i < 0 {
			t.Fatalf("%s: %q missing or out of order (after byte %d)", what, p, pos)
		}
		pos += i + len(p)
	}
}

func TestReaderShowsCatalogueNamesInOrder(t *testing.T) {
	f := newReader(t) // subject test-math
	ctx := context.Background()
	cat := catalogue.NewStore(f.db)
	name := func(s string) *string { return &s }
	physics := f.newSubject("test-physics")
	chA := f.chapter(f.subject, "ch-a")
	chB := f.chapter(f.subject, "ch-b")
	chC := f.chapter(f.subject, "ch-c")
	fr := f.publishNew("TEST Leçon A", "fr", &chA)
	ar := f.publishNew("TEST درس ب", "ar", &chB)
	f.publishNew("TEST Leçon C", "fr", &chC)
	f.publishNew("TEST Leçon sans chapitre", "fr", nil)
	g := f
	g.subject, g.reviewer = physics, f.newUser("test-reviewer-physics", physics, permissions.Reviewer)
	g.publishNew("TEST Physique", "fr", nil)

	// Without names: codes, by code.
	dash := f.get(f.student, "/learn")
	wantInOrder(t, "dashboard by code", dash.Body.String(), `>test-math</a>`, `>test-physics</a>`)

	// Names and order, by the audited operator path.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := cat.SetSubjectNames(ctx, testOperator, "test-math", name("TEST Mathématiques"), name("TEST رياضيات"))
	must(err)
	_, _, err = cat.SetSubjectNames(ctx, testOperator, "test-physics", nil, name("TEST فيزياء"))
	must(err)
	_, _, err = cat.SetSubjectOrder(ctx, testOperator, "test-math", 2)
	must(err)
	_, _, err = cat.SetSubjectOrder(ctx, testOperator, "test-physics", 1)
	must(err)
	_, _, _, err = cat.SetChapterNames(ctx, testOperator, "test-math", "ch-a", name("TEST Suites"), name("TEST المتتاليات"))
	must(err)
	_, _, _, err = cat.SetChapterNames(ctx, testOperator, "test-math", "ch-b", nil, name("TEST الأعداد العقدية"))
	must(err)
	_, _, _, err = cat.SetChapterOrder(ctx, testOperator, "test-math", "ch-a", 3)
	must(err)
	_, _, _, err = cat.SetChapterOrder(ctx, testOperator, "test-math", "ch-c", 1)
	must(err)

	// Dashboard: subjects by order; French name, else the Arabic one
	// isolated with its language and direction.
	dash = f.get(f.student, "/learn")
	wantInOrder(t, "dashboard", dash.Body.String(),
		`<a href="/learn/subjects/test-physics"><bdi lang="ar" dir="rtl">TEST فيزياء</bdi></a>`,
		`<a href="/learn/subjects/test-math">TEST Mathématiques</a>`)

	// Subject page: heading and breadcrumb named; chapters by order, then
	// code (ch-b 0, ch-c 1, ch-a 3), then lessons without a chapter.
	subj := f.get(f.student, "/learn/subjects/test-math")
	wantPage(t, "subject", subj, http.StatusOK, `<title>TEST Mathématiques · Learnoo</title>`,
		`<span aria-current="page">TEST Mathématiques</span>`, `<h1>TEST Mathématiques</h1>`)
	wantInOrder(t, "subject chapters", subj.Body.String(),
		`<h2><bdi lang="ar" dir="rtl">TEST الأعداد العقدية</bdi></h2>`, // ch-b, order 0, Arabic only
		`<h2>ch-c</h2>`,        // order 1, no name: the code
		`<h2>TEST Suites</h2>`, // order 3
		`<h2>Sans chapitre</h2>`)

	// A subject named only in Arabic: its page title says so.
	wantPage(t, "arabic-only subject", f.get(f.student, "/learn/subjects/test-physics"), http.StatusOK,
		`<title lang="ar">TEST فيزياء · Learnoo</title>`, `<h1><bdi lang="ar" dir="rtl">TEST فيزياء</bdi></h1>`)

	// Lesson pages: the breadcrumb names the subject and chapter in the
	// lesson's language when set.
	wantPage(t, "french lesson", f.get(f.student, lessonPath(fr.publicID)), http.StatusOK,
		`<a href="/learn/subjects/test-math">TEST Mathématiques</a> › <span>TEST Suites</span>`)
	wantPage(t, "arabic lesson", f.get(f.student, lessonPath(ar.publicID)), http.StatusOK,
		`<a href="/learn/subjects/test-math"><bdi lang="ar" dir="rtl">TEST رياضيات</bdi></a> › <span><bdi lang="ar" dir="rtl">TEST الأعداد العقدية</bdi></span>`)

	// Search results and bookmarks carry the same context.
	search := f.get(f.student, "/learn/search?q="+url.QueryEscape("TEST"))
	wantPage(t, "search", search, http.StatusOK,
		`<span class="context"><bdi lang="ar" dir="rtl">TEST رياضيات</bdi> › <bdi lang="ar" dir="rtl">TEST الأعداد العقدية</bdi></span>`,
		`<span class="context">TEST Mathématiques › TEST Suites</span>`,
		`<span class="context">TEST Mathématiques › ch-c</span>`,
		`<span class="context"><bdi lang="ar" dir="rtl">TEST فيزياء</bdi></span>`)
	wantRedirect(t, "bookmark", f.post(f.student, lessonPath(ar.publicID)+"/bookmark", nil), lessonPath(ar.publicID))
	wantPage(t, "bookmarks", f.get(f.student, "/learn"), http.StatusOK,
		`<span class="context"><bdi lang="ar" dir="rtl">TEST رياضيات</bdi> › <bdi lang="ar" dir="rtl">TEST الأعداد العقدية</bdi></span>`)

	// Names are escaped text, never markup.
	_, _, _, err = cat.SetChapterNames(ctx, testOperator, "test-math", "ch-a", name(`TEST <b>gras</b> & "x"`), nil)
	must(err)
	w := f.get(f.student, lessonPath(fr.publicID))
	wantPage(t, "escaped name", w, http.StatusOK, `TEST &lt;b&gt;gras&lt;/b&gt; &amp; &#34;x&#34;`)
	wantAbsent(t, "escaped name", w, `<b>gras</b>`)
}
