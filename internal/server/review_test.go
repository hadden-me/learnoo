package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
)

// Review and publication endpoints (LRN-007) through the real server
// handler. Simulated users, subjects and unreviewed test documents only.

type reviewFixture struct {
	studioFixture
	reviewer  apiUser
	publisher apiUser
}

func newReview(t *testing.T) reviewFixture {
	f := newStudio(t)
	return reviewFixture{studioFixture: f,
		reviewer:  f.newUser("reviewer-one", f.subject, permissions.Reviewer),
		publisher: f.newUser("publisher-one", f.subject, permissions.Publisher)}
}

func reviewPath(id int64) string { return "/api/review/revisions/" + strconv.FormatInt(id, 10) }

func publishPath(id int64) string { return revPath(id) + "/publish" }

// submittedLesson creates a lesson as u, with optional media, and submits it.
func (f reviewFixture) submittedLesson(u apiUser, title string, media ...int64) revisionBody {
	f.t.Helper()
	body := map[string]any{"subjectId": f.subject, "language": "fr", "content": testDoc(title, "fr")}
	if len(media) > 0 {
		body["mediaIds"] = media
	}
	w := f.api(u, http.MethodPost, "/api/lessons", body)
	wantStatus(f.t, "create lesson", w, http.StatusCreated, "")
	return f.submitRev(u, decodeRevision(f.t, w))
}

func (f reviewFixture) submitRev(u apiUser, rev revisionBody) revisionBody {
	f.t.Helper()
	w := f.api(u, http.MethodPost, revPath(rev.ID)+"/submit", map[string]any{"expectedVersion": rev.Version})
	wantStatus(f.t, "submit", w, http.StatusOK, "")
	return decodeRevision(f.t, w)
}

type reviewedBody struct {
	revisionBody
	Review *struct {
		Action     string  `json:"action"`
		ReviewerID int64   `json:"reviewerId"`
		Comment    *string `json:"comment"`
		DecidedAt  string  `json:"decidedAt"`
	} `json:"review"`
	ApprovedBy *int64 `json:"approvedBy"`
}

func decodeReviewed(t *testing.T, w *httptest.ResponseRecorder) reviewedBody {
	t.Helper()
	var b reviewedBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("not a revision: %s", w.Body)
	}
	return b
}

// reviewState captures everything a rejected request must leave unchanged.
func (f reviewFixture) reviewState() string {
	f.t.Helper()
	var s string
	err := f.db.QueryRow(context.Background(), `
		SELECT coalesce((SELECT string_agg(id || ':' || status || ':' || updated_at, ',' ORDER BY id) FROM lesson_revisions), '') || '|' ||
		       (SELECT count(*) FROM review_decisions) || '|' || (SELECT count(*) FROM publication_audit) || '|' ||
		       coalesce((SELECT string_agg(id || ':' || coalesce(published_revision_id::text, '-'), ',' ORDER BY id) FROM lessons), '')`).Scan(&s)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// reviewEndpoints lists every LRN-007 route with a valid body.
func reviewEndpoints(revID, mediaID int64) []struct {
	method, path string
	body         any
} {
	return []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/review/queue", nil},
		{http.MethodGet, reviewPath(revID), nil},
		{http.MethodPost, reviewPath(revID) + "/approve", map[string]any{"comment": "Fine."}},
		{http.MethodPost, reviewPath(revID) + "/request-changes", map[string]any{"comment": "Simulated comment."}},
		{http.MethodGet, fmt.Sprintf("/api/review/media/%d", mediaID), nil},
		{http.MethodPost, publishPath(revID), map[string]any{}},
	}
}

func (f reviewFixture) uploadedMedia() int64 {
	f.t.Helper()
	w := f.uploadBytes(f.author, "image/png", pngBytes(f.t, 42))
	wantStatus(f.t, "upload", w, http.StatusCreated, "")
	var up struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		f.t.Fatal(err)
	}
	return up.ID
}

