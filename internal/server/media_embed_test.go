package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
)

// Uploaded images embedded in lesson content (issue #77): the save and
// submission rules, the Studio preview's media routes, and the reader's
// lesson-scoped media route. Simulated users and TEST documents only.

// mediaDoc is testDoc with one uploaded image per ID appended, and the
// fixture image when withFixture is set.
func mediaDoc(title string, withFixture bool, ids ...int64) map[string]any {
	doc := testDoc(title, "fr")
	blocks := doc["blocks"].([]any)
	if withFixture {
		blocks = append(blocks, map[string]any{"type": "image", "src": "/static/complex-plane.svg", "alt": "Plan de test"})
	}
	for _, id := range ids {
		blocks = append(blocks, map[string]any{"type": "image", "src": fmt.Sprintf("media:%d", id), "alt": fmt.Sprintf("TEST image %d", id), "caption": "TEST légende"})
	}
	doc["blocks"] = blocks
	return doc
}

// uploadAs uploads a generated PNG as u and returns its media ID.
func (e *env) uploadAs(u apiUser, seed uint8) int64 {
	e.t.Helper()
	w := e.uploadBytes(u, "image/png", pngBytes(e.t, seed))
	wantStatus(e.t, "upload", w, http.StatusCreated, "")
	var up struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil || up.ID == 0 {
		e.t.Fatalf("upload: %v %s", err, w.Body)
	}
	return up.ID
}

func (e *env) createWithMedia(u apiUser, subject int64, content any, mediaIDs []int64) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.api(u, http.MethodPost, "/api/lessons", map[string]any{
		"subjectId": subject, "language": "fr", "content": content, "mediaIds": mediaIDs,
	})
}

func TestEmbeddedMediaMustBeAttachedAndReadable(t *testing.T) {
	f := newReview(t)
	other := f.newUser("author-two", f.subject, permissions.Author)
	mine := f.uploadAs(f.author, 101)
	theirs := f.uploadAs(other, 102)
	ctx := context.Background()

	// Embedded but not attached.
	wantStatus(t, "unattached", f.createWithMedia(f.author, f.subject, mediaDoc("TEST non jointe", false, mine), []int64{}),
		http.StatusUnprocessableEntity, "unattached_media")
	wantStatus(t, "unattached among others", f.createWithMedia(f.author, f.subject, mediaDoc("TEST non jointe", false, mine, theirs), []int64{mine}),
		http.StatusUnprocessableEntity, "unattached_media")
	// Attached but not readable by the author, or missing: the same answer.
	wantStatus(t, "unreadable", f.createWithMedia(f.author, f.subject, mediaDoc("TEST autre", false, theirs), []int64{theirs}),
		http.StatusUnprocessableEntity, "unknown_media")
	wantStatus(t, "missing", f.createWithMedia(f.author, f.subject, mediaDoc("TEST absente", false, 999999999), []int64{999999999}),
		http.StatusUnprocessableEntity, "unknown_media")
	// Malformed references are schema errors.
	bad := mediaDoc("TEST mal formée", false)
	bad["blocks"] = append(bad["blocks"].([]any), map[string]any{"type": "image", "src": fmt.Sprintf("media:0%d", mine), "alt": "x"})
	wantStatus(t, "malformed", f.createWithMedia(f.author, f.subject, bad, []int64{mine}), http.StatusUnprocessableEntity, "invalid_content")
	noAlt := mediaDoc("TEST sans alt", false)
	noAlt["blocks"] = append(noAlt["blocks"].([]any), map[string]any{"type": "image", "src": fmt.Sprintf("media:%d", mine)})
	wantStatus(t, "missing alt", f.createWithMedia(f.author, f.subject, noAlt, []int64{mine}), http.StatusUnprocessableEntity, "invalid_content")
	if n := f.count(`SELECT count(*) FROM lessons`); n != 0 {
		t.Fatalf("%d lessons stored by refused requests", n)
	}

	// Valid: an attached, readable upload next to the fixture image.
	w := f.createWithMedia(f.author, f.subject, mediaDoc("TEST mixte", true, mine), []int64{mine})
	wantStatus(t, "create", w, http.StatusCreated, "")
	rev := decodeRevision(t, w)
	if !bytes.Contains(rev.Content, []byte(fmt.Sprintf(`"src":"media:%d"`, mine))) || !bytes.Contains(rev.Content, []byte(`"caption":"TEST légende"`)) {
		t.Fatalf("stored content: %s", rev.Content)
	}

	// Saving: the same rules; removing the last reference keeps the attachment.
	save := func(content any, ids []int64) *httptest.ResponseRecorder {
		return f.api(f.author, http.MethodPut, revPath(rev.ID), map[string]any{"expectedVersion": rev.Version, "content": content, "mediaIds": ids})
	}
	wantStatus(t, "save unattached", save(mediaDoc("TEST mixte", true, mine), []int64{}), http.StatusUnprocessableEntity, "unattached_media")
	wantStatus(t, "save unreadable", save(mediaDoc("TEST mixte", true, theirs), []int64{mine, theirs}), http.StatusUnprocessableEntity, "unknown_media")
	w = save(mediaDoc("TEST sans image", false), []int64{mine})
	wantStatus(t, "save without reference", w, http.StatusOK, "")
	rev = decodeRevision(t, w)
	if len(rev.MediaIDs) != 1 || rev.MediaIDs[0] != mine {
		t.Fatalf("attachment dropped: %v", rev.MediaIDs)
	}
	w = save(mediaDoc("TEST mixte", true, mine), []int64{mine})
	wantStatus(t, "save with reference", w, http.StatusOK, "")
	rev = decodeRevision(t, w)

	// Submission rechecks the stored snapshot: a draft whose attachment was
	// removed behind the API (drafts allow it) cannot enter review.
	if _, err := f.db.Exec(ctx, `DELETE FROM revision_media WHERE revision_id = $1`, rev.ID); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "submit unattached", f.api(f.author, http.MethodPost, revPath(rev.ID)+"/submit", map[string]any{"expectedVersion": rev.Version}),
		http.StatusUnprocessableEntity, "unattached_media")
	if _, err := f.db.Exec(ctx, `INSERT INTO revision_media (revision_id, media_asset_id) VALUES ($1, $2)`, rev.ID, mine); err != nil {
		t.Fatal(err)
	}
	sub := f.submitRev(f.author, rev)
	// The submitted snapshot freezes content and attachments together.
	if _, err := f.db.Exec(ctx, `DELETE FROM revision_media WHERE revision_id = $1`, sub.ID); err == nil {
		t.Fatal("submitted media references changed")
	}
	// The reviewer sees the embedded reference and fetches the image.
	w = f.api(f.reviewer, http.MethodGet, reviewPath(sub.ID), nil)
	wantStatus(t, "review revision", w, http.StatusOK, "")
	if !strings.Contains(w.Body.String(), fmt.Sprintf(`media:%d`, mine)) {
		t.Fatalf("review content lacks the reference: %s", w.Body)
	}
	wantStatus(t, "review media", f.api(f.reviewer, http.MethodGet, fmt.Sprintf("/api/review/media/%d", mine), nil), http.StatusOK, "")
}

