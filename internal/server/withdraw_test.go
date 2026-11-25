package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/permissions"
)

// Withdrawal, emergency unpublication and restoration endpoints (LRN-009)
// through the real server handler. Simulated users, subjects and unreviewed
// test documents only.

type withdrawFixture struct {
	reviewFixture
	admin apiUser // administrator only: no publisher, author or reviewer
}

func newWithdraw(t *testing.T) withdrawFixture {
	f := newReview(t)
	return withdrawFixture{reviewFixture: f, admin: f.newUser("admin-one", f.subject, permissions.Administrator)}
}

func withdrawPath(id int64) string { return revPath(id) + "/withdraw" }
func restorePath(id int64) string  { return revPath(id) + "/restore" }
func unpublishPath(lesson int64) string {
	return fmt.Sprintf("/api/lessons/%d/unpublish", lesson)
}

// withdrawState extends reviewState with withdrawals and the audit contents.
func (f withdrawFixture) withdrawState() string {
	f.t.Helper()
	var s string
	err := f.db.QueryRow(context.Background(), `
		SELECT coalesce((SELECT jsonb_agg(p ORDER BY p.id)::text FROM publication_audit p), '') || '|' ||
		       coalesce((SELECT jsonb_agg(w ORDER BY w.id)::text FROM revision_withdrawals w), '') || '|' ||
		       coalesce((SELECT string_agg(id || ':' || coalesce(candidate_closed_at::text, '-'), ',' ORDER BY id) FROM lesson_revisions), '')`).Scan(&s)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.reviewState() + "|" + s
}

// approvedRev approves rev through the API.
func (f withdrawFixture) approvedRev(rev revisionBody) revisionBody {
	f.t.Helper()
	wantStatus(f.t, "approve", f.api(f.reviewer, http.MethodPost, reviewPath(rev.ID)+"/approve", map[string]any{}), http.StatusOK, "")
	return rev
}

func (f withdrawFixture) publishedRev(rev revisionBody) {
	f.t.Helper()
	wantStatus(f.t, "publish", f.api(f.publisher, http.MethodPost, publishPath(rev.ID), map[string]any{}), http.StatusOK, "")
}

func (f withdrawFixture) successorRev(lesson int64, title string) revisionBody {
	f.t.Helper()
	w := f.api(f.author, http.MethodPost, fmt.Sprintf("/api/lessons/%d/drafts", lesson), map[string]any{"content": testDoc(title, "fr")})
	wantStatus(f.t, "draft", w, http.StatusCreated, "")
	return f.submitRev(f.author, decodeRevision(f.t, w))
}

// firstAndSecond publishes a first and a second edition through the API.
func (f withdrawFixture) firstAndSecond() (first, second revisionBody) {
	f.t.Helper()
	first = f.approvedRev(f.submittedLesson(f.author, "First edition"))
	f.publishedRev(first)
	second = f.approvedRev(f.successorRev(first.LessonID, "Second edition"))
	f.publishedRev(second)
	return first, second
}

func withdrawEndpoints(rev, lesson, expected int64) []struct {
	path string
	body any
} {
	return []struct {
		path string
		body any
	}{
		{withdrawPath(rev), map[string]any{"reason": "Simulated reason.", "expectedPublishedRevisionId": expected}},
		{unpublishPath(lesson), map[string]any{"reason": "Simulated reason.", "expectedPublishedRevisionId": expected}},
		{restorePath(rev), map[string]any{"expectedPublishedRevisionId": expected}},
	}
}