func TestReviewAnonymousAndCrossSiteRequestsAreRejected(t *testing.T) {
	f := newReview(t)
	img := f.uploadedMedia()
	rev := f.submittedLesson(f.author, "Simulated lesson", img)
	before := f.reviewState()
	for _, ep := range reviewEndpoints(rev.ID, img) {
		name := ep.method + " " + ep.path
		wantStatus(t, "anonymous "+name, f.api(apiUser{}, ep.method, ep.path, ep.body, withCSRF("anything")),
			http.StatusUnauthorized, "unauthenticated")
		if ep.method == http.MethodGet {
			continue
		}
		u := f.reviewer
		if strings.HasSuffix(ep.path, "/publish") {
			u = f.publisher
		}
		for label, opts := range map[string][]reqOpt{
			"missing CSRF":      {withCookie(u.cookie)},
			"wrong CSRF":        {withCookie(u.cookie), withCSRF("not-the-token")},
			"other user's CSRF": {withCookie(u.cookie), withCSRF(f.author.csrf)},
		} {
			wantStatus(t, label+" "+name, f.api(apiUser{}, ep.method, ep.path, ep.body, opts...), http.StatusForbidden, "csrf_rejected")
		}
		for label, opt := range map[string]reqOpt{
			"Sec-Fetch-Site": withHeader("Sec-Fetch-Site", "cross-site"),
			"Origin":         withHeader("Origin", "https://evil.example"),
		} {
			wantStatus(t, "cross-origin via "+label+" "+name, f.api(u, ep.method, ep.path, ep.body, opt),
				http.StatusForbidden, "cross_origin_rejected")
		}
	}
	if after := f.reviewState(); after != before {
		t.Fatalf("rejected requests changed state:\n%s\n%s", before, after)
	}
}

// Accounts without any reviewer assignment get 403 on review routes, and
// accounts without publisher get 403 on publication, before any lookup, so
// existing and missing IDs look the same.
func TestReviewRoutesRequirePermissions(t *testing.T) {
	f := newReview(t)
	img := f.uploadedMedia()
	rev := f.submittedLesson(f.author, "Simulated lesson", img)
	admin := f.newUser("admin-one", f.subject, permissions.Administrator, permissions.Publisher, permissions.Author)
	before := f.reviewState()
	for who, u := range map[string]apiUser{
		"student":        f.newUser("student-one", f.subject),
		"author":         f.author,
		"publisher-only": f.publisher,
		"admin":          admin,
	} {
		for _, id := range []int64{rev.ID, rev.ID + 1000} {
			for _, ep := range reviewEndpoints(id, img) {
				if strings.HasSuffix(ep.path, "/publish") {
					continue
				}
				wantStatus(t, who+" "+ep.method+" "+ep.path, f.api(u, ep.method, ep.path, ep.body), http.StatusForbidden, "forbidden")
			}
		}
	}
	for who, u := range map[string]apiUser{
		"student":                    f.newUser("student-two", f.subject),
		"author":                     f.author,
		"reviewer without publisher": f.reviewer,
		"administrator only":         f.newUser("admin-two", f.subject, permissions.Administrator),
	} {
		for _, id := range []int64{rev.ID, rev.ID + 1000} {
			wantStatus(t, who+" publish", f.api(u, http.MethodPost, publishPath(id), map[string]any{}), http.StatusForbidden, "forbidden")
		}
	}
	if after := f.reviewState(); after != before {
		t.Fatalf("denied requests changed state:\n%s\n%s", before, after)
	}

	// Revocation applies to the next request.
	if _, err := permissions.NewStore(f.db).RevokeReviewer(context.Background(), testOperator, f.reviewer.id, f.subject); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "revoked reviewer", f.api(f.reviewer, http.MethodGet, reviewPath(rev.ID), nil), http.StatusForbidden, "forbidden")
	// A disabled reviewer is denied too.
	other := f.newUser("reviewer-two", f.subject, permissions.Reviewer)
	if _, err := f.db.Exec(context.Background(), `UPDATE users SET disabled_at = now() WHERE id = $1`, other.id); err != nil {
		t.Fatal(err)
	}
	// Disabled accounts lose their session; the in-transaction recheck
	// covers a disablement racing a request (internal/review tests).
	wantStatus(t, "disabled reviewer", f.api(other, http.MethodPost, reviewPath(rev.ID)+"/approve", map[string]any{}),
		http.StatusUnauthorized, "unauthenticated")
	if after := f.reviewState(); after != before {
		t.Fatalf("revoked or disabled reviewers changed state:\n%s\n%s", before, after)
	}
}