func TestPreviewLinksEmbeddedMediaToTheViewersRoute(t *testing.T) {
	f := newStudio(t)
	content := mediaDoc("TEST aperçu", true, 7)
	for route, prefix := range map[string]string{"": "/api/media/", "author": "/api/media/", "review": "/api/review/media/"} {
		body := map[string]any{"content": content}
		if route != "" {
			body["mediaRoute"] = route
		}
		html := previewOf(t, f.api(f.author, http.MethodPost, "/api/preview", body))
		want := `<figure class="lesson-media"><img src="` + prefix + `7" alt="TEST image 7" loading="lazy"><figcaption>TEST légende</figcaption></figure>`
		if !strings.Contains(html, want) || strings.Contains(html, "media:7") {
			t.Fatalf("route %q: %s", route, html)
		}
		if !strings.Contains(html, `src="/static/complex-plane.svg"`) {
			t.Fatalf("route %q lost the fixture image: %s", route, html)
		}
	}
	for _, raw := range []string{`"reader"`, `"Author"`, `null`, `1`} {
		body := []byte(`{"content":` + string(mustJSON(t, content)) + `,"mediaRoute":` + raw + `}`)
		wantStatus(t, "mediaRoute "+raw, f.api(f.author, http.MethodPost, "/api/preview", body), http.StatusBadRequest, "invalid_request")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mediaPath(publicID string, id int64) string {
	return fmt.Sprintf("%s/media/%d", lessonPath(publicID), id)
}

// wantMedia expects the reader media route to serve the image privately.
func wantMedia(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", what, w.Code, w.Body)
	}
	for k, v := range map[string]string{
		"Content-Type": "image/png", "Cache-Control": "private, no-store", "X-Content-Type-Options": "nosniff",
		"Content-Disposition": "inline", "Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'; sandbox", "Vary": "Cookie",
	} {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s: %s %q, want %q", what, k, got, v)
		}
	}
}

