package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// Simulated users, subjects, chapters, unreviewed test documents and
// generated test images only.

type apiUser struct {
	id     int64
	cookie string
	csrf   string
}

var addrCounter atomic.Int64

// testOperator is the local operator actor the tests grant and revoke with.
var testOperator = func() permissions.Actor {
	a, err := permissions.OperatorAs("test")
	if err != nil {
		panic(err)
	}
	return a
}()

// newUser creates and signs in a simulated user holding perms (reviewer is
// granted for subject).
func (e *env) newUser(name string, subject int64, perms ...permissions.Permission) apiUser {
	e.t.Helper()
	email := name + "@example.test"
	u := e.createUser(email, testPassword)
	ps := permissions.NewStore(e.db)
	for _, p := range perms {
		var err error
		if p == permissions.Reviewer {
			_, err = ps.GrantReviewer(context.Background(), testOperator, u.ID, subject)
		} else {
			_, err = ps.Grant(context.Background(), testOperator, u.ID, p)
		}
		if err != nil {
			e.t.Fatal(err)
		}
	}
	// A distinct client address per login keeps the per-address throttle out of the way.
	addr := fmt.Sprintf("198.51.100.%d", addrCounter.Add(1)%250+1)
	w := e.login(email, testPassword, withAddr(addr))
	if w.Code != http.StatusOK {
		e.t.Fatalf("login %s: %d %s", name, w.Code, w.Body)
	}
	var b sessionBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		e.t.Fatal(err)
	}
	return apiUser{id: u.ID, cookie: sessionCookie(e.t, w).Value, csrf: b.CSRFToken}
}

func (u apiUser) opts() []reqOpt {
	if u.cookie == "" {
		return nil
	}
	return []reqOpt{withCookie(u.cookie), withCSRF(u.csrf)}
}

// checkAPIHeaders asserts the caching rules every API response must carry.
func checkAPIHeaders(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("%s: Cache-Control %q lacks no-store", what, cc)
	}
	if v := w.Header().Get("Vary"); v != "Cookie" {
		t.Errorf("%s: Vary %q, want Cookie", what, v)
	}
}

// api sends a JSON request (body is marshalled unless it is []byte, sent as is).
func (e *env) api(u apiUser, method, path string, body any, extra ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			e.t.Error(err)
		}
		rd = bytes.NewReader(buf)
	}
	r := httptest.NewRequest(method, "http://learnoo.test"+path, rd)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.RemoteAddr = "192.0.2.10:40000"
	for _, o := range append(u.opts(), extra...) {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	checkAPIHeaders(e.t, method+" "+path, w)
	return w
}

// upload sends a raw image body.
func (e *env) upload(u apiUser, ctype string, body io.Reader, contentLength int64, extra ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://learnoo.test/api/media", body)
	r.ContentLength = contentLength
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	r.RemoteAddr = "192.0.2.10:40000"
	for _, o := range append(u.opts(), extra...) {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	checkAPIHeaders(e.t, "upload", w)
	return w
}

func (e *env) uploadBytes(u apiUser, ctype string, data []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.upload(u, ctype, bytes.NewReader(data), int64(len(data)))
}

func (e *env) newSubject(code string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(context.Background(), `INSERT INTO subjects (code) VALUES ($1) RETURNING id`, code).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) chapter(subject int64, code string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(context.Background(), `INSERT INTO chapters (subject_id, code) VALUES ($1, $2) RETURNING id`, subject, code).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// testDoc builds a minimal schema v1 unreviewed document with an embedded exercise.
func testDoc(title, lang string) map[string]any {
	return map[string]any{
		"schemaVersion": 1,
		"title":         title,
		"language":      lang,
		"status":        "unreviewed",
		"objective":     "Simulated objective for API tests.",
		"blocks": []any{
			map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": "Simulated text: " + title}}},
			map[string]any{"type": "exercise", "title": "Simulated exercise", "children": []any{
				map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "math", "text": "1+1"}}},
				map[string]any{"type": "solution", "children": []any{
					map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": "Simulated solution"}}},
				}},
			}},
		},
	}
}

type revisionBody struct {
	ID             int64           `json:"id"`
	LessonID       int64           `json:"lessonId"`
	Number         int             `json:"number"`
	Status         string          `json:"status"`
	AuthorID       int64           `json:"authorId"`
	BaseRevisionID *int64          `json:"baseRevisionId"`
	Title          string          `json:"title"`
	Objectives     string          `json:"objectives"`
	Prerequisites  string          `json:"prerequisites"`
	Content        json.RawMessage `json:"content"`
	Language       string          `json:"language"`
	SubjectID      int64           `json:"subjectId"`
	ChapterID      *int64          `json:"chapterId"`
	Version        int             `json:"version"`
	SubmittedAt    *string         `json:"submittedAt"`
	MediaIDs       []int64         `json:"mediaIds"`
}

func decodeRevision(t *testing.T, w *httptest.ResponseRecorder) revisionBody {
	t.Helper()
	var b revisionBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("not a revision: %s", w.Body)
	}
	return b
}

func wantStatus(t *testing.T, what string, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", what, w.Code, status, w.Body)
	}
	if code != "" {
		if got := errorCode(t, w); got != code {
			t.Fatalf("%s: code %q, want %q: %s", what, got, code, w.Body)
		}
	}
}

type studioFixture struct {
	*env
	subject int64
	author  apiUser
}

func newStudio(t *testing.T) studioFixture {
	e := newEnv(t)
	subject := e.newSubject("simulated-math")
	return studioFixture{env: e, subject: subject, author: e.newUser("author-one", subject, permissions.Author)}
}

func (f studioFixture) createLesson(u apiUser, title string) revisionBody {
	f.t.Helper()
	w := f.api(u, http.MethodPost, "/api/lessons", map[string]any{
		"subjectId": f.subject, "language": "fr", "content": testDoc(title, "fr"),
	})
	wantStatus(f.t, "create lesson", w, http.StatusCreated, "")
	return decodeRevision(f.t, w)
}

func revPath(id int64) string { return "/api/revisions/" + strconv.FormatInt(id, 10) }

