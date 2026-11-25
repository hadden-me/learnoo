package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/rendering"
)

// Studio integration endpoints (issue #65). Simulated users, subjects,
// chapters and TEST documents only.

func wantPrivateNoStore(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if cc := w.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("%s: Cache-Control %q, want private, no-store", what, cc)
	}
}

func wantBody(t *testing.T, what string, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", what, w.Code, w.Body)
	}
	if got := strings.TrimSuffix(w.Body.String(), "\n"); got != want {
		t.Fatalf("%s:\n got %s\nwant %s", what, got, want)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("%s: Content-Type %q", what, ct)
	}
	wantPrivateNoStore(t, what, w)
}

func TestCatalogueAuthorizationAndOrdering(t *testing.T) {
	f := newReader(t) // subject test-math, author, reviewer of test-math, publisher, admin, student
	// Inserted out of order; listed by code.
	zoo := f.newSubject("test-zoology")
	f.newSubject("test-algebra")
	f.chapter(f.subject, "ch-b")
	chA := f.chapter(f.subject, "ch-a")
	f.chapter(zoo, "zoo-1")
	otherReviewer := f.newUser("test-reviewer-zoo", zoo, permissions.Reviewer)
	adminAuthor := f.newUser("test-admin-author", 0, permissions.Administrator, permissions.Author)

	subjects := "/api/catalogue/subjects"
	chapters := "/api/catalogue/subjects/test-math/chapters"
	for _, path := range []string{subjects, chapters} {
		wantStatus(t, "anonymous "+path, f.api(apiUser{}, http.MethodGet, path, nil), http.StatusUnauthorized, "unauthenticated")
		for name, u := range map[string]apiUser{"student": f.student, "publisher": f.publisher, "admin": f.admin} {
			wantStatus(t, name+" "+path, f.api(u, http.MethodGet, path, nil), http.StatusForbidden, "forbidden")
		}
		// Unsafe methods are not routes.
		if w := f.api(f.author, http.MethodPost, path, map[string]any{}); w.Code < 400 {
			t.Errorf("POST %s: %d", path, w.Code)
		}
	}
	var ids [3]int64
	if err := f.db.QueryRow(context.Background(), `SELECT
		(SELECT id FROM subjects WHERE code = 'test-algebra'), (SELECT id FROM subjects WHERE code = 'test-math'), $1::bigint`, zoo).
		Scan(&ids[0], &ids[1], &ids[2]); err != nil {
		t.Fatal(err)
	}
	wantSubjects := `{"subjects":[{"id":` + itoa(ids[0]) + `,"code":"test-algebra"},{"id":` + itoa(ids[1]) +
		`,"code":"test-math"},{"id":` + itoa(ids[2]) + `,"code":"test-zoology"}]}`
	for name, u := range map[string]apiUser{"author": f.author, "reviewer": f.reviewer, "other reviewer": otherReviewer, "admin author": adminAuthor} {
		wantBody(t, name+" subjects", f.api(u, http.MethodGet, subjects, nil), wantSubjects)
	}
	var chB int64
	if err := f.db.QueryRow(context.Background(), `SELECT id FROM chapters WHERE code = 'ch-b'`).Scan(&chB); err != nil {
		t.Fatal(err)
	}
	// A reviewer of another subject still reads the whole catalogue.
	wantBody(t, "chapters", f.api(otherReviewer, http.MethodGet, chapters, nil),
		`{"subject":{"id":`+itoa(f.subject)+`,"code":"test-math"},"chapters":[{"id":`+itoa(chA)+`,"code":"ch-a"},{"id":`+itoa(chB)+`,"code":"ch-b"}]}`)
	wantBody(t, "no chapters", f.api(f.author, http.MethodGet, "/api/catalogue/subjects/test-algebra/chapters", nil),
		`{"subject":{"id":`+itoa(ids[0])+`,"code":"test-algebra"},"chapters":[]}`)
	for _, code := range []string{"unknown", "TEST-MATH", "-x", "a%20b", strings.Repeat("a", 64)} {
		wantStatus(t, "chapters of "+code, f.api(f.author, http.MethodGet, "/api/catalogue/subjects/"+code+"/chapters", nil), http.StatusNotFound, "not_found")
	}
	// A student still gets 403 for an unknown subject: nothing is revealed.
	wantStatus(t, "student unknown subject", f.api(f.student, http.MethodGet, "/api/catalogue/subjects/unknown/chapters", nil), http.StatusForbidden, "forbidden")
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

type ownedBody struct {
	Lessons []struct {
		ID                  int64  `json:"id"`
		PublicID            string `json:"publicId"`
		SubjectID           int64  `json:"subjectId"`
		SubjectCode         string `json:"subjectCode"`
		Language            string `json:"language"`
		TranslationGroupID  string `json:"translationGroupId"`
		Archived            bool   `json:"archived"`
		PublishedRevisionID *int64 `json:"publishedRevisionId"`
		CreatedAt           string `json:"createdAt"`
		HasFeedback         bool   `json:"hasFeedback"`
		FeedbackRevisionID  *int64 `json:"feedbackRevisionId"`
		LatestRevision      *struct {
			ID          int64   `json:"id"`
			Number      int     `json:"number"`
			Status      string  `json:"status"`
			Title       string  `json:"title"`
			ChapterID   *int64  `json:"chapterId"`
			ChapterCode *string `json:"chapterCode"`
			Version     int     `json:"version"`
			UpdatedAt   string  `json:"updatedAt"`
		} `json:"latestRevision"`
	} `json:"lessons"`
}

func (f readerFixture) owned(u apiUser) ownedBody {
	f.t.Helper()
	w := f.api(u, http.MethodGet, "/api/lessons/mine", nil)
	if w.Code != http.StatusOK {
		f.t.Fatalf("mine: %d %s", w.Code, w.Body)
	}
	wantPrivateNoStore(f.t, "mine", w)
	var b ownedBody
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		f.t.Fatalf("mine: %v: %s", err, w.Body)
	}
	if b.Lessons == nil {
		f.t.Fatalf("mine: lessons is not an array: %s", w.Body)
	}
	return b
}