// wantNoMedia expects the enumeration-safe 404 (no redirect, no lesson data).
func wantNoMedia(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusNotFound || errorCode(t, w) != "not_found" {
		t.Fatalf("%s: %d %s", what, w.Code, w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("%s: Cache-Control %q", what, cc)
	}
}

func TestReaderServesOnlyCurrentlyPublishedLessonMedia(t *testing.T) {
	rf := newReader(t)
	wf := withdrawFixture{reviewFixture: reviewFixture{studioFixture: studioFixture{env: rf.env, subject: rf.subject, author: rf.author},
		reviewer: rf.reviewer, publisher: rf.publisher}, admin: rf.admin}
	first, second, attachedOnly := rf.uploadAs(rf.author, 111), rf.uploadAs(rf.author, 112), rf.uploadAs(rf.author, 113)
	otherAuthor := rf.newUser("test-author-two", rf.subject, permissions.Author)
	otherDraft := rf.uploadAs(otherAuthor, 114)
	ctx := context.Background()

	// First edition: embeds `first`, attaches `attachedOnly` without embedding it.
	w := rf.createWithMedia(rf.author, rf.subject, mediaDoc("TEST Image première édition", true, first), []int64{first, attachedOnly})
	wantStatus(t, "create", w, http.StatusCreated, "")
	rev1 := wf.submitRev(rf.author, decodeRevision(t, w))
	publicID := rf.publicID(rev1.LessonID)
	student := rf.student
	student2 := rf.invitedStudent("test-student-two@example.test", "TEST Élève Deux")

	// In review: the reviewer fetches it, students do not.
	wantStatus(t, "reviewer", rf.api(rf.reviewer, http.MethodGet, fmt.Sprintf("/api/review/media/%d", first), nil), http.StatusOK, "")
	wantNoMedia(t, "unpublished", rf.get(student, mediaPath(publicID, first)))

	wf.approvedRev(rev1)
	wantNoMedia(t, "approved, not published", rf.get(student, mediaPath(publicID, first)))
	wf.publishedRev(rev1)

	page := rf.get(student, lessonPath(publicID))
	wantPage(t, "lesson page", page, http.StatusOK, fmt.Sprintf(`<img src="%s" alt="TEST image %d" loading="lazy">`, mediaPath(publicID, first), first))
	wantMedia(t, "student", rf.get(student, mediaPath(publicID, first)))
	wantMedia(t, "another student", rf.get(student2, mediaPath(publicID, first)))
	head := rf.page(student, http.MethodHead, mediaPath(publicID, first), nil)
	wantMedia(t, "HEAD", head)
	if head.Body.Len() != 0 {
		t.Fatal("HEAD returned a body")
	}
	got := rf.get(student, mediaPath(publicID, first))
	if !bytes.Equal(got.Body.Bytes()[:8], []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("not the stored PNG")
	}

	// Refused alike: anonymous, attached but not embedded, another author's
	// draft image, malformed IDs and paths, the author and review routes for
	// a student.
	wantNoMedia(t, "anonymous", rf.get(apiUser{}, mediaPath(publicID, first)))
	wantNoMedia(t, "attached only", rf.get(student, mediaPath(publicID, attachedOnly)))
	wantNoMedia(t, "other draft media", rf.get(student, mediaPath(publicID, otherDraft)))
	for _, p := range []string{
		lessonPath(publicID) + "/media/0", lessonPath(publicID) + fmt.Sprintf("/media/0%d", first), lessonPath(publicID) + "/media/abc",
		lessonPath(publicID) + "/media/1e3", lessonPath(publicID) + "/media/99999999999999999999", lessonPath(publicID) + fmt.Sprintf("/media/%d.png", first),
		lessonPath(strings.ToUpper(publicID)) + fmt.Sprintf("/media/%d", first), lessonPath("not-a-uuid") + fmt.Sprintf("/media/%d", first),
	} {
		wantNoMedia(t, p, rf.get(student, p))
	}
	for _, p := range []string{fmt.Sprintf("/api/media/%d", first), fmt.Sprintf("/api/review/media/%d", first)} {
		if w := rf.api(student, http.MethodGet, p, nil); w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
			t.Fatalf("student on %s: %d", p, w.Code)
		}
	}

	// Another published lesson's path does not serve this lesson's image.
	other := rf.publishNew("TEST Autre leçon", "fr", nil)
	wantNoMedia(t, "other lesson", rf.get(student, mediaPath(other.publicID, first)))

	// A successor draft's new image is not served while it is unpublished.
	w = rf.api(rf.author, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", rev1.LessonID),
		map[string]any{"content": mediaDoc("TEST Image deuxième édition", false, second), "mediaIds": []int64{second}})
	wantStatus(t, "successor", w, http.StatusCreated, "")
	succ := decodeRevision(t, w)
	wantNoMedia(t, "draft image", rf.get(student, mediaPath(publicID, second)))
	rev2 := wf.submitRev(rf.author, succ)
	wf.approvedRev(rev2)
	wantNoMedia(t, "approved successor image", rf.get(student, mediaPath(publicID, second)))
	wantMedia(t, "first still published", rf.get(student, mediaPath(publicID, first)))

	// Publishing the second edition switches the served images.
	wf.publishedRev(rev2)
	wantMedia(t, "second published", rf.get(student, mediaPath(publicID, second)))
	wantNoMedia(t, "first no longer published", rf.get(student, mediaPath(publicID, first)))

	// Withdrawal of the published revision: nothing is served.
	wantStatus(t, "withdraw", rf.api(rf.admin, http.MethodPost, withdrawPath(rev2.ID),
		map[string]any{"reason": "TEST withdrawal.", "expectedPublishedRevisionId": rev2.ID}), http.StatusOK, "")
	wantNoMedia(t, "withdrawn", rf.get(student, mediaPath(publicID, second)))
	wantNoMedia(t, "withdrawn, earlier image", rf.get(student, mediaPath(publicID, first)))

	// Restoring the first edition serves its image again, not the withdrawn one.
	wantStatus(t, "restore", rf.api(rf.publisher, http.MethodPost, restorePath(rev1.ID),
		map[string]any{"expectedPublishedRevisionId": nil}), http.StatusOK, "")
	wantMedia(t, "restored", rf.get(student, mediaPath(publicID, first)))
	wantNoMedia(t, "withdrawn after restore", rf.get(student, mediaPath(publicID, second)))

	// Emergency unpublication, then archival.
	rf.unpublish(rev1.LessonID, rev1.ID)
	wantNoMedia(t, "unpublished", rf.get(student, mediaPath(publicID, first)))
	wantStatus(t, "restore again", rf.api(rf.publisher, http.MethodPost, restorePath(rev1.ID),
		map[string]any{"expectedPublishedRevisionId": nil}), http.StatusOK, "")
	wantMedia(t, "restored again", rf.get(student, mediaPath(publicID, first)))
	if _, err := rf.db.Exec(ctx, `UPDATE lessons SET archived_at = now() WHERE id = $1`, rev1.LessonID); err != nil {
		t.Fatal(err)
	}
	wantNoMedia(t, "archived", rf.get(student, mediaPath(publicID, first)))
}