func TestWithdrawalAnonymousAndCrossSiteRequestsAreRejected(t *testing.T) {
	f := newWithdraw(t)
	first, second := f.firstAndSecond()
	before := f.withdrawState()
	for _, ep := range withdrawEndpoints(first.ID, first.LessonID, second.ID) {
		wantStatus(t, "anonymous "+ep.path, f.api(apiUser{}, http.MethodPost, ep.path, ep.body, withCSRF("anything")),
			http.StatusUnauthorized, "unauthenticated")
		for label, opts := range map[string][]reqOpt{
			"missing CSRF":      {withCookie(f.admin.cookie)},
			"wrong CSRF":        {withCookie(f.admin.cookie), withCSRF("not-the-token")},
			"other user's CSRF": {withCookie(f.admin.cookie), withCSRF(f.author.csrf)},
		} {
			wantStatus(t, label+" "+ep.path, f.api(apiUser{}, http.MethodPost, ep.path, ep.body, opts...), http.StatusForbidden, "csrf_rejected")
		}
		for label, opt := range map[string]reqOpt{
			"Sec-Fetch-Site": withHeader("Sec-Fetch-Site", "cross-site"),
			"Origin":         withHeader("Origin", "https://evil.example"),
		} {
			wantStatus(t, "cross-origin via "+label+" "+ep.path, f.api(f.admin, http.MethodPost, ep.path, ep.body, opt),
				http.StatusForbidden, "cross_origin_rejected")
		}
	}
	if after := f.withdrawState(); after != before {
		t.Fatalf("rejected requests changed state:\n%s\n%s", before, after)
	}
}

// Withdrawal and unpublication need administrator; restoration needs
// publisher or administrator. Denials come before any lookup, so existing
// and missing IDs look the same.
func TestWithdrawalRoutesRequirePermissions(t *testing.T) {
	f := newWithdraw(t)
	first, second := f.firstAndSecond()
	disabled := f.newUser("disabled-admin", f.subject, permissions.Administrator, permissions.Publisher)
	if _, err := f.db.Exec(context.Background(), `UPDATE users SET disabled_at = now() WHERE id = $1`, disabled.id); err != nil {
		t.Fatal(err)
	}
	before := f.withdrawState()
	for who, u := range map[string]apiUser{
		"student":        f.newUser("student-one", f.subject),
		"author":         f.author,
		"reviewer":       f.reviewer,
		"publisher-only": f.publisher,
	} {
		for _, id := range []int64{second.ID, second.ID + 1000} {
			eps := withdrawEndpoints(id, first.LessonID, second.ID)
			for _, ep := range eps[:2] {
				wantStatus(t, who+" "+ep.path, f.api(u, http.MethodPost, ep.path, ep.body), http.StatusForbidden, "forbidden")
			}
		}
	}
	for who, u := range map[string]apiUser{
		"student":  f.newUser("student-two", f.subject),
		"author":   f.author,
		"reviewer": f.reviewer,
	} {
		for _, id := range []int64{first.ID, first.ID + 1000} {
			wantStatus(t, who+" restore", f.api(u, http.MethodPost, restorePath(id), map[string]any{"expectedPublishedRevisionId": second.ID}),
				http.StatusForbidden, "forbidden")
		}
	}
	// A disabled account's session no longer authenticates.
	for _, ep := range withdrawEndpoints(second.ID, first.LessonID, second.ID) {
		wantStatus(t, "disabled administrator "+ep.path, f.api(disabled, http.MethodPost, ep.path, ep.body), http.StatusUnauthorized, "unauthenticated")
	}
	if after := f.withdrawState(); after != before {
		t.Fatalf("denied requests changed state:\n%s\n%s", before, after)
	}
	// An administrator without publisher may restore; a publisher may too.
	wantStatus(t, "administrator restores", f.api(f.admin, http.MethodPost, restorePath(first.ID),
		map[string]any{"expectedPublishedRevisionId": second.ID}), http.StatusOK, "")
	wantStatus(t, "publisher restores", f.api(f.publisher, http.MethodPost, restorePath(second.ID),
		map[string]any{"expectedPublishedRevisionId": first.ID}), http.StatusOK, "")
	// Revocation applies to the next request. Another active administrator
	// must exist, or the last-administrator guard (LRN-013) refuses it.
	f.newUser("spare-admin", f.subject, permissions.Administrator)
	if _, err := permissions.NewStore(f.db).Revoke(context.Background(), testOperator, f.admin.id, permissions.Administrator); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "revoked administrator", f.api(f.admin, http.MethodPost, unpublishPath(first.LessonID),
		map[string]any{"reason": "x", "expectedPublishedRevisionId": second.ID}), http.StatusForbidden, "forbidden")
}

type pointerChangeBody struct {
	LessonID            int64   `json:"lessonId"`
	Action              string  `json:"action"`
	PublishedRevisionID *int64  `json:"publishedRevisionId"`
	PreviousRevisionID  *int64  `json:"previousRevisionId"`
	ActorID             int64   `json:"actorId"`
	Reason              *string `json:"reason"`
	OccurredAt          string  `json:"occurredAt"`
}