func TestOwnedLessonsListOnlyTheCallersLessons(t *testing.T) {
	f := newReader(t)
	ctx := context.Background()
	ch := f.chapter(f.subject, "ch-1")
	other := f.newUser("test-author-two", f.subject, permissions.Author)
	adminAuthor := f.newUser("test-admin-author", f.subject, permissions.Administrator, permissions.Author)

	wantStatus(t, "anonymous", f.api(apiUser{}, http.MethodGet, "/api/lessons/mine", nil), http.StatusUnauthorized, "unauthenticated")
	for name, u := range map[string]apiUser{"student": f.student, "reviewer": f.reviewer, "publisher": f.publisher, "admin": f.admin} {
		wantStatus(t, name, f.api(u, http.MethodGet, "/api/lessons/mine", nil), http.StatusForbidden, "forbidden")
	}
	if got := f.owned(f.author).Lessons; len(got) != 0 {
		t.Fatalf("no lessons yet: %+v", got)
	}

	// 1: published, with a draft of a correction on top.
	pub := f.publish(f.approved(0, readerDoc("TEST Publiée", "fr"), "fr", &ch))
	draft, err := f.ls.CreateDraft(ctx, f.author.id, pub.lessonID, lessons.Fields{Content: readerDoc("TEST Publiée v2", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	// 2: changes requested.
	rev, err := f.ls.CreateLesson(ctx, f.author.id, lessons.NewLesson{SubjectID: f.subject, Language: "ar", Fields: lessons.Fields{Content: readerDoc("TEST مراجعة", "ar")}})
	if err != nil {
		t.Fatal(err)
	}
	if rev, err = f.ls.Submit(ctx, f.author.id, rev.ID, rev.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = f.rs.RequestChanges(ctx, f.reviewer.id, rev.ID, "TEST commentaire de relecture"); err != nil {
		t.Fatal(err)
	}
	// 3: a lesson whose only draft was discarded.
	gone, err := f.ls.CreateLesson(ctx, f.author.id, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: readerDoc("TEST Abandonnée", "fr")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ls.DiscardDraft(ctx, f.author.id, gone.ID, gone.Version); err != nil {
		t.Fatal(err)
	}
	// Lessons of other users, including an administrator holding author.
	if _, err := f.ls.CreateLesson(ctx, other.id, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: readerDoc("TEST Autre autrice", "fr")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ls.CreateLesson(ctx, adminAuthor.id, lessons.NewLesson{SubjectID: f.subject, Language: "fr", Fields: lessons.Fields{Content: readerDoc("TEST Admin", "fr")}}); err != nil {
		t.Fatal(err)
	}
	// Most recently updated first: the discarded lesson's only timestamp is
	// its creation, the last of the three; then the changes-requested
	// revision; then the correction draft.
	got := f.owned(f.author).Lessons
	if len(got) != 3 {
		t.Fatalf("author sees %d lessons, want 3: %+v", len(got), got)
	}
	l2, l0, l1 := got[0], got[1], got[2]
	if l2.ID != gone.LessonID || l0.ID != rev.LessonID || l1.ID != pub.lessonID {
		t.Fatalf("order %d %d %d, want %d %d %d", got[0].ID, got[1].ID, got[2].ID, gone.LessonID, rev.LessonID, pub.lessonID)
	}
	if lr := l0.LatestRevision; lr == nil || lr.ID != rev.ID || lr.Status != "changes_requested" || !l0.HasFeedback || l0.FeedbackRevisionID == nil || *l0.FeedbackRevisionID != rev.ID ||
		l0.Language != "ar" || l0.PublishedRevisionID != nil || lr.Title != "TEST مراجعة" || lr.ChapterID != nil || lr.ChapterCode != nil {
		t.Errorf("changes-requested lesson: %+v %+v", l0, lr)
	}
	if lr := l1.LatestRevision; lr == nil || lr.ID != draft.ID || lr.Number != 2 || lr.Status != "draft" || l1.HasFeedback || l1.FeedbackRevisionID != nil ||
		lr.Version != draft.Version || lr.Title != "TEST Publiée v2" || l1.PublishedRevisionID == nil || *l1.PublishedRevisionID != pub.revisionID ||
		l1.PublicID != pub.publicID || l1.SubjectCode != "test-math" || l1.SubjectID != f.subject || l1.Archived || l1.TranslationGroupID == "" {
		t.Errorf("published lesson with draft: %+v %+v", l1, lr)
	}
	if l2.LatestRevision != nil || l2.PublishedRevisionID != nil {
		t.Errorf("discarded lesson: %+v", l2)
	}
	// The published revision's chapter shows while it is the latest.
	if err := f.ls.DiscardDraft(ctx, f.author.id, draft.ID, draft.Version); err != nil {
		t.Fatal(err)
	}
	for _, l := range f.owned(f.author).Lessons {
		if l.ID == pub.lessonID {
			if lr := l.LatestRevision; lr == nil || lr.Status != "approved" || lr.ChapterCode == nil || *lr.ChapterCode != "ch-1" || lr.ChapterID == nil || *lr.ChapterID != ch {
				t.Errorf("published latest revision: %+v", lr)
			}
		}
	}

	// Feedback stays actionable on the successor draft, until a later
	// revision is submitted; a new change request makes it actionable again.
	wantFeedback := func(what string, want *int64, status string) {
		t.Helper()
		for _, l := range f.owned(f.author).Lessons {
			if l.ID != rev.LessonID {
				continue
			}
			id := l.FeedbackRevisionID
			if l.HasFeedback != (want != nil) || (want == nil) != (id == nil) || (want != nil && *id != *want) || l.LatestRevision.Status != status {
				t.Errorf("%s: hasFeedback %v, feedbackRevisionId %v, latest status %s; want %v, %s", what, l.HasFeedback, id, l.LatestRevision.Status, want, status)
			}
			return
		}
		t.Fatalf("%s: lesson not listed", what)
	}
	successor, err := f.ls.CreateDraft(ctx, f.author.id, rev.LessonID, lessons.Fields{Content: readerDoc("TEST مراجعة ٢", "ar")})
	if err != nil {
		t.Fatal(err)
	}
	wantFeedback("successor draft", &rev.ID, "draft")
	if successor, err = f.ls.Submit(ctx, f.author.id, successor.ID, successor.Version); err != nil {
		t.Fatal(err)
	}
	wantFeedback("successor submitted", nil, "in_review")
	if _, err = f.rs.RequestChanges(ctx, f.reviewer.id, successor.ID, "TEST deuxième commentaire"); err != nil {
		t.Fatal(err)
	}
	wantFeedback("second change request", &successor.ID, "changes_requested")
	third, err := f.ls.CreateDraft(ctx, f.author.id, rev.LessonID, lessons.Fields{Content: readerDoc("TEST مراجعة ٣", "ar")})
	if err != nil {
		t.Fatal(err)
	}
	wantFeedback("third draft", &successor.ID, "draft")
	if third, err = f.ls.Submit(ctx, f.author.id, third.ID, third.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = f.rs.Approve(ctx, f.reviewer.id, third.ID, ""); err != nil {
		t.Fatal(err)
	}
	wantFeedback("approved", nil, "approved")

	// Others see only their own; the administrator gets no exception.
	if got := f.owned(other).Lessons; len(got) != 1 || got[0].LatestRevision.Title != "TEST Autre autrice" {
		t.Errorf("other author sees %+v", got)
	}
	if got := f.owned(adminAuthor).Lessons; len(got) != 1 || got[0].LatestRevision.Title != "TEST Admin" {
		t.Errorf("admin author sees %+v", got)
	}
	// Losing author mid-session denies the listing (checked by the store too).
	if _, err := permissions.NewStore(f.db).Revoke(ctx, testOperator, other.id, permissions.Author); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "revoked author", f.api(other, http.MethodGet, "/api/lessons/mine", nil), http.StatusForbidden, "forbidden")
	if _, err := f.ls.Owned(ctx, other.id); !errorsIsDenied(err) {
		t.Errorf("store after revocation: %v", err)
	}
}

func errorsIsDenied(err error) bool {
	return err != nil && strings.Contains(err.Error(), permissions.ErrDenied.Error())
}

func previewOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	wantPrivateNoStore(t, "preview", w)
	var b map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || len(b) != 1 {
		t.Fatalf("preview body %s: %v", w.Body, err)
	}
	return b["html"]
}

func TestPreviewMatchesTheReader(t *testing.T) {
	f := newReader(t)
	for _, tc := range []struct {
		title, lang string
		content     []byte
	}{
		{"TEST Leçon mixte", "fr", richDoc("TEST Leçon mixte")},
		{"TEST درس", "ar", readerDoc("TEST درس", "ar")},
	} {
		p := f.publish(f.approved(0, tc.content, tc.lang, nil))
		page := f.get(f.student, lessonPath(p.publicID))
		wantPage(t, "reader", page, http.StatusOK)

		before := f.count(`SELECT count(*) FROM lesson_revisions`)
		html := previewOf(t, f.api(f.author, http.MethodPost, "/api/preview", map[string]json.RawMessage{"content": tc.content}))
		if f.count(`SELECT count(*) FROM lesson_revisions`) != before {
			t.Fatal("preview stored a revision")
		}
		doc, err := lessons.ValidateContent(tc.content)
		if err != nil {
			t.Fatal(err)
		}
		parts, err := rendering.RenderLesson(doc)
		if err != nil {
			t.Fatal(err)
		}
		if html != string(parts.HTML()) {
			t.Fatalf("preview is not the canonical parts:\n%s", html)
		}
		// Each part appears byte for byte in the reader's page for the
		// published copy of the same document.
		body := page.Body.String()
		for name, part := range map[string]string{"heading": string(parts.Heading), "objective": string(parts.Objective), "body": string(parts.Body)} {
			if part == "" || !strings.Contains(body, part) {
				t.Fatalf("%s: reader page lacks the preview's %s %q", tc.lang, name, part)
			}
		}
		dir := rendering.Dir(tc.lang)
		if !strings.HasPrefix(html, `<h1 lang="`+tc.lang+`" dir="`+dir+`">`) {
			t.Errorf("preview heading: %s", html)
		}
	}
}

func TestPreviewAuthorizationAndValidation(t *testing.T) {
	f := newReader(t)
	doc := map[string]json.RawMessage{"content": readerDoc("TEST Aperçu", "fr")}
	path := "/api/preview"
	wantStatus(t, "anonymous", f.api(apiUser{}, http.MethodPost, path, doc), http.StatusUnauthorized, "unauthenticated")
	wantStatus(t, "student", f.api(f.student, http.MethodPost, path, doc), http.StatusForbidden, "forbidden")
	// Every account with Studio access previews (issue #66 second slice):
	// reviewers, publishers and administrators read submitted revisions
	// through it.
	for _, u := range []apiUser{f.reviewer, f.publisher, f.admin} {
		previewOf(t, f.api(u, http.MethodPost, path, doc))
	}
	noCSRF := apiUser{id: f.author.id, cookie: f.author.cookie}
	wantStatus(t, "no CSRF token", f.api(noCSRF, http.MethodPost, path, doc), http.StatusForbidden, "")
	wantStatus(t, "wrong CSRF token", f.api(noCSRF, http.MethodPost, path, doc, withCSRF("x"+f.author.csrf)), http.StatusForbidden, "")
	wantStatus(t, "cross-site", f.api(f.author, http.MethodPost, path, doc, withHeader("Sec-Fetch-Site", "cross-site")), http.StatusForbidden, "cross_origin_rejected")
	if w := f.api(f.author, http.MethodGet, path, nil); w.Code < 400 {
		t.Errorf("GET preview: %d", w.Code)
	}
	previewOf(t, f.api(f.author, http.MethodPost, path, doc))

	raw := func(s string) []byte { return []byte(s) }
	valid := string(readerDoc("TEST Aperçu", "fr"))
	for _, tc := range []struct {
		what   string
		body   []byte
		opts   []reqOpt
		status int
		code   string
	}{
		{"not JSON content type", raw(`{"content":` + valid + `}`), []reqOpt{withHeader("Content-Type", "text/plain")}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"empty body", raw(``), nil, http.StatusBadRequest, "invalid_request"},
		{"array", raw(`[]`), nil, http.StatusBadRequest, "invalid_request"},
		{"missing content", raw(`{}`), nil, http.StatusBadRequest, "invalid_request"},
		{"null content", raw(`{"content":null}`), nil, http.StatusBadRequest, "invalid_request"},
		{"unknown key", raw(`{"content":` + valid + `,"chapterId":1}`), nil, http.StatusBadRequest, "invalid_request"},
		{"case-folded key", raw(`{"Content":` + valid + `}`), nil, http.StatusBadRequest, "invalid_request"},
		{"duplicate key", raw(`{"content":` + valid + `,"content":` + valid + `}`), nil, http.StatusBadRequest, "invalid_request"},
		{"trailing data", raw(`{"content":` + valid + `} {}`), nil, http.StatusBadRequest, "invalid_request"},
		{"content not an object", raw(`{"content":"x"}`), nil, http.StatusUnprocessableEntity, "invalid_content"},
		{"wrong schema", raw(`{"content":` + strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":2`, 1) + `}`), nil, http.StatusUnprocessableEntity, "invalid_content"},
		{"raw HTML node", raw(`{"content":` + string(readerDoc("TEST", "fr", map[string]any{"type": "html", "text": "<script>alert(1)</script>"})) + `}`), nil, http.StatusUnprocessableEntity, "invalid_content"},
		{"unknown document key", raw(`{"content":` + strings.Replace(valid, `{`, `{"extra":1,`, 1) + `}`), nil, http.StatusUnprocessableEntity, "invalid_content"},
		{"NUL", raw(`{"content":` + strings.Replace(valid, "Texte de test.", `Texte\u0000`, 1) + `}`), nil, http.StatusUnprocessableEntity, "invalid_content"},
		{"over 256 KiB", raw(`{"content":` + valid + strings.Repeat(" ", 256<<10) + `}`), nil, http.StatusRequestEntityTooLarge, "too_large"},
	} {
		w := f.api(f.author, http.MethodPost, path, tc.body, tc.opts...)
		wantStatus(t, tc.what, w, tc.status, tc.code)
		if strings.Contains(w.Body.String(), "<script>") {
			t.Errorf("%s: echoed markup: %s", tc.what, w.Body)
		}
	}
	// A document over the 128 KiB document limit but inside the body limit
	// is refused by the canonical validator, as for a draft save.
	long := readerDoc("TEST long", "fr")
	var big map[string]any
	_ = json.Unmarshal(long, &big)
	blocks := []any{}
	for range 20 {
		blocks = append(blocks, para(text(strings.Repeat("é", 3990))))
	}
	big["blocks"] = blocks
	bigJSON, _ := json.Marshal(map[string]any{"content": big})
	if len(bigJSON) <= 128<<10 || len(bigJSON) >= 256<<10 {
		t.Fatalf("test document size %d", len(bigJSON))
	}
	w := f.api(f.author, http.MethodPost, path, bigJSON)
	wantStatus(t, "document over 128 KiB", w, http.StatusUnprocessableEntity, "invalid_content")
	// The message names the problem in JSON terms.
	if !strings.Contains(w.Body.String(), "128 KiB") {
		t.Errorf("size message: %s", w.Body)
	}
}