func pngBytes(t *testing.T, seed uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for x := 0; x < 8; x++ {
		for y := 0; y < 6; y++ {
			img.Set(x, y, color.NRGBA{R: seed, G: uint8(x * 30), B: uint8(y * 40), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// endpoints lists every Studio route with a body that would otherwise be valid.
func (f studioFixture) endpoints(revID, lessonID int64) []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodPost, "/api/lessons"},
		{http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", lessonID)},
		{http.MethodGet, revPath(revID)},
		{http.MethodPut, revPath(revID)},
		{http.MethodPost, revPath(revID) + "/submit"},
		{http.MethodDelete, revPath(revID) + "?expectedVersion=1"},
		{http.MethodPost, "/api/media"},
		{http.MethodGet, "/api/media/1"},
	}
}

func (f studioFixture) bodyFor(method, path string) any {
	switch {
	case method == http.MethodGet || method == http.MethodDelete:
		return nil
	case path == "/api/lessons":
		return map[string]any{"subjectId": f.subject, "language": "fr", "content": testDoc("Probe", "fr")}
	case strings.HasSuffix(path, "/submit"):
		return map[string]any{"expectedVersion": 1}
	case method == http.MethodPut:
		return map[string]any{"expectedVersion": 1, "content": testDoc("Probe", "fr")}
	default:
		return map[string]any{"content": testDoc("Probe", "fr")}
	}
}

func TestStudioAnonymousAndCrossSiteRequestsAreRejected(t *testing.T) {
	f := newStudio(t)
	rev := f.createLesson(f.author, "Simulated lesson")
	other := f.newUser("author-two", f.subject, permissions.Author)
	before := f.count(`SELECT count(*) FROM lesson_revisions`)
	for _, ep := range f.endpoints(rev.ID, rev.LessonID) {
		name := ep.method + " " + ep.path
		w := f.api(apiUser{}, ep.method, ep.path, f.bodyFor(ep.method, ep.path), withCSRF("anything"))
		wantStatus(t, "anonymous "+name, w, http.StatusUnauthorized, "unauthenticated")
		// A forged cookie is anonymous too.
		w = f.api(apiUser{}, ep.method, ep.path, f.bodyFor(ep.method, ep.path),
			withCookie("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), withCSRF("anything"))
		wantStatus(t, "forged cookie "+name, w, http.StatusUnauthorized, "unauthenticated")
		if ep.method == http.MethodGet {
			continue
		}
		for label, opts := range map[string][]reqOpt{
			"missing CSRF":      {withCookie(f.author.cookie)},
			"wrong CSRF":        {withCookie(f.author.cookie), withCSRF("not-the-token")},
			"other user's CSRF": {withCookie(f.author.cookie), withCSRF(other.csrf)},
		} {
			w = f.api(apiUser{}, ep.method, ep.path, f.bodyFor(ep.method, ep.path), opts...)
			wantStatus(t, label+" "+name, w, http.StatusForbidden, "csrf_rejected")
		}
		for label, opt := range map[string]reqOpt{
			"Sec-Fetch-Site": withHeader("Sec-Fetch-Site", "cross-site"),
			"Origin":         withHeader("Origin", "https://evil.example"),
		} {
			w = f.api(f.author, ep.method, ep.path, f.bodyFor(ep.method, ep.path), opt)
			wantStatus(t, "cross-origin via "+label+" "+name, w, http.StatusForbidden, "cross_origin_rejected")
		}
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions`); n != before {
		t.Fatalf("rejected requests changed revisions: %d -> %d", before, n)
	}
	if got := decodeRevision(t, f.api(f.author, http.MethodGet, revPath(rev.ID), nil)); got.Version != 1 || got.Status != "draft" {
		t.Fatalf("draft changed by rejected requests: %+v", got)
	}
}

func TestStudioRequiresAuthorPermission(t *testing.T) {
	f := newStudio(t)
	rev := f.createLesson(f.author, "Simulated lesson")
	img := f.uploadBytes(f.author, "image/png", pngBytes(t, 1))
	wantStatus(t, "author upload", img, http.StatusCreated, "")
	others := map[string]apiUser{
		"student":        f.newUser("student-one", f.subject),
		"reviewer-only":  f.newUser("reviewer-one", f.subject, permissions.Reviewer),
		"publisher-only": f.newUser("publisher-one", f.subject, permissions.Publisher),
		"admin-only":     f.newUser("admin-one", f.subject, permissions.Administrator),
	}
	before := f.count(`SELECT count(*) FROM lesson_revisions`) + f.count(`SELECT count(*) FROM media_assets`)
	for who, u := range others {
		// The denial is the same for an existing and a missing revision.
		for _, id := range []int64{rev.ID, rev.ID + 1000} {
			for _, ep := range f.endpoints(id, rev.LessonID) {
				w := f.api(u, ep.method, ep.path, f.bodyFor(ep.method, ep.path))
				if ep.path == "/api/media" {
					w = f.uploadBytes(u, "image/png", pngBytes(t, 2))
				}
				wantStatus(t, who+" "+ep.method+" "+ep.path, w, http.StatusForbidden, "forbidden")
			}
		}
	}
	if after := f.count(`SELECT count(*) FROM lesson_revisions`) + f.count(`SELECT count(*) FROM media_assets`); after != before {
		t.Fatalf("denied requests stored data: %d -> %d", before, after)
	}

	// Revocation applies to the next request.
	if _, err := permissions.NewStore(f.db).Revoke(context.Background(), testOperator, f.author.id, permissions.Author); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "revoked author reads own draft", f.api(f.author, http.MethodGet, revPath(rev.ID), nil), http.StatusForbidden, "forbidden")
}

func TestStudioOtherAuthorsGetNotFound(t *testing.T) {
	f := newStudio(t)
	rev := f.createLesson(f.author, "Simulated lesson")
	other := f.newUser("author-two", f.subject, permissions.Author)
	before := f.api(f.author, http.MethodGet, revPath(rev.ID), nil).Body.String()

	missing := rev.ID + 1000
	for _, c := range []struct {
		method, suffix string
		body           any
	}{
		{http.MethodGet, "", nil},
		{http.MethodPut, "", map[string]any{"expectedVersion": 1, "content": testDoc("Hijack", "fr")}},
		{http.MethodPost, "/submit", map[string]any{"expectedVersion": 1}},
		{http.MethodDelete, "?expectedVersion=1", nil},
	} {
		theirs := f.api(other, c.method, revPath(rev.ID)+c.suffix, c.body)
		absent := f.api(other, c.method, revPath(missing)+c.suffix, c.body)
		wantStatus(t, "other author "+c.method, theirs, http.StatusNotFound, "not_found")
		if theirs.Body.String() != absent.Body.String() || theirs.Code != absent.Code {
			t.Fatalf("%s: someone else's revision (%d %s) differs from a missing one (%d %s)",
				c.method, theirs.Code, theirs.Body, absent.Code, absent.Body)
		}
	}
	if after := f.api(f.author, http.MethodGet, revPath(rev.ID), nil).Body.String(); after != before {
		t.Fatalf("other author changed the draft:\n%s\n%s", before, after)
	}
}

func TestStudioDraftLifecycle(t *testing.T) {
	f := newStudio(t)
	chapter := f.chapter(f.subject, "simulated-chapter")

	w := f.api(f.author, http.MethodPost, "/api/lessons", map[string]any{
		"subjectId": f.subject, "language": "fr", "content": testDoc("Première version", "fr"),
		"prerequisites": "Simulated prerequisites", "chapterId": chapter,
	})
	wantStatus(t, "create", w, http.StatusCreated, "")
	rev := decodeRevision(t, w)
	if w.Header().Get("Location") != revPath(rev.ID) {
		t.Fatalf("Location %q", w.Header().Get("Location"))
	}
	if rev.Status != "draft" || rev.Version != 1 || rev.Number != 1 || rev.Title != "Première version" ||
		rev.AuthorID != f.author.id || rev.ChapterID == nil || *rev.ChapterID != chapter ||
		rev.Prerequisites != "Simulated prerequisites" || rev.MediaIDs == nil || rev.BaseRevisionID != nil ||
		rev.Objectives != "Simulated objective for API tests." {
		t.Fatalf("created revision: %+v", rev)
	}

	got := decodeRevision(t, f.api(f.author, http.MethodGet, revPath(rev.ID), nil))
	if got.ID != rev.ID || !strings.Contains(string(got.Content), "Première version") {
		t.Fatalf("read back: %+v", got)
	}

	// Save, then a stale save from a second tab.
	w = f.api(f.author, http.MethodPut, revPath(rev.ID), map[string]any{
		"expectedVersion": 1, "content": testDoc("Winner", "fr"),
	})
	wantStatus(t, "save", w, http.StatusOK, "")
	if saved := decodeRevision(t, w); saved.Version != 2 || saved.Title != "Winner" || saved.ChapterID != nil {
		t.Fatalf("saved: %+v", saved)
	}
	w = f.api(f.author, http.MethodPut, revPath(rev.ID), map[string]any{
		"expectedVersion": 1, "content": testDoc("Loser", "fr"),
	})
	wantStatus(t, "stale save", w, http.StatusConflict, "version_conflict")
	if got := decodeRevision(t, f.api(f.author, http.MethodGet, revPath(rev.ID), nil)); got.Title != "Winner" || got.Version != 2 {
		t.Fatalf("stale save overwrote the winner: %+v", got)
	}

	// Submission.
	wantStatus(t, "stale submit", f.api(f.author, http.MethodPost, revPath(rev.ID)+"/submit",
		map[string]any{"expectedVersion": 1}), http.StatusConflict, "version_conflict")
	w = f.api(f.author, http.MethodPost, revPath(rev.ID)+"/submit", map[string]any{"expectedVersion": 2})
	wantStatus(t, "submit", w, http.StatusOK, "")
	sub := decodeRevision(t, w)
	if sub.Status != "in_review" || sub.SubmittedAt == nil || sub.Version != 3 {
		t.Fatalf("submitted: %+v", sub)
	}
	frozen := f.api(f.author, http.MethodGet, revPath(rev.ID), nil).Body.String()

	// After submission nothing changes, whatever the version.
	for _, v := range []int{2, 3} {
		wantStatus(t, "save after submit", f.api(f.author, http.MethodPut, revPath(rev.ID), map[string]any{
			"expectedVersion": v, "content": testDoc("Too late", "fr"),
		}), http.StatusConflict, "not_draft")
		wantStatus(t, "discard after submit", f.api(f.author, http.MethodDelete,
			fmt.Sprintf("%s?expectedVersion=%d", revPath(rev.ID), v), nil), http.StatusConflict, "not_draft")
		wantStatus(t, "resubmit", f.api(f.author, http.MethodPost, revPath(rev.ID)+"/submit",
			map[string]any{"expectedVersion": v}), http.StatusConflict, "not_draft")
	}
	if after := f.api(f.author, http.MethodGet, revPath(rev.ID), nil).Body.String(); after != frozen {
		t.Fatalf("submitted revision changed:\n%s\n%s", frozen, after)
	}
	// The in-review revision is the active candidate.
	wantStatus(t, "draft while in review", f.api(f.author, http.MethodPost,
		fmt.Sprintf("/api/lessons/%d/drafts", rev.LessonID), map[string]any{"content": testDoc("Next", "fr")}),
		http.StatusConflict, "active_candidate_exists")

	// Discard a draft of a second lesson, then start a new one. Revision
	// numbers are never reused, even after a discard.
	other := f.createLesson(f.author, "Discarded lesson")
	wantStatus(t, "stale discard", f.api(f.author, http.MethodDelete, revPath(other.ID)+"?expectedVersion=2", nil),
		http.StatusConflict, "version_conflict")
	for _, q := range []string{"", "?expectedVersion=", "?expectedVersion=x", "?expectedVersion=1&expectedVersion=1", "?expectedVersion=1&x=1", "?expectedVersion=+1"} {
		wantStatus(t, "discard query "+q, f.api(f.author, http.MethodDelete, revPath(other.ID)+q, nil), http.StatusBadRequest, "invalid_request")
	}
	w = f.api(f.author, http.MethodDelete, revPath(other.ID)+"?expectedVersion=1", nil)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("discard: %d %s", w.Code, w.Body)
	}
	wantStatus(t, "read discarded", f.api(f.author, http.MethodGet, revPath(other.ID), nil), http.StatusNotFound, "not_found")
	w = f.api(f.author, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", other.LessonID),
		map[string]any{"content": testDoc("Second attempt", "fr"), "prerequisites": "None"})
	wantStatus(t, "new draft", w, http.StatusCreated, "")
	second := decodeRevision(t, w)
	if second.LessonID != other.LessonID || second.Number != 2 || second.Status != "draft" || second.Version != 1 {
		t.Fatalf("new draft: %+v", second)
	}
	wantStatus(t, "discard second", f.api(f.author, http.MethodDelete, revPath(second.ID)+"?expectedVersion=1", nil),
		http.StatusNoContent, "")
	w = f.api(f.author, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", other.LessonID),
		map[string]any{"content": testDoc("Third attempt", "fr")})
	wantStatus(t, "third draft", w, http.StatusCreated, "")
	if third := decodeRevision(t, w); third.Number != 3 {
		t.Fatalf("revision number reused after discard: got %d, want 3", third.Number)
	}
	wantStatus(t, "draft of missing lesson", f.api(f.author, http.MethodPost, "/api/lessons/999999/drafts",
		map[string]any{"content": testDoc("Nowhere", "fr")}), http.StatusNotFound, "not_found")
}

// An approved revision awaiting publication is the active candidate.
// Approval and publication are simulated with direct SQL here (decision and
// audit rows, as the database requires); the review endpoints have their
// own tests.
func TestStudioApprovedUnpublishedBlocksDrafts(t *testing.T) {
	f := newStudio(t)
	rev := f.createLesson(f.author, "Approved")
	wantStatus(t, "submit", f.api(f.author, http.MethodPost, revPath(rev.ID)+"/submit",
		map[string]any{"expectedVersion": rev.Version}), http.StatusOK, "")
	reviewer := f.newUser("reviewer-one", f.subject, permissions.Reviewer)
	publisher := f.newUser("publisher-one", f.subject, permissions.Publisher)
	exec := func(sql ...string) {
		t.Helper()
		tx, err := f.db.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		for _, s := range sql {
			if _, err := tx.Exec(context.Background(), s); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	exec(fmt.Sprintf(`INSERT INTO review_decisions (revision_id, reviewer_id, action) VALUES (%d, %d, 'approve')`, rev.ID, reviewer.id),
		fmt.Sprintf(`UPDATE lesson_revisions SET status = 'approved' WHERE id = %d`, rev.ID))
	draftPath := fmt.Sprintf("/api/lessons/%d/drafts", rev.LessonID)
	wantStatus(t, "draft while approved and unpublished", f.api(f.author, http.MethodPost, draftPath,
		map[string]any{"content": testDoc("Blocked", "fr")}), http.StatusConflict, "active_candidate_exists")
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1`, rev.LessonID); n != 1 {
		t.Fatalf("blocked draft stored: %d revisions", n)
	}
	exec(fmt.Sprintf(`INSERT INTO publication_audit (lesson_id, revision_id, actor_id, action) VALUES (%d, %d, %d, 'publish')`, rev.LessonID, rev.ID, publisher.id),
		fmt.Sprintf(`UPDATE lessons SET published_revision_id = %d WHERE id = %d`, rev.ID, rev.LessonID))
	w := f.api(f.author, http.MethodPost, draftPath, map[string]any{"content": testDoc("Next edition", "fr")})
	wantStatus(t, "draft after publication", w, http.StatusCreated, "")
	if next := decodeRevision(t, w); next.BaseRevisionID == nil || *next.BaseRevisionID != rev.ID {
		t.Fatalf("base %v, want %d", next.BaseRevisionID, rev.ID)
	}
}

func TestStudioConcurrentDraftCreationExactlyOne(t *testing.T) {
	f := newStudio(t)
	first := f.createLesson(f.author, "Simulated lesson")
	wantStatus(t, "discard first", f.api(f.author, http.MethodDelete, revPath(first.ID)+"?expectedVersion=1", nil), http.StatusNoContent, "")

	const n = 8
	codes := make([]int, n)
	errCodes := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := f.api(f.author, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", first.LessonID),
				map[string]any{"content": testDoc(fmt.Sprintf("Racer %d", i), "fr")})
			codes[i] = w.Code
			if w.Code != http.StatusCreated {
				var b struct{ Error struct{ Code string } }
				_ = json.Unmarshal(w.Body.Bytes(), &b)
				errCodes[i] = b.Error.Code
			}
		}()
	}
	close(start)
	wg.Wait()
	created := 0
	for i, c := range codes {
		switch {
		case c == http.StatusCreated:
			created++
		case c == http.StatusConflict && errCodes[i] == "active_candidate_exists":
		default:
			t.Errorf("racer %d: %d %s", i, c, errCodes[i])
		}
	}
	if created != 1 {
		t.Fatalf("%d drafts created, want exactly 1", created)
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1`, first.LessonID); n != 1 {
		t.Fatalf("%d revisions stored", n)
	}
}

func TestStudioInvalidRequestsStoreNothing(t *testing.T) {
	f := newStudio(t)
	otherSubject := f.newSubject("simulated-physics")
	foreignChapter := f.chapter(otherSubject, "simulated-foreign")
	source := f.createLesson(f.author, "Source lesson")
	stored := func() int {
		return f.count(`SELECT (SELECT count(*) FROM lessons) + (SELECT count(*) FROM lesson_revisions) + (SELECT count(*) FROM translation_groups)`)
	}
	before := stored()

	bad := testDoc("Bad", "fr")
	bad["blocks"] = []any{map[string]any{"type": "html", "html": "<script>alert(1)</script>"}}
	reviewed := testDoc("Claims review", "fr")
	reviewed["status"] = "reviewed"
	big := testDoc("Big", "fr")
	big["blocks"] = []any{map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 300<<10)}}}}
	nearLimit := testDoc("Over 128 KiB", "fr")
	nearLimit["blocks"] = []any{map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": strings.Repeat("y", 130<<10)}}}}

	lesson := func(extra map[string]any) map[string]any {
		m := map[string]any{"subjectId": f.subject, "language": "fr", "content": testDoc("Fine", "fr")}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, c := range map[string]struct {
		body   any
		ctype  string
		status int
		code   string
	}{
		"script block":       {lesson(map[string]any{"content": bad}), "", 422, "invalid_content"},
		"reviewed status":    {lesson(map[string]any{"content": reviewed}), "", 422, "invalid_content"},
		"document > 128 KiB": {lesson(map[string]any{"content": nearLimit}), "", 422, "invalid_content"},
		"content not object": {lesson(map[string]any{"content": "text"}), "", 422, "invalid_content"},
		"long prerequisites": {lesson(map[string]any{"prerequisites": strings.Repeat("p", 2001)}), "", 422, "invalid_content"},
		"language mismatch":  {lesson(map[string]any{"language": "ar"}), "", 422, "language_mismatch"},
		"unknown subject":    {lesson(map[string]any{"subjectId": 999999}), "", 422, "unknown_subject"},
		"foreign chapter":    {lesson(map[string]any{"chapterId": foreignChapter}), "", 422, "unknown_chapter"},
		"unknown media":      {lesson(map[string]any{"mediaIds": []int64{999999}}), "", 422, "unknown_media"},
		"negative media":     {lesson(map[string]any{"mediaIds": []int64{-1}}), "", 422, "unknown_media"},
		"unknown source":     {lesson(map[string]any{"translationOf": 999999}), "", 422, "unknown_lesson"},
		"source other subj":  {lesson(map[string]any{"translationOf": source.LessonID, "subjectId": otherSubject, "language": "ar", "content": testDoc("T", "ar")}), "", 422, "translation_subject"},
		"same language":      {lesson(map[string]any{"translationOf": source.LessonID}), "", 409, "translation_exists"},
		"unknown field":      {lesson(map[string]any{"title": "Not a field"}), "", 400, "invalid_request"},
		"missing content":    {map[string]any{"subjectId": f.subject, "language": "fr"}, "", 400, "invalid_request"},
		"null content":       {lesson(map[string]any{"content": nil}), "", 400, "invalid_request"},
		"null prerequisites": {lesson(map[string]any{"prerequisites": nil}), "", 400, "invalid_request"},
		"null media list":    {lesson(map[string]any{"mediaIds": nil}), "", 400, "invalid_request"},
		"null media element": {lesson(map[string]any{"mediaIds": []any{nil}}), "", 400, "invalid_request"},
		"null subject":       {lesson(map[string]any{"subjectId": nil}), "", 400, "invalid_request"},
		"null language":      {lesson(map[string]any{"language": nil}), "", 400, "invalid_request"},
		"fractional media":   {[]byte(`{"subjectId":1,"language":"fr","content":{},"mediaIds":[1.0]}`), "", 400, "invalid_request"},
		"fractional subject": {[]byte(`{"subjectId":1.0,"language":"fr","content":{}}`), "", 400, "invalid_request"},
		"missing subject":    {map[string]any{"language": "fr", "content": testDoc("Fine", "fr")}, "", 400, "invalid_request"},
		"malformed JSON":     {[]byte(`{"subjectId": 1,`), "", 400, "invalid_request"},
		"two JSON values":    {[]byte(`{} {}`), "", 400, "invalid_request"},
		"form encoded":       {[]byte(`subjectId=1`), "application/x-www-form-urlencoded", 415, "unsupported_media_type"},
		"body > 256 KiB":     {lesson(map[string]any{"content": big}), "", 413, "too_large"},
	} {
		var opts []reqOpt
		if c.ctype != "" {
			opts = append(opts, withHeader("Content-Type", c.ctype))
		}
		w := f.api(f.author, http.MethodPost, "/api/lessons", c.body, opts...)
		wantStatus(t, name, w, c.status, c.code)
	}
	if after := stored(); after != before {
		t.Fatalf("rejected creations stored rows: %d -> %d", before, after)
	}

	// Invalid saves leave the draft untouched.
	snapshot := f.api(f.author, http.MethodGet, revPath(source.ID), nil).Body.String()
	for name, c := range map[string]struct {
		body   any
		status int
		code   string
	}{
		"invalid content":    {map[string]any{"expectedVersion": 1, "content": bad}, 422, "invalid_content"},
		"language mismatch":  {map[string]any{"expectedVersion": 1, "content": testDoc("Ar", "ar")}, 422, "language_mismatch"},
		"unknown media":      {map[string]any{"expectedVersion": 1, "content": testDoc("M", "fr"), "mediaIds": []int64{424242}}, 422, "unknown_media"},
		"missing version":    {map[string]any{"content": testDoc("V", "fr")}, 400, "invalid_request"},
		"null version":       {map[string]any{"expectedVersion": nil, "content": testDoc("V", "fr")}, 400, "invalid_request"},
		"null prerequisites": {map[string]any{"expectedVersion": 1, "content": testDoc("V", "fr"), "prerequisites": nil}, 400, "invalid_request"},
		"oversized":          {map[string]any{"expectedVersion": 1, "content": big}, 413, "too_large"},
	} {
		wantStatus(t, "save "+name, f.api(f.author, http.MethodPut, revPath(source.ID), c.body), c.status, c.code)
	}
	wantStatus(t, "submit without version", f.api(f.author, http.MethodPost, revPath(source.ID)+"/submit", map[string]any{}),
		http.StatusBadRequest, "invalid_request")
	if after := f.api(f.author, http.MethodGet, revPath(source.ID), nil).Body.String(); after != snapshot {
		t.Fatalf("rejected saves changed the draft:\n%s\n%s", snapshot, after)
	}

	// Malformed IDs are not found, never parsed loosely.
	for _, p := range []string{"/api/revisions/abc", "/api/revisions/0", "/api/revisions/-1", "/api/revisions/01",
		"/api/revisions/99999999999999999999", "/api/lessons/abc/drafts"} {
		method := http.MethodGet
		var body any
		if strings.HasSuffix(p, "/drafts") {
			method, body = http.MethodPost, map[string]any{"content": testDoc("X", "fr")}
		}
		wantStatus(t, p, f.api(f.author, method, p, body), http.StatusNotFound, "not_found")
	}
}

func TestStudioTranslationCreatesLinkedLesson(t *testing.T) {
	f := newStudio(t)
	source := f.createLesson(f.author, "Source lesson")
	w := f.api(f.author, http.MethodPost, "/api/lessons", map[string]any{
		"subjectId": f.subject, "language": "ar", "translationOf": source.LessonID, "content": testDoc("ترجمة", "ar"),
	})
	wantStatus(t, "translation", w, http.StatusCreated, "")
	tr := decodeRevision(t, w)
	if tr.LessonID == source.LessonID || tr.Language != "ar" {
		t.Fatalf("translation: %+v", tr)
	}
	if n := f.count(`SELECT count(DISTINCT translation_group_id) FROM lessons WHERE id IN ($1, $2)`, source.LessonID, tr.LessonID); n != 1 {
		t.Fatalf("translation is not in the source's group")
	}
}

// Only a lesson's owner (its creator, holding author) starts a draft of it
// or translates it (issue #6). Anyone else, administrators included, gets
// exactly the response for a lesson that does not exist.
func TestStudioOnlyOwnerDraftsAndTranslates(t *testing.T) {
	f := newStudio(t)
	rev := f.createLesson(f.author, "Owned lesson")
	wantStatus(t, "discard", f.api(f.author, http.MethodDelete, revPath(rev.ID)+"?expectedVersion=1", nil), http.StatusNoContent, "")
	other := f.newUser("author-two", f.subject, permissions.Author)
	adminAuthor := f.newUser("admin-author", f.subject, permissions.Administrator, permissions.Author)
	adminOnly := f.newUser("admin-only", f.subject, permissions.Administrator)
	before := f.count(`SELECT count(*) FROM lesson_revisions`) + f.count(`SELECT count(*) FROM lessons`)

	draftBody := map[string]any{"content": testDoc("Not mine", "fr")}
	draftPath := fmt.Sprintf("/api/lessons/%d/drafts", rev.LessonID)
	translation := func(source int64) map[string]any {
		return map[string]any{"subjectId": f.subject, "language": "ar", "translationOf": source, "content": testDoc("ترجمة", "ar")}
	}
	for name, u := range map[string]apiUser{"another author": other, "administrator with author": adminAuthor} {
		missing := f.api(u, http.MethodPost, "/api/lessons/999999/drafts", draftBody)
		wantStatus(t, name+": draft of missing lesson", missing, http.StatusNotFound, "not_found")
		theirs := f.api(u, http.MethodPost, draftPath, draftBody)
		wantStatus(t, name+": draft of another's lesson", theirs, http.StatusNotFound, "not_found")
		if theirs.Body.String() != missing.Body.String() {
			t.Fatalf("%s: non-owner draft differs from missing lesson: %s vs %s", name, theirs.Body, missing.Body)
		}
		missing = f.api(u, http.MethodPost, "/api/lessons", translation(999999))
		wantStatus(t, name+": translation of missing lesson", missing, http.StatusUnprocessableEntity, "unknown_lesson")
		theirs = f.api(u, http.MethodPost, "/api/lessons", translation(rev.LessonID))
		wantStatus(t, name+": translation of another's lesson", theirs, http.StatusUnprocessableEntity, "unknown_lesson")
		if theirs.Body.String() != missing.Body.String() {
			t.Fatalf("%s: non-owner translation differs from missing lesson: %s vs %s", name, theirs.Body, missing.Body)
		}
	}
	// An archived lesson looks missing too, never 409 archived, to
	// non-owners; its owner is told it is archived.
	archived := f.createLesson(f.author, "Archived owned lesson")
	wantStatus(t, "discard", f.api(f.author, http.MethodDelete, revPath(archived.ID)+"?expectedVersion=1", nil), http.StatusNoContent, "")
	if _, err := f.db.Exec(context.Background(), `UPDATE lessons SET archived_at = now() WHERE id = $1`, archived.LessonID); err != nil {
		t.Fatal(err)
	}
	archivedPath := fmt.Sprintf("/api/lessons/%d/drafts", archived.LessonID)
	for name, u := range map[string]apiUser{"another author": other, "administrator with author": adminAuthor} {
		missing := f.api(u, http.MethodPost, "/api/lessons/999999/drafts", draftBody)
		theirs := f.api(u, http.MethodPost, archivedPath, draftBody)
		wantStatus(t, name+": draft of another's archived lesson", theirs, http.StatusNotFound, "not_found")
		if theirs.Body.String() != missing.Body.String() {
			t.Fatalf("%s: archived non-owner draft differs from missing lesson: %s vs %s", name, theirs.Body, missing.Body)
		}
		missing = f.api(u, http.MethodPost, "/api/lessons", translation(999999))
		theirs = f.api(u, http.MethodPost, "/api/lessons", translation(archived.LessonID))
		wantStatus(t, name+": translation of another's archived lesson", theirs, http.StatusUnprocessableEntity, "unknown_lesson")
		if theirs.Body.String() != missing.Body.String() {
			t.Fatalf("%s: archived non-owner translation differs from missing lesson: %s vs %s", name, theirs.Body, missing.Body)
		}
	}
	wantStatus(t, "owner draft of archived lesson", f.api(f.author, http.MethodPost, archivedPath, draftBody), http.StatusConflict, "archived")
	before = f.count(`SELECT count(*) FROM lesson_revisions`) + f.count(`SELECT count(*) FROM lessons`)

	// Without author the middleware refuses before any lookup.
	wantStatus(t, "administrator without author", f.api(adminOnly, http.MethodPost, draftPath, draftBody), http.StatusForbidden, "forbidden")
	if after := f.count(`SELECT count(*) FROM lesson_revisions`) + f.count(`SELECT count(*) FROM lessons`); after != before {
		t.Fatalf("refused requests stored rows: %d -> %d", before, after)
	}

	// The owner may do both.
	wantStatus(t, "owner draft", f.api(f.author, http.MethodPost, draftPath, map[string]any{"content": testDoc("Mine", "fr")}), http.StatusCreated, "")
	wantStatus(t, "owner translation", f.api(f.author, http.MethodPost, "/api/lessons", translation(rev.LessonID)), http.StatusCreated, "")

	// The owner without author, or disabled, may not.
	revoked := f.newUser("revoked-owner", f.subject, permissions.Author)
	revokedRev := f.createLesson(revoked, "Revoked owner's lesson")
	wantStatus(t, "discard", f.api(revoked, http.MethodDelete, revPath(revokedRev.ID)+"?expectedVersion=1", nil), http.StatusNoContent, "")
	if _, err := permissions.NewStore(f.db).Revoke(context.Background(), testOperator, revoked.id, permissions.Author); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "owner without author", f.api(revoked, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", revokedRev.LessonID), draftBody),
		http.StatusForbidden, "forbidden")
	disabled := f.newUser("disabled-owner", f.subject, permissions.Author)
	disabledRev := f.createLesson(disabled, "Disabled owner's lesson")
	wantStatus(t, "discard", f.api(disabled, http.MethodDelete, revPath(disabledRev.ID)+"?expectedVersion=1", nil), http.StatusNoContent, "")
	if _, err := f.db.Exec(context.Background(), `UPDATE users SET disabled_at = now() WHERE id = $1`, disabled.id); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "disabled owner", f.api(disabled, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", disabledRev.LessonID), draftBody),
		http.StatusUnauthorized, "unauthenticated")
	for _, l := range []int64{revokedRev.LessonID, disabledRev.LessonID} {
		if n := f.count(`SELECT count(*) FROM lesson_revisions WHERE lesson_id = $1`, l); n != 0 {
			t.Fatalf("lesson %d: refused owner stored %d revisions", l, n)
		}
	}
}

func TestStudioMediaUploadAndServe(t *testing.T) {
	f := newStudio(t)
	data := pngBytes(t, 10)

	w := f.uploadBytes(f.author, "image/png", data)
	wantStatus(t, "upload", w, http.StatusCreated, "")
	var up struct {
		ID          int64
		ContentType string
		ByteSize    int64
		Width       int
		Height      int
		SHA256      string
		URL         string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up.ID <= 0 || up.ContentType != "image/png" || up.Width != 8 || up.Height != 6 ||
		up.URL != fmt.Sprintf("/api/media/%d", up.ID) || w.Header().Get("Location") != up.URL || len(up.SHA256) != 64 {
		t.Fatalf("upload response: %s", w.Body)
	}

	// The same pixels again (here: the same file, and a re-encoding with
	// another compression level) reuse the asset.
	again := f.uploadBytes(f.author, "image/png", data)
	wantStatus(t, "re-upload", again, http.StatusCreated, "")
	var up2 struct{ ID int64 }
	_ = json.Unmarshal(again.Body.Bytes(), &up2)
	if up2.ID != up.ID {
		t.Fatalf("re-upload got id %d, want %d", up2.ID, up.ID)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var recoded bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&recoded, img); err != nil {
		t.Fatal(err)
	}
	other := f.newUser("author-two", f.subject, permissions.Author)
	w = f.uploadBytes(other, "image/png", recoded.Bytes())
	var up3 struct{ ID int64 }
	_ = json.Unmarshal(w.Body.Bytes(), &up3)
	if w.Code != http.StatusCreated || up3.ID != up.ID {
		t.Fatalf("same pixels re-encoded: %d %s, want 201 with existing id %d", w.Code, w.Body, up.ID)
	}
	if n := f.count(`SELECT count(*) FROM media_assets WHERE id = $1 AND created_by = $2`, up.ID, f.author.id); n != 1 {
		t.Fatalf("deduplicated upload changed created_by")
	}
	if n := f.count(`SELECT count(*) FROM media_uploads WHERE media_asset_id = $1`, up.ID); n != 2 {
		t.Fatalf("%d upload links, want one per uploader", n)
	}
	if n := f.count(`SELECT count(*) FROM media_assets`); n != 1 {
		t.Fatalf("%d asset rows, want 1", n)
	}

	// Serving.
	w = f.api(f.author, http.MethodGet, up.URL, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("serve: %d %s", w.Code, w.Body)
	}
	for k, v := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'; sandbox",
		"Cache-Control":           "private, no-store",
		"Content-Disposition":     "inline",
		"Content-Length":          strconv.FormatInt(up.ByteSize, 10),
	} {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s: %q, want %q", k, got, v)
		}
	}
	if _, err := png.Decode(bytes.NewReader(w.Body.Bytes())); err != nil || int64(w.Body.Len()) != up.ByteSize {
		t.Fatalf("served body is not the stored PNG: %v", err)
	}
	head := f.api(f.author, http.MethodHead, up.URL, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("HEAD: %d len=%d", head.Code, head.Body.Len())
	}

	// JPEG keeps its type.
	jimg := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := range jimg.Pix {
		jimg.Pix[i] = uint8(i)
	}
	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, jimg, nil); err != nil {
		t.Fatal(err)
	}
	w = f.uploadBytes(f.author, "image/jpeg", jbuf.Bytes())
	wantStatus(t, "jpeg upload", w, http.StatusCreated, "")
	var jup struct{ URL string }
	_ = json.Unmarshal(w.Body.Bytes(), &jup)
	if got := f.api(f.author, http.MethodGet, jup.URL, nil).Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("jpeg served as %q", got)
	}

	// Uploaded media can be referenced by a draft, by any author.
	rev := f.createLesson(other, "Lesson with image")
	w = f.api(other, http.MethodPut, revPath(rev.ID), map[string]any{
		"expectedVersion": 1, "content": testDoc("Lesson with image", "fr"), "mediaIds": []int64{up.ID, up.ID},
	})
	wantStatus(t, "save with media", w, http.StatusOK, "")
	if got := decodeRevision(t, w).MediaIDs; len(got) != 1 || got[0] != up.ID {
		t.Fatalf("media references: %v", got)
	}

	// Serving requires a session and the author permission.
	wantStatus(t, "anonymous serve", f.api(apiUser{}, http.MethodGet, up.URL, nil), http.StatusUnauthorized, "unauthenticated")
	student := f.newUser("student-one", f.subject)
	wantStatus(t, "student serve", f.api(student, http.MethodGet, up.URL, nil), http.StatusForbidden, "forbidden")
}

func TestStudioConcurrentIdenticalUploadsShareOneAsset(t *testing.T) {
	f := newStudio(t)
	data := pngBytes(t, 30)
	const n = 4
	ids := make([]int64, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := f.uploadBytes(f.author, "image/png", data)
			codes[i] = w.Code
			var b struct{ ID int64 }
			_ = json.Unmarshal(w.Body.Bytes(), &b)
			ids[i] = b.ID
		}()
	}
	wg.Wait()
	for i := range n {
		if codes[i] != http.StatusCreated {
			t.Fatalf("upload %d: %d", i, codes[i])
		}
		if ids[i] != ids[0] || ids[i] <= 0 {
			t.Fatalf("ids differ: %v", ids)
		}
	}
	if f.count(`SELECT count(*) FROM media_assets`) != 1 || f.count(`SELECT count(*) FROM media_uploads`) != 1 {
		t.Fatalf("rows: %d assets, %d links", f.count(`SELECT count(*) FROM media_assets`), f.count(`SELECT count(*) FROM media_uploads`))
	}
}

func TestStudioMediaRejections(t *testing.T) {
	f := newStudio(t)
	valid := pngBytes(t, 20)
	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	trailing := append(append([]byte{}, valid...), []byte("<?php echo 'polyglot'; ?>")...)

	for name, c := range map[string]struct {
		ctype  string
		data   []byte
		status int
		code   string
	}{
		"svg declared":         {"image/svg+xml", svg, 415, "unsupported_media_type"},
		"svg as png":           {"image/png", svg, 415, "unsupported_media_type"},
		"gif declared":         {"image/gif", gifBuf.Bytes(), 415, "unsupported_media_type"},
		"gif as png":           {"image/png", gifBuf.Bytes(), 415, "unsupported_media_type"},
		"png as jpeg":          {"image/jpeg", valid, 415, "type_mismatch"},
		"no content type":      {"", valid, 415, "unsupported_media_type"},
		"octet-stream":         {"application/octet-stream", valid, 415, "unsupported_media_type"},
		"multipart":            {"multipart/form-data; boundary=x", valid, 415, "unsupported_media_type"},
		"trailing data":        {"image/png", trailing, 422, "trailing_data"},
		"truncated":            {"image/png", valid[:len(valid)/2], 422, "invalid_image"},
		"empty":                {"image/png", nil, 400, "invalid_request"},
		"content-length > max": {"image/png", make([]byte, media.MaxUploadBytes+1), 413, "too_large"},
	} {
		w := f.uploadBytes(f.author, c.ctype, c.data)
		wantStatus(t, name, w, c.status, c.code)
	}
	if w := f.uploadBytes(f.author, "image/png", trailing); !strings.Contains(errorMessage(t, w), "PNG") {
		t.Fatalf("trailing-data message should tell authors what to do: %s", w.Body)
	}

	// A body that lies about (or omits) its length is cut off near the limit,
	// never read to the end.
	for name, declared := range map[string]int64{"chunked": -1, "understated": 1024} {
		cr := &countingReader{remaining: 64 << 20}
		w := f.upload(f.author, "image/png", cr, declared)
		wantStatus(t, name+" oversized", w, http.StatusRequestEntityTooLarge, "too_large")
		if cr.read > media.MaxUploadBytes+64<<10 {
			t.Fatalf("%s: read %d bytes of an oversized body", name, cr.read)
		}
	}
	// A declared oversized length is refused before reading anything.
	cr := &countingReader{remaining: 64 << 20}
	wantStatus(t, "declared oversized", f.upload(f.author, "image/png", cr, 64<<20), http.StatusRequestEntityTooLarge, "too_large")
	if cr.read != 0 {
		t.Fatalf("declared oversized body was read (%d bytes)", cr.read)
	}
	if n := f.count(`SELECT count(*) FROM media_assets`); n != 0 {
		t.Fatalf("rejected uploads stored %d assets", n)
	}
}

func TestStudioMediaFetchTampering(t *testing.T) {
	f := newStudio(t)
	// A row whose storage key is valid for the database but not a media key,
	// and a valid key with no file: neither is served.
	var sum [32]byte
	_, _ = rand.Read(sum[:])
	var badKey, missingFile int64
	err := f.db.QueryRow(context.Background(), `
		INSERT INTO media_assets (sha256, content_type, byte_size, width, height, storage_key, created_at, created_by)
		VALUES ($1, 'image/png', 10, 2, 2, $2, now(), $3) RETURNING id`,
		sum[:], "simulated/"+hex.EncodeToString(sum[:])+".png", f.author.id).Scan(&badKey)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rand.Read(sum[:])
	err = f.db.QueryRow(context.Background(), `
		INSERT INTO media_assets (sha256, content_type, byte_size, width, height, storage_key, created_at, created_by)
		VALUES ($1, 'image/png', 10, 2, 2, $2, now(), $3) RETURNING id`,
		sum[:], hex.EncodeToString(sum[:])+".png", f.author.id).Scan(&missingFile)
	if err != nil {
		t.Fatal(err)
	}
	// Give the author access, so only the key or file decides.
	if _, err := f.db.Exec(context.Background(), `INSERT INTO media_uploads (user_id, media_asset_id, created_at)
		SELECT $1, unnest($2::bigint[]), now()`, f.author.id, []int64{badKey, missingFile}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		fmt.Sprintf("/api/media/%d", badKey),
		fmt.Sprintf("/api/media/%d", missingFile),
		"/api/media/999999", "/api/media/abc", "/api/media/0", "/api/media/-1", "/api/media/01",
		"/api/media/1.png", "/api/media/99999999999999999999",
		"/api/media/" + hex.EncodeToString(sum[:]) + ".png",
		"/api/media/..%2F..%2Fetc%2Fpasswd", "/api/media/%2e%2e",
	} {
		wantStatus(t, p, f.api(f.author, http.MethodGet, p, nil), http.StatusNotFound, "not_found")
	}
	// Dot segments are cleaned by the router (redirect), never resolved on disk.
	if w := f.api(f.author, http.MethodGet, "/api/media/../../etc/passwd", nil); w.Code == http.StatusOK {
		t.Fatalf("dot-segment path served: %s", w.Body)
	}
}

type countingReader struct {
	remaining int64
	read      int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	for i := range p {
		p[i] = 0
	}
	c.remaining -= int64(len(p))
	c.read += int64(len(p))
	return len(p), nil
}

func errorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct{ Error struct{ Message string } }
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("not a JSON error: %s", w.Body)
	}
	return b.Error.Message
}

func TestStudioMediaAccessIsScoped(t *testing.T) {
	f := newStudio(t)
	w := f.uploadBytes(f.author, "image/png", pngBytes(t, 40))
	wantStatus(t, "upload", w, http.StatusCreated, "")
	var up struct {
		ID  int64
		URL string
	}
	_ = json.Unmarshal(w.Body.Bytes(), &up)
	stranger := f.newUser("author-two", f.subject, permissions.Author)

	// Another author who never uploaded the image cannot read it, and the
	// response equals the one for a missing ID.
	theirs := f.api(stranger, http.MethodGet, up.URL, nil)
	missing := f.api(stranger, http.MethodGet, fmt.Sprintf("/api/media/%d", up.ID+1000), nil)
	wantStatus(t, "cross-author read", theirs, http.StatusNotFound, "not_found")
	if theirs.Body.String() != missing.Body.String() {
		t.Fatalf("cross-author read differs from missing: %s vs %s", theirs.Body, missing.Body)
	}
	if w := f.api(stranger, http.MethodHead, up.URL, nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-author HEAD: %d", w.Code)
	}

	// Nor reference it in a draft.
	wantStatus(t, "reference on create", f.api(stranger, http.MethodPost, "/api/lessons", map[string]any{
		"subjectId": f.subject, "language": "fr", "content": testDoc("Stolen", "fr"), "mediaIds": []int64{up.ID},
	}), http.StatusUnprocessableEntity, "unknown_media")
	rev := f.createLesson(stranger, "Stranger lesson")
	wantStatus(t, "reference on save", f.api(stranger, http.MethodPut, revPath(rev.ID), map[string]any{
		"expectedVersion": 1, "content": testDoc("Stolen", "fr"), "mediaIds": []int64{up.ID},
	}), http.StatusUnprocessableEntity, "unknown_media")
	discarded := f.createLesson(f.author, "Lesson to continue")
	wantStatus(t, "discard", f.api(f.author, http.MethodDelete, revPath(discarded.ID)+"?expectedVersion=1", nil), http.StatusNoContent, "")
	wantStatus(t, "reference on new draft", f.api(stranger, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", discarded.LessonID),
		map[string]any{"content": testDoc("Stolen", "fr"), "mediaIds": []int64{up.ID}}), http.StatusUnprocessableEntity, "unknown_media")
	if n := f.count(`SELECT count(*) FROM revision_media WHERE media_asset_id = $1`, up.ID); n != 0 {
		t.Fatalf("rejected references stored: %d", n)
	}

	// A revision the user authored that references the asset grants read
	// access (simulated with a direct insert on their draft).
	if _, err := f.db.Exec(context.Background(), `INSERT INTO revision_media (revision_id, media_asset_id) VALUES ($1, $2)`, rev.ID, up.ID); err != nil {
		t.Fatal(err)
	}
	if w := f.api(stranger, http.MethodGet, up.URL, nil); w.Code != http.StatusOK {
		t.Fatalf("own draft's media: %d %s", w.Code, w.Body)
	}
	// and may be kept when saving that draft.
	wantStatus(t, "keep referenced media", f.api(stranger, http.MethodPut, revPath(rev.ID), map[string]any{
		"expectedVersion": 1, "content": testDoc("Kept", "fr"), "mediaIds": []int64{up.ID},
	}), http.StatusOK, "")

	// Uploading the same image gives the second author access too.
	third := f.newUser("author-three", f.subject, permissions.Author)
	wantStatus(t, "third before upload", f.api(third, http.MethodGet, up.URL, nil), http.StatusNotFound, "not_found")
	w = f.uploadBytes(third, "image/png", pngBytes(t, 40))
	wantStatus(t, "dedup upload", w, http.StatusCreated, "")
	var again struct{ ID int64 }
	_ = json.Unmarshal(w.Body.Bytes(), &again)
	if again.ID != up.ID {
		t.Fatalf("dedup id %d, want %d", again.ID, up.ID)
	}
	if w := f.api(third, http.MethodGet, up.URL, nil); w.Code != http.StatusOK {
		t.Fatalf("after own upload: %d", w.Code)
	}
}

func TestStudioStrictJSON(t *testing.T) {
	f := newStudio(t)
	rev := f.createLesson(f.author, "Strict lesson")
	doc, _ := json.Marshal(testDoc("Strict", "fr"))
	for name, body := range map[string]string{
		"extra closing brace": `{"expectedVersion":1}}`,
		"extra bracket":       `{"expectedVersion":1}]`,
		"trailing value":      `{"expectedVersion":1} {"expectedVersion":1}`,
		"trailing garbage":    `{"expectedVersion":1} x`,
		"case-folded key":     `{"ExpectedVersion":1}`,
		"duplicate key":       `{"expectedVersion":1,"expectedVersion":1}`,
		"array":               `[{"expectedVersion":1}]`,
		"wrong type":          `{"expectedVersion":"1"}`,
		"null version":        `{"expectedVersion":null}`,
		"fractional version":  `{"expectedVersion":1.0}`,
		"unknown key":         `{"expectedVersion":1,"x":1}`,
	} {
		w := f.api(f.author, http.MethodPost, revPath(rev.ID)+"/submit", []byte(body))
		wantStatus(t, name, w, http.StatusBadRequest, "invalid_request")
		if strings.Contains(errorMessage(t, w), "Go ") || strings.Contains(errorMessage(t, w), "studioapi") {
			t.Fatalf("%s: message leaks Go details: %s", name, w.Body)
		}
	}
	// Errors name the JSON key, never the Go field (issue #51).
	w := f.api(f.author, http.MethodPut, revPath(rev.ID), []byte(`{"expectedVersion":1,"content":`+string(doc)+`,"mediaIds":[1.0]}`))
	wantStatus(t, "fractional media id", w, http.StatusBadRequest, "invalid_request")
	if msg := errorMessage(t, w); !strings.Contains(msg, `"mediaIds"`) || strings.Contains(msg, "fieldsJSON") || strings.Contains(msg, "int64") {
		t.Fatalf("fractional media id: message %q", msg)
	}
	for name, body := range map[string]string{
		"case-folded content key": `{"expectedVersion":1,"Content":` + string(doc) + `}`,
		"duplicate content":       `{"expectedVersion":1,"content":` + string(doc) + `,"content":` + string(doc) + `}`,
	} {
		wantStatus(t, name, f.api(f.author, http.MethodPut, revPath(rev.ID), []byte(body)), http.StatusBadRequest, "invalid_request")
	}
	if got := decodeRevision(t, f.api(f.author, http.MethodGet, revPath(rev.ID), nil)); got.Version != 1 || got.Status != "draft" {
		t.Fatalf("rejected bodies changed the draft: %+v", got)
	}
	// Whitespace around the object is fine.
	wantStatus(t, "padded", f.api(f.author, http.MethodPost, revPath(rev.ID)+"/submit", []byte(" {\"expectedVersion\":1}\n")), http.StatusOK, "")
}

func TestStudioContentMessagesAreClean(t *testing.T) {
	f := newStudio(t)
	nul := testDoc("Nul", "fr")
	nul["blocks"] = []any{map[string]any{"type": "paragraph", "children": []any{map[string]any{"type": "text", "text": "a\u0000b"}}}}
	for name, c := range map[string]struct {
		body map[string]any
		want string
	}{
		"NUL in content":       {map[string]any{"content": nul}, "NUL"},
		"NUL in prerequisites": {map[string]any{"content": testDoc("P", "fr"), "prerequisites": "a\u0000b"}, "NUL"},
		"content is a string":  {map[string]any{"content": "text"}, "JSON object"},
		"field of wrong type":  {map[string]any{"content": map[string]any{"schemaVersion": "one"}}, "schemaVersion"},
	} {
		c.body["subjectId"], c.body["language"] = f.subject, "fr"
		w := f.api(f.author, http.MethodPost, "/api/lessons", c.body)
		wantStatus(t, name, w, http.StatusUnprocessableEntity, "invalid_content")
		msg := errorMessage(t, w)
		if !strings.Contains(msg, c.want) || strings.Contains(msg, "Go ") || strings.Contains(msg, "rendering.") ||
			strings.Contains(msg, "SQLSTATE") || strings.Contains(msg, "jsonb") {
			t.Fatalf("%s: message %q", name, msg)
		}
	}
	if n := f.count(`SELECT count(*) FROM lesson_revisions`); n != 0 {
		t.Fatalf("%d revisions stored", n)
	}
}

// A body that stops arriving hits the server's read deadline: 408, not 503.
func TestStudioUploadReadTimeout(t *testing.T) {
	f := newStudio(t)
	srv := httptest.NewUnstartedServer(f.h)
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()

	pr, pw := io.Pipe()
	defer pw.Close()
	go func() {
		_, _ = pw.Write(pngBytes(t, 50)[:20]) // then stall
	}()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/media", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("Cookie", (&http.Cookie{Name: "__Host-learnoo_session", Value: f.author.cookie}).String())
	req.Header.Set("X-CSRF-Token", f.author.csrf)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b struct{ Error struct{ Code string } }
	_ = json.NewDecoder(resp.Body).Decode(&b)
	if resp.StatusCode != http.StatusRequestTimeout || b.Error.Code != "upload_timeout" || resp.Header.Get("Retry-After") != "" {
		t.Fatalf("slow upload: %d %q Retry-After=%q", resp.StatusCode, b.Error.Code, resp.Header.Get("Retry-After"))
	}
}