// A reviewer of another subject gets exactly the response for a missing
// revision or asset.
func TestReviewOtherSubjectGetsNotFound(t *testing.T) {
	f := newReview(t)
	img := f.uploadedMedia()
	rev := f.submittedLesson(f.author, "Simulated lesson", img)
	physics := f.newSubject("simulated-physics")
	other := f.newUser("physics-reviewer", physics, permissions.Reviewer)
	before := f.reviewState()
	for _, ep := range reviewEndpoints(rev.ID, img) {
		if ep.path == "/api/review/queue" || strings.HasSuffix(ep.path, "/publish") {
			continue
		}
		theirs := f.api(other, ep.method, ep.path, ep.body)
		missingPath := strings.Replace(ep.path, strconv.FormatInt(rev.ID, 10), strconv.FormatInt(rev.ID+1000, 10), 1)
		if strings.HasPrefix(ep.path, "/api/review/media/") {
			missingPath = fmt.Sprintf("/api/review/media/%d", img+1000)
		}
		absent := f.api(other, ep.method, missingPath, ep.body)
		wantStatus(t, "other subject "+ep.method+" "+ep.path, theirs, http.StatusNotFound, "not_found")
		if theirs.Code != absent.Code || theirs.Body.String() != absent.Body.String() {
			t.Fatalf("%s %s: other subject (%d %s) differs from missing (%d %s)", ep.method, ep.path,
				theirs.Code, theirs.Body, absent.Code, absent.Body)
		}
	}
	w := f.api(other, http.MethodGet, "/api/review/queue", nil)
	wantStatus(t, "other subject queue", w, http.StatusOK, "")
	if !strings.Contains(w.Body.String(), `"revisions":[]`) {
		t.Fatalf("other subject queue: %s", w.Body)
	}
	wantStatus(t, "queue of unassigned subject", f.api(other, http.MethodGet, fmt.Sprintf("/api/review/queue?subjectId=%d", f.subject), nil),
		http.StatusNotFound, "not_found")
	if after := f.reviewState(); after != before {
		t.Fatalf("other-subject requests changed state:\n%s\n%s", before, after)
	}
}