func decodeInto[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(strings.NewReader(w.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return v
}

func TestWithdrawalLifecycle(t *testing.T) {
	f := newWithdraw(t)
	first, second := f.firstAndSecond()
	lesson := first.LessonID

	// Withdrawing the published revision unpublishes it.
	w := f.api(f.admin, http.MethodPost, withdrawPath(second.ID),
		map[string]any{"reason": " Simulated: incorrect solution. ", "expectedPublishedRevisionId": second.ID})
	wantStatus(t, "withdraw published", w, http.StatusOK, "")
	wd := decodeInto[struct {
		RevisionID    int64              `json:"revisionId"`
		LessonID      int64              `json:"lessonId"`
		WithdrawnBy   int64              `json:"withdrawnBy"`
		Reason        string             `json:"reason"`
		WithdrawnAt   string             `json:"withdrawnAt"`
		Unpublication *pointerChangeBody `json:"unpublication"`
	}](t, w)
	if wd.RevisionID != second.ID || wd.LessonID != lesson || wd.WithdrawnBy != f.admin.id || wd.Reason != "Simulated: incorrect solution." ||
		wd.WithdrawnAt == "" || wd.Unpublication == nil || wd.Unpublication.Action != "unpublish" ||
		wd.Unpublication.PublishedRevisionID != nil || *wd.Unpublication.PreviousRevisionID != second.ID ||
		wd.Unpublication.Reason == nil || *wd.Unpublication.Reason != wd.Reason {
		t.Fatalf("withdrawal %s", w.Body)
	}
	before := f.withdrawState()
	wantStatus(t, "publish withdrawn", f.api(f.publisher, http.MethodPost, publishPath(second.ID), map[string]any{}), http.StatusConflict, "withdrawn")
	wantStatus(t, "restore withdrawn", f.api(f.publisher, http.MethodPost, restorePath(second.ID),
		map[string]any{"expectedPublishedRevisionId": nil}), http.StatusConflict, "withdrawn")
	wantStatus(t, "withdraw twice", f.api(f.admin, http.MethodPost, withdrawPath(second.ID),
		map[string]any{"reason": "Again.", "expectedPublishedRevisionId": nil}), http.StatusConflict, "already_withdrawn")
	wantStatus(t, "unpublish an unpublished lesson", f.api(f.admin, http.MethodPost, unpublishPath(lesson),
		map[string]any{"reason": "x", "expectedPublishedRevisionId": second.ID}), http.StatusConflict, "not_published")
	wantStatus(t, "restore with a stale pointer", f.api(f.publisher, http.MethodPost, restorePath(first.ID),
		map[string]any{"expectedPublishedRevisionId": second.ID}), http.StatusConflict, "stale_pointer")
	if after := f.withdrawState(); after != before {
		t.Fatalf("rejected requests changed state:\n%s\n%s", before, after)
	}

	// Restore the first edition, then unpublish it in an emergency.
	w = f.api(f.publisher, http.MethodPost, restorePath(first.ID), map[string]any{"expectedPublishedRevisionId": nil})
	wantStatus(t, "restore", w, http.StatusOK, "")
	rs := decodeInto[pointerChangeBody](t, w)
	if rs.Action != "restore" || rs.PublishedRevisionID == nil || *rs.PublishedRevisionID != first.ID || rs.PreviousRevisionID != nil ||
		rs.ActorID != f.publisher.id || rs.Reason != nil || rs.LessonID != lesson {
		t.Fatalf("restoration %s", w.Body)
	}
	wantStatus(t, "restore the current revision", f.api(f.publisher, http.MethodPost, restorePath(first.ID),
		map[string]any{"expectedPublishedRevisionId": first.ID}), http.StatusConflict, "already_current")
	before = f.withdrawState()
	wantStatus(t, "unpublish with a stale pointer", f.api(f.admin, http.MethodPost, unpublishPath(lesson),
		map[string]any{"reason": "x", "expectedPublishedRevisionId": second.ID}), http.StatusConflict, "stale_pointer")
	wantStatus(t, "unpublish without reason", f.api(f.admin, http.MethodPost, unpublishPath(lesson),
		map[string]any{"expectedPublishedRevisionId": first.ID}), http.StatusUnprocessableEntity, "reason_required")
	wantStatus(t, "unpublish with a blank reason", f.api(f.admin, http.MethodPost, unpublishPath(lesson),
		map[string]any{"reason": " \n", "expectedPublishedRevisionId": first.ID}), http.StatusUnprocessableEntity, "reason_required")
	wantStatus(t, "unpublish with a long reason", f.api(f.admin, http.MethodPost, unpublishPath(lesson),
		map[string]any{"reason": strings.Repeat("x", 2001), "expectedPublishedRevisionId": first.ID}), http.StatusUnprocessableEntity, "invalid_reason")
	wantStatus(t, "unpublish with a NUL reason", f.api(f.admin, http.MethodPost, unpublishPath(lesson),
		[]byte(fmt.Sprintf(`{"reason":"a\u0000b","expectedPublishedRevisionId":%d}`, first.ID))), http.StatusUnprocessableEntity, "invalid_reason")
	wantStatus(t, "unpublish an unknown lesson", f.api(f.admin, http.MethodPost, unpublishPath(lesson+1000),
		map[string]any{"reason": "x", "expectedPublishedRevisionId": first.ID}), http.StatusNotFound, "not_found")
	if after := f.withdrawState(); after != before {
		t.Fatalf("rejected unpublications changed state:\n%s\n%s", before, after)
	}
	w = f.api(f.admin, http.MethodPost, unpublishPath(lesson), map[string]any{"reason": "Simulated: unlawful image.", "expectedPublishedRevisionId": first.ID})
	wantStatus(t, "unpublish", w, http.StatusOK, "")
	up := decodeInto[pointerChangeBody](t, w)
	if up.Action != "unpublish" || up.PublishedRevisionID != nil || *up.PreviousRevisionID != first.ID || up.ActorID != f.admin.id ||
		up.Reason == nil || *up.Reason != "Simulated: unlawful image." {
		t.Fatalf("unpublication %s", w.Body)
	}
	if n := f.count(`SELECT count(*) FROM lessons WHERE id = $1 AND published_revision_id IS NULL`, lesson); n != 1 {
		t.Fatal("lesson still published")
	}
	if n := f.count(`SELECT count(*) FROM publication_audit WHERE lesson_id = $1`, lesson); n != 5 {
		t.Fatalf("%d audit rows, want 5 (publish, publish, unpublish, restore, unpublish)", n)
	}
}

// Withdrawing a pending approved candidate releases the lesson for a new
// draft; restoring a never-published candidate is refused.
func TestWithdrawalReleasesPendingCandidate(t *testing.T) {
	f := newWithdraw(t)
	first := f.approvedRev(f.submittedLesson(f.author, "First edition"))
	f.publishedRev(first)
	pending := f.approvedRev(f.successorRev(first.LessonID, "Pending"))
	draftPath := fmt.Sprintf("/api/lessons/%d/drafts", first.LessonID)
	wantStatus(t, "draft while pending", f.api(f.author, http.MethodPost, draftPath, map[string]any{"content": testDoc("Blocked", "fr")}),
		http.StatusConflict, "active_candidate_exists")
	before := f.withdrawState()
	wantStatus(t, "restore never published", f.api(f.publisher, http.MethodPost, restorePath(pending.ID),
		map[string]any{"expectedPublishedRevisionId": first.ID}), http.StatusConflict, "never_published")
	wantStatus(t, "withdraw with a stale pointer", f.api(f.admin, http.MethodPost, withdrawPath(pending.ID),
		map[string]any{"reason": "x", "expectedPublishedRevisionId": nil}), http.StatusConflict, "stale_pointer")
	wantStatus(t, "withdraw a draft-less unknown revision", f.api(f.admin, http.MethodPost, withdrawPath(pending.ID+1000),
		map[string]any{"reason": "x", "expectedPublishedRevisionId": first.ID}), http.StatusNotFound, "not_found")
	if after := f.withdrawState(); after != before {
		t.Fatalf("rejected requests changed state:\n%s\n%s", before, after)
	}
	w := f.api(f.admin, http.MethodPost, withdrawPath(pending.ID), map[string]any{"reason": "Simulated: incorrect.", "expectedPublishedRevisionId": first.ID})
	wantStatus(t, "withdraw pending", w, http.StatusOK, "")
	if !strings.Contains(w.Body.String(), `"unpublication":null`) {
		t.Fatalf("withdrawal of an unpublished revision: %s", w.Body)
	}
	wantStatus(t, "draft after withdrawal", f.api(f.author, http.MethodPost, draftPath, map[string]any{"content": testDoc("Corrected", "fr")}),
		http.StatusCreated, "")
	wantStatus(t, "publish withdrawn candidate", f.api(f.publisher, http.MethodPost, publishPath(pending.ID), map[string]any{}),
		http.StatusConflict, "withdrawn")
}

func TestWithdrawalStrictJSON(t *testing.T) {
	f := newWithdraw(t)
	first, second := f.firstAndSecond()
	before := f.withdrawState()
	withdraw, unpublish, restore := withdrawPath(second.ID), unpublishPath(first.LessonID), restorePath(first.ID)
	sid := fmt.Sprint(second.ID)
	for name, c := range map[string]struct {
		path, body string
		u          apiUser
	}{
		"withdraw case-folded key":      {withdraw, `{"Reason":"x","expectedPublishedRevisionId":` + sid + `}`, f.admin},
		"withdraw duplicate key":        {withdraw, `{"reason":"x","reason":"y","expectedPublishedRevisionId":` + sid + `}`, f.admin},
		"withdraw unknown key":          {withdraw, `{"reason":"x","expectedPublishedRevisionId":` + sid + `,"force":true}`, f.admin},
		"withdraw missing pointer":      {withdraw, `{"reason":"x"}`, f.admin},
		"withdraw pointer as string":    {withdraw, `{"reason":"x","expectedPublishedRevisionId":"` + sid + `"}`, f.admin},
		"withdraw pointer zero":         {withdraw, `{"reason":"x","expectedPublishedRevisionId":0}`, f.admin},
		"withdraw pointer fraction":     {withdraw, `{"reason":"x","expectedPublishedRevisionId":1.5}`, f.admin},
		"withdraw reason wrong type":    {withdraw, `{"reason":1,"expectedPublishedRevisionId":` + sid + `}`, f.admin},
		"withdraw trailing":             {withdraw, `{"reason":"x","expectedPublishedRevisionId":` + sid + `} {}`, f.admin},
		"withdraw array":                {withdraw, `[]`, f.admin},
		"unpublish null pointer":        {unpublish, `{"reason":"x","expectedPublishedRevisionId":null}`, f.admin},
		"unpublish missing pointer":     {unpublish, `{"reason":"x"}`, f.admin},
		"unpublish negative pointer":    {unpublish, `{"reason":"x","expectedPublishedRevisionId":-1}`, f.admin},
		"unpublish overflowing pointer": {unpublish, `{"reason":"x","expectedPublishedRevisionId":99999999999999999999}`, f.admin},
		"restore empty object":          {restore, `{}`, f.publisher},
		"restore reason key":            {restore, `{"reason":"x","expectedPublishedRevisionId":` + sid + `}`, f.publisher},
		"restore empty body":            {restore, ``, f.publisher},
		"restore garbage":               {restore, `{"expectedPublishedRevisionId":` + sid + `} x`, f.publisher},
	} {
		wantStatus(t, name, f.api(c.u, http.MethodPost, c.path, []byte(c.body)), http.StatusBadRequest, "invalid_request")
	}
	for _, p := range []string{"/api/revisions/abc/withdraw", "/api/revisions/0/restore", "/api/lessons/01/unpublish"} {
		u := f.admin
		wantStatus(t, "malformed "+p, f.api(u, http.MethodPost, p, map[string]any{"reason": "x", "expectedPublishedRevisionId": second.ID}),
			http.StatusNotFound, "not_found")
	}
	r := httptest.NewRequest(http.MethodPost, "http://learnoo.test"+withdraw, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "text/plain")
	r.RemoteAddr = "192.0.2.10:40000"
	for _, o := range f.admin.opts() {
		o(r)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	wantStatus(t, "text/plain body", w, http.StatusUnsupportedMediaType, "unsupported_media_type")
	if after := f.withdrawState(); after != before {
		t.Fatalf("rejected bodies changed state:\n%s\n%s", before, after)
	}
}