// The preview and the reader page differ only in the media URLs.
func TestPreviewMatchesReaderExceptMediaURLs(t *testing.T) {
	rf := newReader(t)
	wf := withdrawFixture{reviewFixture: reviewFixture{studioFixture: studioFixture{env: rf.env, subject: rf.subject, author: rf.author},
		reviewer: rf.reviewer, publisher: rf.publisher}, admin: rf.admin}
	id := rf.uploadAs(rf.author, 121)
	content := mediaDoc("TEST Aperçu et lecture", true, id)
	w := rf.createWithMedia(rf.author, rf.subject, content, []int64{id})
	wantStatus(t, "create", w, http.StatusCreated, "")
	rev := wf.approvedRev(wf.submitRev(rf.author, decodeRevision(t, w)))
	wf.publishedRev(rev)
	publicID := rf.publicID(rev.LessonID)

	html := previewOf(t, rf.api(rf.author, http.MethodPost, "/api/preview", map[string]any{"content": content}))
	page := rf.get(rf.student, lessonPath(publicID)).Body.String()
	readerHTML := strings.ReplaceAll(html, "/api/media/", lessonPath(publicID)+"/media/")
	if readerHTML == html {
		t.Fatal("preview has no media URL")
	}
	// Heading, objective and body: the preview fragment is the reader's
	// lesson markup once the media prefix is swapped.
	start := strings.Index(page, "<h1 ")
	if start < 0 || !strings.Contains(page, readerHTML[strings.Index(readerHTML, "<article"):]) || !strings.Contains(page[start:], readerHTML[:strings.Index(readerHTML, "</h1>")]) {
		t.Fatalf("reader page differs from the preview beyond media URLs:\npreview: %s", readerHTML)
	}
}