func TestReviewCycleThroughPublication(t *testing.T) {
	f := newReview(t)
	img := f.uploadedMedia()
	rev := f.submittedLesson(f.author, "Première version", img)

	// The queue lists it; the reviewer reads it and its media.
	w := f.api(f.reviewer, http.MethodGet, fmt.Sprintf("/api/review/queue?subjectId=%d", f.subject), nil)
	wantStatus(t, "queue", w, http.StatusOK, "")
	var queue struct {
		Revisions []struct {
			RevisionID, LessonID, AuthorID, SubjectID int64
			Title                                     string
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &queue); err != nil || len(queue.Revisions) != 1 ||
		queue.Revisions[0].RevisionID != rev.ID || queue.Revisions[0].AuthorID != f.author.id || queue.Revisions[0].Title != "Première version" {
		t.Fatalf("queue %s", w.Body)
	}
	got := decodeReviewed(t, f.api(f.reviewer, http.MethodGet, reviewPath(rev.ID), nil))
	if got.ID != rev.ID || got.Status != "in_review" || got.Review != nil || got.ApprovedBy != nil || !strings.Contains(string(got.Content), "Première version") {
		t.Fatalf("reviewer read: %+v", got)
	}
	w = f.api(f.reviewer, http.MethodGet, fmt.Sprintf("/api/review/media/%d", img), nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("reviewer media: %d %v", w.Code, w.Header())
	}
	// Publishing an unreviewed revision is impossible.
	wantStatus(t, "publish in review", f.api(f.publisher, http.MethodPost, publishPath(rev.ID), map[string]any{}), http.StatusConflict, "not_approved")

	// Changes requested need a comment, which the author then reads.
	for _, body := range []any{map[string]any{}, map[string]any{"comment": "  "}, map[string]any{"comment": nil}} {
		wantStatus(t, "request changes without comment", f.api(f.reviewer, http.MethodPost, reviewPath(rev.ID)+"/request-changes", body),
			http.StatusUnprocessableEntity, "comment_required")
	}
	wantStatus(t, "comment too long", f.api(f.reviewer, http.MethodPost, reviewPath(rev.ID)+"/request-changes",
		map[string]any{"comment": strings.Repeat("x", 5001)}), http.StatusUnprocessableEntity, "invalid_comment")
	w = f.api(f.reviewer, http.MethodPost, reviewPath(rev.ID)+"/request-changes", map[string]any{"comment": "Préciser l'exercice."})
	wantStatus(t, "request changes", w, http.StatusOK, "")
	got = decodeReviewed(t, w)
	if got.Status != "changes_requested" || got.Review == nil || got.Review.Action != "request_changes" || got.Review.ReviewerID != f.reviewer.id ||
		got.Review.Comment == nil || *got.Review.Comment != "Préciser l'exercice." || got.ApprovedBy != nil {
		t.Fatalf("changes requested: %s", w.Body)
	}
	mine := decodeReviewed(t, f.api(f.author, http.MethodGet, revPath(rev.ID), nil))
	if mine.Review == nil || *mine.Review.Comment != "Préciser l'exercice." {
		t.Fatalf("author does not see the feedback: %+v", mine)
	}
	wantStatus(t, "decided revision leaves review", f.api(f.reviewer, http.MethodGet, reviewPath(rev.ID), nil), http.StatusNotFound, "not_found")
	wantStatus(t, "media of decided revision", f.api(f.reviewer, http.MethodGet, fmt.Sprintf("/api/review/media/%d", img), nil), http.StatusNotFound, "not_found")
	wantStatus(t, "approve after decision", f.api(f.reviewer, http.MethodPost, reviewPath(rev.ID)+"/approve", map[string]any{}),
		http.StatusConflict, "not_in_review")

	// The author corrects in a successor; it is approved.
	w = f.api(f.author, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", rev.LessonID), map[string]any{"content": testDoc("Version corrigée", "fr")})
	wantStatus(t, "successor", w, http.StatusCreated, "")
	next := f.submitRev(f.author, decodeRevision(t, w))
	w = f.api(f.reviewer, http.MethodPost, reviewPath(next.ID)+"/approve", map[string]any{})
	wantStatus(t, "approve", w, http.StatusOK, "")
	got = decodeReviewed(t, w)
	if got.Status != "approved" || got.ApprovedBy == nil || *got.ApprovedBy != f.reviewer.id || got.AuthorID != f.author.id ||
		got.Review == nil || got.Review.Comment != nil {
		t.Fatalf("approved: %s", w.Body)
	}
	mine = decodeReviewed(t, f.api(f.author, http.MethodGet, revPath(next.ID), nil))
	if mine.ApprovedBy == nil || *mine.ApprovedBy != f.reviewer.id {
		t.Fatalf("author does not see the approving reviewer: %+v", mine)
	}

	// Publication.
	w = f.api(f.publisher, http.MethodPost, publishPath(next.ID), map[string]any{})
	wantStatus(t, "publish", w, http.StatusOK, "")
	var pub struct {
		LessonID, PublishedRevisionID, PublishedBy int64
		PreviousRevisionID                         *int64
		PublishedAt                                string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pub); err != nil || pub.LessonID != rev.LessonID || pub.PublishedRevisionID != next.ID ||
		pub.PreviousRevisionID != nil || pub.PublishedBy != f.publisher.id || pub.PublishedAt == "" {
		t.Fatalf("publication: %s", w.Body)
	}
	wantStatus(t, "publish twice", f.api(f.publisher, http.MethodPost, publishPath(next.ID), map[string]any{}), http.StatusConflict, "already_published")
	wantStatus(t, "publish missing", f.api(f.publisher, http.MethodPost, publishPath(next.ID+1000), map[string]any{}), http.StatusNotFound, "not_found")
	wantStatus(t, "publish changes requested", f.api(f.publisher, http.MethodPost, publishPath(rev.ID), map[string]any{}), http.StatusConflict, "not_approved")
	if n := f.count(`SELECT count(*) FROM publication_audit WHERE lesson_id = $1 AND revision_id = $2 AND actor_id = $3`,
		rev.LessonID, next.ID, f.publisher.id); n != 1 {
		t.Fatalf("%d audit rows", n)
	}

	// The lesson accepts a new draft, based on the published revision.
	w = f.api(f.author, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", rev.LessonID), map[string]any{"content": testDoc("Édition suivante", "fr")})
	wantStatus(t, "draft after publication", w, http.StatusCreated, "")
	if d := decodeRevision(t, w); d.BaseRevisionID == nil || *d.BaseRevisionID != next.ID {
		t.Fatalf("base %v", d.BaseRevisionID)
	}
}

// Nobody approves their own revision, administrators included.
func TestReviewSelfApprovalIsForbidden(t *testing.T) {
	f := newReview(t)
	admin := f.newUser("admin-one", f.subject, permissions.Administrator, permissions.Publisher, permissions.Author, permissions.Reviewer)
	rev := f.submittedLesson(admin, "Admin's own lesson")
	before := f.reviewState()
	wantStatus(t, "self-approval", f.api(admin, http.MethodPost, reviewPath(rev.ID)+"/approve", map[string]any{}), http.StatusForbidden, "self_review")
	wantStatus(t, "self change request", f.api(admin, http.MethodPost, reviewPath(rev.ID)+"/request-changes",
		map[string]any{"comment": "Simulated comment."}), http.StatusForbidden, "self_review")
	wantStatus(t, "publish own unapproved", f.api(admin, http.MethodPost, publishPath(rev.ID), map[string]any{}), http.StatusConflict, "not_approved")
	if after := f.reviewState(); after != before {
		t.Fatalf("self-review changed state:\n%s\n%s", before, after)
	}
	// Their own revision is not in their queue.
	w := f.api(admin, http.MethodGet, "/api/review/queue", nil)
	wantStatus(t, "queue", w, http.StatusOK, "")
	if strings.Contains(w.Body.String(), fmt.Sprintf(`"revisionId":%d`, rev.ID)) {
		t.Fatalf("own revision in queue: %s", w.Body)
	}
}

func TestReviewPublishStaleBaseConflicts(t *testing.T) {
	f := newReview(t)
	first := f.submittedLesson(f.author, "First edition")
	wantStatus(t, "approve first", f.api(f.reviewer, http.MethodPost, reviewPath(first.ID)+"/approve", map[string]any{}), http.StatusOK, "")
	wantStatus(t, "publish first", f.api(f.publisher, http.MethodPost, publishPath(first.ID), map[string]any{}), http.StatusOK, "")
	w := f.api(f.author, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", first.LessonID), map[string]any{"content": testDoc("Second edition", "fr")})
	wantStatus(t, "second draft", w, http.StatusCreated, "")
	second := f.submitRev(f.author, decodeRevision(t, w))
	wantStatus(t, "approve second", f.api(f.reviewer, http.MethodPost, reviewPath(second.ID)+"/approve", map[string]any{}), http.StatusOK, "")
	// Unpublication (LRN-009) simulated with direct SQL.
	if _, err := f.db.Exec(context.Background(), `UPDATE lessons SET published_revision_id = NULL WHERE id = $1`, first.LessonID); err != nil {
		t.Fatal(err)
	}
	before := f.reviewState()
	wantStatus(t, "stale publish", f.api(f.publisher, http.MethodPost, publishPath(second.ID), map[string]any{}), http.StatusConflict, "stale_base")
	if after := f.reviewState(); after != before {
		t.Fatalf("stale publish changed state:\n%s\n%s", before, after)
	}
}

func TestReviewStrictJSONAndQuery(t *testing.T) {
	f := newReview(t)
	rev := f.submittedLesson(f.author, "Strict lesson")
	before := f.reviewState()
	approve := reviewPath(rev.ID) + "/approve"
	for name, body := range map[string]string{
		"case-folded key":  `{"Comment":"x"}`,
		"duplicate key":    `{"comment":"x","comment":"y"}`,
		"unknown key":      `{"comment":"x","approve":true}`,
		"wrong type":       `{"comment":1}`,
		"trailing value":   `{"comment":"x"} {}`,
		"array":            `[{"comment":"x"}]`,
		"empty body":       ``,
		"extra close":      `{}}`,
		"trailing garbage": `{} x`,
	} {
		wantStatus(t, name, f.api(f.reviewer, http.MethodPost, approve, []byte(body)), http.StatusBadRequest, "invalid_request")
		wantStatus(t, name+" (request changes)", f.api(f.reviewer, http.MethodPost, reviewPath(rev.ID)+"/request-changes", []byte(body)),
			http.StatusBadRequest, "invalid_request")
	}
	for name, body := range map[string]string{
		"unknown key": `{"baseRevisionId":null}`,
		"array":       `[]`,
		"empty body":  ``,
		"trailing":    `{} {}`,
	} {
		wantStatus(t, "publish "+name, f.api(f.publisher, http.MethodPost, publishPath(rev.ID), []byte(body)), http.StatusBadRequest, "invalid_request")
	}
	r := httptest.NewRequest(http.MethodPost, "http://learnoo.test"+approve, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "text/plain")
	r.RemoteAddr = "192.0.2.10:40000"
	for _, o := range f.reviewer.opts() {
		o(r)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	wantStatus(t, "text/plain body", w, http.StatusUnsupportedMediaType, "unsupported_media_type")
	for _, q := range []string{"?subjectId=abc", "?subjectId=0", "?subjectId=1&subjectId=2", "?other=1", "?subjectId=-1"} {
		wantStatus(t, "queue "+q, f.api(f.reviewer, http.MethodGet, "/api/review/queue"+q, nil), http.StatusBadRequest, "invalid_request")
	}
	for _, p := range []string{"/api/review/revisions/abc", "/api/review/revisions/0", "/api/review/revisions/01"} {
		wantStatus(t, "malformed "+p, f.api(f.reviewer, http.MethodGet, p, nil), http.StatusNotFound, "not_found")
	}
	if after := f.reviewState(); after != before {
		t.Fatalf("rejected bodies changed state:\n%s\n%s", before, after)
	}
	// Whitespace around the object is fine.
	wantStatus(t, "padded", f.api(f.reviewer, http.MethodPost, approve, []byte(" {\"comment\":\"ok\"}\n")), http.StatusOK, "")
}
