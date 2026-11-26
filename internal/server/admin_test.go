package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// The administration API (LMS-B10): simulated TEST accounts, subjects and
// names only.

type adminUserResp struct {
	ID               int64    `json:"id"`
	Email            string   `json:"email"`
	Disabled         bool     `json:"disabled"`
	Permissions      []string `json:"permissions"`
	ReviewerSubjects []struct {
		ID   int64  `json:"id"`
		Code string `json:"code"`
	} `json:"reviewerSubjects"`
}

type userChangeResp struct {
	User    adminUserResp `json:"user"`
	Changed bool          `json:"changed"`
}

// relogin signs an existing simulated account in again.
func (e *env) relogin(name string, id int64) apiUser {
	e.t.Helper()
	addr := fmt.Sprintf("198.51.100.%d", addrCounter.Add(1)%250+1)
	w := e.login(name+"@example.test", testPassword, withAddr(addr))
	if w.Code != http.StatusOK {
		e.t.Fatalf("login %s: %d %s", name, w.Code, w.Body)
	}
	var b sessionBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		e.t.Fatal(err)
	}
	return apiUser{id: id, cookie: sessionCookie(e.t, w).Value, csrf: b.CSRFToken}
}

func (e *env) activeAdmins() int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM user_permissions p JOIN users u ON u.id = p.user_id
		WHERE p.permission = 'administrator' AND u.disabled_at IS NULL`)
}

func userPath(id int64, action string) string {
	if action == "" {
		return fmt.Sprintf("/api/admin/users/%d", id)
	}
	return fmt.Sprintf("/api/admin/users/%d/%s", id, action)
}

// Every administration route needs a session, then administrator (403
// before any lookup or body is read), and unsafe methods the CSRF token.
func TestAdminAPIRequiresAdministrator(t *testing.T) {
	e := newEnv(t)
	math := e.newSubject("test-math")
	e.chapter(math, "ch-01")
	admin := e.newUser("adm-admin", 0, permissions.Administrator)
	target := e.newUser("adm-target", 0)
	others := map[string]apiUser{
		"student":   e.newUser("adm-student", 0),
		"author":    e.newUser("adm-author", 0, permissions.Author),
		"publisher": e.newUser("adm-publisher", 0, permissions.Publisher),
		"reviewer":  e.newUser("adm-reviewer", math, permissions.Reviewer),
	}
	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/admin/users", nil},
		{http.MethodGet, userPath(target.id, ""), nil},
		{http.MethodGet, userPath(999999, ""), nil},
		{http.MethodPost, userPath(target.id, "grant"), map[string]any{"permission": "author"}},
		{http.MethodPost, userPath(999999, "grant"), []byte(`{"bogus":`)},
		{http.MethodPost, userPath(target.id, "revoke"), map[string]any{"permission": "author"}},
		{http.MethodPost, userPath(target.id, "disable"), map[string]any{}},
		{http.MethodPost, userPath(target.id, "enable"), map[string]any{}},
		{http.MethodPost, "/api/admin/invitations", map[string]any{"email": "x@example.test"}},
		{http.MethodGet, "/api/admin/catalogue", nil},
		{http.MethodPut, "/api/admin/catalogue/subjects/test-math/names", map[string]any{"nameFr": "TEST x"}},
		{http.MethodPut, "/api/admin/catalogue/subjects/test-nope/order", map[string]any{"sortOrder": 1}},
		{http.MethodPut, "/api/admin/catalogue/subjects/test-math/chapters/ch-01/names", map[string]any{"nameFr": "TEST x"}},
		{http.MethodPut, "/api/admin/catalogue/subjects/test-math/chapters/ch-01/order", map[string]any{"sortOrder": 1}},
	}
	before := e.count(`SELECT (SELECT count(*) FROM permission_audit) + (SELECT count(*) FROM account_status_audit)
		+ (SELECT count(*) FROM catalogue_audit) + (SELECT count(*) FROM student_invitations)`)
	for _, rt := range routes {
		wantStatus(t, "anonymous "+rt.method+" "+rt.path, e.api(apiUser{}, rt.method, rt.path, rt.body), http.StatusUnauthorized, "unauthenticated")
		for name, u := range others {
			wantStatus(t, name+" "+rt.method+" "+rt.path, e.api(u, rt.method, rt.path, rt.body), http.StatusForbidden, "forbidden")
		}
		if rt.method != http.MethodGet {
			wantStatus(t, "no CSRF "+rt.path, e.api(admin, rt.method, rt.path, rt.body, withCSRF("")), http.StatusForbidden, "csrf_rejected")
			wantStatus(t, "cross-origin "+rt.path, e.api(admin, rt.method, rt.path, rt.body, withHeader("Sec-Fetch-Site", "cross-site")),
				http.StatusForbidden, "cross_origin_rejected")
		}
	}
	after := e.count(`SELECT (SELECT count(*) FROM permission_audit) + (SELECT count(*) FROM account_status_audit)
		+ (SELECT count(*) FROM catalogue_audit) + (SELECT count(*) FROM student_invitations)`)
	if after != before {
		t.Fatalf("refused requests wrote %d audit or invitation rows", after-before)
	}
	// Administrators get the routes; a missing user is 404 for them only.
	wantStatus(t, "admin list", e.api(admin, http.MethodGet, "/api/admin/users", nil), http.StatusOK, "")
	wantStatus(t, "admin missing user", e.api(admin, http.MethodGet, userPath(999999, ""), nil), http.StatusNotFound, "not_found")
	wantStatus(t, "admin malformed user", e.api(admin, http.MethodGet, "/api/admin/users/01", nil), http.StatusNotFound, "not_found")
	if cc := e.api(admin, http.MethodGet, "/api/admin/catalogue", nil).Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
}

func TestAdminGrantsAndStatusThroughTheStores(t *testing.T) {
	e := newEnv(t)
	math := e.newSubject("test-math")
	retired := e.newSubject("test-old")
	admin := e.newUser("gr-admin", 0, permissions.Administrator, permissions.Author)
	teacher := e.newUser("gr-teacher", 0)
	ctx := context.Background()

	grant := func(u apiUser, target int64, body any) *httptest.ResponseRecorder {
		return e.api(u, http.MethodPost, userPath(target, "grant"), body)
	}
	revoke := func(u apiUser, target int64, body any) *httptest.ResponseRecorder {
		return e.api(u, http.MethodPost, userPath(target, "revoke"), body)
	}

	// Strict JSON and request shape.
	for name, body := range map[string]any{
		"unknown key":         []byte(`{"permission":"author","x":1}`),
		"wrong case":          []byte(`{"Permission":"author"}`),
		"duplicate":           []byte(`{"permission":"author","permission":"author"}`),
		"null":                []byte(`{"permission":null}`),
		"trailing":            []byte(`{"permission":"author"} {}`),
		"missing":             []byte(`{}`),
		"unknown permission":  map[string]any{"permission": "owner"},
		"subject with global": map[string]any{"permission": "author", "subjectId": math},
		"reviewer no subject": map[string]any{"permission": "reviewer"},
		"subject as string":   []byte(`{"permission":"reviewer","subjectId":"1"}`),
	} {
		wantStatus(t, name, grant(admin, teacher.id, body), http.StatusBadRequest, "invalid_request")
	}
	w := e.api(admin, http.MethodPost, userPath(teacher.id, "grant"), []byte(`{"permission":"author"}`), withHeader("Content-Type", "text/plain"))
	wantStatus(t, "content type", w, http.StatusUnsupportedMediaType, "unsupported_media_type")

	// Grant author: audited with the administrator as actor; a repeat is a no-op.
	w = grant(admin, teacher.id, map[string]any{"permission": "author"})
	wantStatus(t, "grant author", w, http.StatusOK, "")
	if r := decodeBody[userChangeResp](t, w); !r.Changed || strings.Join(r.User.Permissions, ",") != "author" {
		t.Fatalf("grant author: %+v", r)
	}
	if r := decodeBody[userChangeResp](t, grant(admin, teacher.id, map[string]any{"permission": "author"})); r.Changed {
		t.Fatal("repeated grant changed something")
	}
	if n := e.count(`SELECT count(*) FROM permission_audit WHERE target_user_id = $1 AND actor_id = $2 AND operator_label IS NULL
		AND action = 'grant' AND permission = 'author'`, teacher.id, admin.id); n != 1 {
		t.Fatalf("author grant audit rows %d", n)
	}

	// Reviewer per subject: the teacher sees the review queue only after it.
	wantStatus(t, "queue before", e.api(e.relogin("gr-teacher", teacher.id), http.MethodGet, "/api/review/queue", nil), http.StatusForbidden, "forbidden")
	w = grant(admin, teacher.id, map[string]any{"permission": "reviewer", "subjectId": math})
	wantStatus(t, "grant reviewer", w, http.StatusOK, "")
	if r := decodeBody[userChangeResp](t, w); len(r.User.ReviewerSubjects) != 1 || r.User.ReviewerSubjects[0].Code != "test-math" {
		t.Fatalf("reviewer subjects %+v", r.User.ReviewerSubjects)
	}
	wantStatus(t, "queue after", e.api(e.relogin("gr-teacher", teacher.id), http.MethodGet, "/api/review/queue", nil), http.StatusOK, "")
	wantStatus(t, "unknown subject", grant(admin, teacher.id, map[string]any{"permission": "reviewer", "subjectId": 999999}), http.StatusUnprocessableEntity, "unknown_subject")
	wantStatus(t, "zero subject", grant(admin, teacher.id, map[string]any{"permission": "reviewer", "subjectId": 0}), http.StatusUnprocessableEntity, "unknown_subject")
	w = e.api(admin, http.MethodPost, fmt.Sprintf("/api/admin/curriculum/subjects/%d/retire", retired), map[string]any{})
	wantStatus(t, "retire subject", w, http.StatusNoContent, "")
	wantStatus(t, "retired subject", grant(admin, teacher.id, map[string]any{"permission": "reviewer", "subjectId": retired}), http.StatusUnprocessableEntity, "retired_subject")
	wantStatus(t, "unknown user", grant(admin, 999999, map[string]any{"permission": "author"}), http.StatusNotFound, "not_found")
	wantStatus(t, "unknown user revoke", revoke(admin, 999999, map[string]any{"permission": "author"}), http.StatusNotFound, "not_found")
	w = revoke(admin, teacher.id, map[string]any{"permission": "reviewer", "subjectId": math})
	wantStatus(t, "revoke reviewer", w, http.StatusOK, "")
	if r := decodeBody[userChangeResp](t, w); !r.Changed || len(r.User.ReviewerSubjects) != 0 {
		t.Fatalf("revoke reviewer: %+v", r)
	}

	// Administrators are not implicit reviewers.
	wantStatus(t, "admin queue", e.api(admin, http.MethodGet, "/api/review/queue", nil), http.StatusForbidden, "forbidden")

	// No self-grant, whatever the permission; self-revocation is allowed
	// except for the last active administrator.
	for _, body := range []map[string]any{{"permission": "publisher"}, {"permission": "author"}, {"permission": "administrator"}, {"permission": "reviewer", "subjectId": math}} {
		wantStatus(t, fmt.Sprint("self grant ", body), grant(admin, admin.id, body), http.StatusForbidden, "self_change")
	}
	if n := e.count(`SELECT count(*) FROM reviewer_assignments WHERE user_id = $1`, admin.id); n != 0 {
		t.Fatal("self-grant stored a reviewer assignment")
	}
	wantStatus(t, "self disable", e.api(admin, http.MethodPost, userPath(admin.id, "disable"), map[string]any{}), http.StatusForbidden, "self_change")
	wantStatus(t, "last admin self revoke", revoke(admin, admin.id, map[string]any{"permission": "administrator"}), http.StatusConflict, "last_administrator")
	w = revoke(admin, admin.id, map[string]any{"permission": "author"})
	wantStatus(t, "self revoke author", w, http.StatusOK, "")

	// Disable: the target's sessions end at once; audited with the actor.
	session := e.relogin("gr-teacher", teacher.id)
	wantStatus(t, "session before", e.api(session, http.MethodGet, "/api/lessons/mine", nil), http.StatusOK, "")
	w = e.api(admin, http.MethodPost, userPath(teacher.id, "disable"), map[string]any{})
	wantStatus(t, "disable", w, http.StatusOK, "")
	if r := decodeBody[userChangeResp](t, w); !r.Changed || !r.User.Disabled || strings.Join(r.User.Permissions, ",") != "author" {
		t.Fatalf("disable: %+v", r)
	}
	wantStatus(t, "session after", e.api(session, http.MethodGet, "/api/lessons/mine", nil), http.StatusUnauthorized, "unauthenticated")
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = $1`, teacher.id); n != 0 {
		t.Fatalf("%d sessions survive the disable", n)
	}
	if w := e.login("gr-teacher@example.test", testPassword, withAddr("198.51.100.251")); w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled login %d", w.Code)
	}
	wantStatus(t, "disable body", e.api(admin, http.MethodPost, userPath(teacher.id, "disable"), []byte(`{"reason":"x"}`)), http.StatusBadRequest, "invalid_request")
	if r := decodeBody[userChangeResp](t, e.api(admin, http.MethodPost, userPath(teacher.id, "disable"), map[string]any{})); r.Changed {
		t.Fatal("repeated disable changed something")
	}
	w = e.api(admin, http.MethodPost, userPath(teacher.id, "enable"), map[string]any{})
	wantStatus(t, "enable", w, http.StatusOK, "")
	if r := decodeBody[userChangeResp](t, w); !r.Changed || r.User.Disabled {
		t.Fatalf("enable: %+v", r)
	}
	if n := e.count(`SELECT count(*) FROM account_status_audit WHERE target_user_id = $1 AND actor_id = $2 AND operator_label IS NULL`, teacher.id, admin.id); n != 2 {
		t.Fatalf("status audit rows with the actor: %d", n)
	}

	// The history lists both audits, newest first, with the actor.
	w = e.api(admin, http.MethodGet, userPath(teacher.id, ""), nil)
	wantStatus(t, "user", w, http.StatusOK, "")
	detail := decodeBody[struct {
		History []struct {
			Kind, Action string
			ActorID      *int64 `json:"actorId"`
			ActorName    *string
			Operator     *string `json:"operatorLabel"`
		} `json:"history"`
	}](t, w)
	if len(detail.History) != 5 || detail.History[0].Action != "enable" || detail.History[0].Kind != "status" {
		t.Fatalf("history %+v", detail.History)
	}
	for _, h := range detail.History {
		if h.ActorID == nil || *h.ActorID != admin.id || h.Operator != nil {
			t.Errorf("event %+v lacks the administrator as actor", h)
		}
	}
	wantStatus(t, "user query", e.api(admin, http.MethodGet, userPath(teacher.id, "")+"?x=1", nil), http.StatusBadRequest, "invalid_request")

	// Disabling the last active administrator is refused (here by a second
	// administrator who is then removed from the count by revocation).
	second := e.newUser("gr-admin-two", 0, permissions.Administrator)
	wantStatus(t, "revoke first", revoke(second, admin.id, map[string]any{"permission": "administrator"}), http.StatusOK, "")
	wantStatus(t, "revoked admin denied", grant(admin, teacher.id, map[string]any{"permission": "publisher"}), http.StatusForbidden, "forbidden")
	if _, err := permissions.NewStore(e.db).Grant(ctx, testOperator, admin.id, permissions.Administrator); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "disable other admin", e.api(admin, http.MethodPost, userPath(second.id, "disable"), map[string]any{}), http.StatusOK, "")
	if e.activeAdmins() != 1 {
		t.Fatalf("active administrators %d", e.activeAdmins())
	}
}

// Two administrators acting on each other at the same time over HTTP: the
// administrators lock serializes them, the second finds its own
// administrator gone (403), and one active administrator always remains.
func TestAdminConcurrentRemovalsKeepAnAdministrator(t *testing.T) {
	e := newEnv(t)
	dbtest.Warm(t, e.db, 4)
	a := e.newUser("race-a", 0, permissions.Administrator)
	b := e.newUser("race-b", 0, permissions.Administrator)
	ps := permissions.NewStore(e.db)
	ctx := context.Background()
	reset := func() {
		t.Helper()
		if _, err := e.db.Exec(ctx, `UPDATE users SET disabled_at = NULL WHERE id IN ($1, $2)`, a.id, b.id); err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{a.id, b.id} {
			if _, err := ps.Grant(ctx, testOperator, id, permissions.Administrator); err != nil {
				t.Fatal(err)
			}
		}
	}
	race := func(name string, x, y func() *httptest.ResponseRecorder, loserCodes ...string) {
		t.Helper()
		var wg sync.WaitGroup
		var wx, wy *httptest.ResponseRecorder
		wg.Add(2)
		go func() { defer wg.Done(); wx = x() }()
		go func() { defer wg.Done(); wy = y() }()
		wg.Wait()
		ok := 0
		for _, w := range []*httptest.ResponseRecorder{wx, wy} {
			if w.Code == http.StatusOK {
				ok++
				continue
			}
			var b struct{ Error struct{ Code string } }
			_ = json.Unmarshal(w.Body.Bytes(), &b)
			allowed := false
			for _, c := range loserCodes {
				allowed = allowed || b.Error.Code == c
			}
			if !allowed {
				t.Fatalf("%s: unexpected %d %s", name, w.Code, w.Body)
			}
		}
		if ok != 1 {
			t.Fatalf("%s: %d requests succeeded, want exactly 1 (%d %s / %d %s)", name, ok, wx.Code, wx.Body, wy.Code, wy.Body)
		}
		if n := e.activeAdmins(); n != 1 {
			t.Fatalf("%s: %d active administrators, want 1", name, n)
		}
	}
	revokeOther := func(u apiUser, target int64) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			return e.api(u, http.MethodPost, userPath(target, "revoke"), map[string]any{"permission": "administrator"})
		}
	}
	disableOther := func(u apiUser, target int64) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			return e.api(u, http.MethodPost, userPath(target, "disable"), map[string]any{})
		}
	}
	const iterations = 15
	for i := range iterations {
		reset()
		race(fmt.Sprintf("revoke each other #%d", i), revokeOther(a, b.id), revokeOther(b, a.id), "forbidden", "retry")
	}
	for i := range iterations {
		reset()
		// Both revoke themselves: the store's last-administrator guard
		// refuses the second.
		race(fmt.Sprintf("revoke themselves #%d", i), revokeOther(a, a.id), revokeOther(b, b.id), "last_administrator", "retry")
	}
	for i := range iterations {
		reset()
		// Disabling deletes sessions: sign in afresh each round.
		a, b = e.relogin("race-a", a.id), e.relogin("race-b", b.id)
		race(fmt.Sprintf("disable each other #%d", i), disableOther(a, b.id), disableOther(b, a.id), "forbidden", "retry", "unauthenticated")
	}
	for i := range iterations {
		reset()
		a, b = e.relogin("race-a", a.id), e.relogin("race-b", b.id)
		race(fmt.Sprintf("revoke and disable #%d", i), revokeOther(a, b.id), disableOther(b, a.id), "forbidden", "retry", "unauthenticated")
	}
}

func TestAdminUserDirectory(t *testing.T) {
	e := newEnv(t)
	math := e.newSubject("test-math")
	admin := e.newUser("dir-admin", 0, permissions.Administrator)
	teacher := e.newUser("dir-teacher", 0, permissions.Author)
	e.newUser("dir-reviewer", math, permissions.Reviewer)
	for i := range 52 {
		e.createUser(fmt.Sprintf("dir-pupil-%02d@example.test", i), testPassword)
	}
	if _, err := e.db.Exec(context.Background(), `UPDATE users SET disabled_at = now() WHERE email = 'dir-pupil-07@example.test'`); err != nil {
		t.Fatal(err)
	}
	type page struct {
		Users     []adminUserResp `json:"users"`
		NextAfter *int64          `json:"nextAfter"`
	}
	list := func(query string) page {
		t.Helper()
		w := e.api(admin, http.MethodGet, "/api/admin/users"+query, nil)
		wantStatus(t, "list "+query, w, http.StatusOK, "")
		return decodeBody[page](t, w)
	}
	first := list("")
	if len(first.Users) != 50 || first.NextAfter == nil || *first.NextAfter != first.Users[49].ID {
		t.Fatalf("first page: %d users, next %v", len(first.Users), first.NextAfter)
	}
	second := list(fmt.Sprintf("?after=%d", *first.NextAfter))
	if len(second.Users) != 56-50 || second.NextAfter != nil || second.Users[0].ID <= first.Users[49].ID {
		t.Fatalf("second page: %d users, next %v", len(second.Users), second.NextAfter)
	}
	if got := list("?permission=author"); len(got.Users) != 1 || got.Users[0].ID != teacher.id {
		t.Fatalf("teachers: %+v", got.Users)
	}
	if got := list("?permission=reviewer"); len(got.Users) != 1 || got.Users[0].ReviewerSubjects[0].Code != "test-math" {
		t.Fatalf("reviewers: %+v", got.Users)
	}
	if got := list("?q=PUPIL-0&status=disabled"); len(got.Users) != 1 || !got.Users[0].Disabled {
		t.Fatalf("search disabled: %+v", got.Users)
	}
	if got := list("?permission=none&q=dir-"); len(got.Users) != 50 {
		t.Fatalf("students: %d", len(got.Users))
	}
	if got := list("?q=%25"); len(got.Users) != 0 {
		t.Fatalf("a %% search matched %d accounts (no wildcard)", len(got.Users))
	}
	for _, q := range []string{"?q=", "?permission=owner", "?status=gone", "?after=0", "?after=x", "?q=a&q=b", "?page=2", "?q=" + strings.Repeat("a", 101)} {
		wantStatus(t, "bad "+q, e.api(admin, http.MethodGet, "/api/admin/users"+q, nil), http.StatusBadRequest, "invalid_request")
	}
}

func TestAdminInvitesStudents(t *testing.T) {
	e := newEnv(t)
	admin := e.newUser("inv-admin", 0, permissions.Administrator)
	w := e.api(admin, http.MethodPost, "/api/admin/invitations", map[string]any{"email": " New.Pupil@Example.TEST ", "ttlHours": 2})
	wantStatus(t, "invite", w, http.StatusCreated, "")
	resp := decodeBody[struct {
		Invitation struct {
			ID    int64
			Email string
		}
		Path string
	}](t, w)
	if resp.Invitation.Email != "new.pupil@example.test" || !strings.HasPrefix(resp.Path, "/invite/") || len(resp.Path) != len("/invite/")+43 {
		t.Fatalf("invitation %+v", resp)
	}
	if n := e.count(`SELECT count(*) FROM student_invitation_audit WHERE invitation_id = $1 AND action = 'issue' AND actor_id = $2 AND operator_label IS NULL`,
		resp.Invitation.ID, admin.id); n != 1 {
		t.Fatalf("issue audit rows %d", n)
	}
	if w := e.do(http.MethodGet, resp.Path, nil); w.Code != http.StatusOK {
		t.Fatalf("invitation page %d", w.Code)
	}
	wantStatus(t, "existing", e.api(admin, http.MethodPost, "/api/admin/invitations", map[string]any{"email": testEmail}), http.StatusConflict, "account_exists")
	wantStatus(t, "bad email", e.api(admin, http.MethodPost, "/api/admin/invitations", map[string]any{"email": "nope"}), http.StatusUnprocessableEntity, "invalid_input")
	wantStatus(t, "ttl 0", e.api(admin, http.MethodPost, "/api/admin/invitations", map[string]any{"email": "a@example.test", "ttlHours": 0}), http.StatusUnprocessableEntity, "invalid_input")
	wantStatus(t, "ttl 721", e.api(admin, http.MethodPost, "/api/admin/invitations", map[string]any{"email": "a@example.test", "ttlHours": 721}), http.StatusUnprocessableEntity, "invalid_input")
	wantStatus(t, "no email", e.api(admin, http.MethodPost, "/api/admin/invitations", map[string]any{}), http.StatusBadRequest, "invalid_request")
}

func TestAdminCatalogueNamesAndOrder(t *testing.T) {
	e := newEnv(t)
	math := e.newSubject("test-math")
	e.chapter(math, "ch-01")
	e.newSubject("test-empty")
	admin := e.newUser("cat-admin", 0, permissions.Administrator)
	w := e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-math/names", map[string]any{"nameFr": " TEST Mathématiques ", "nameAr": "TEST رياضيات"})
	wantStatus(t, "names", w, http.StatusOK, "")
	got := decodeBody[struct {
		Subject struct{ NameFr, NameAr *string }
		Changed bool
	}](t, w)
	if !got.Changed || *got.Subject.NameFr != "TEST Mathématiques" {
		t.Fatalf("names %+v", got)
	}
	wantStatus(t, "chapter order", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-math/chapters/ch-01/order", map[string]any{"sortOrder": 4}), http.StatusOK, "")
	wantStatus(t, "subject order", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-math/order", map[string]any{"sortOrder": 9}), http.StatusOK, "")
	wantStatus(t, "chapter names", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-math/chapters/ch-01/names", map[string]any{"nameAr": "TEST فصل"}), http.StatusOK, "")
	if n := e.count(`SELECT count(*) FROM catalogue_audit WHERE actor_id = $1 AND operator_label IS NULL`, admin.id); n != 4 {
		t.Fatalf("catalogue audit rows with the administrator: %d", n)
	}
	w = e.api(admin, http.MethodGet, "/api/admin/catalogue", nil)
	wantStatus(t, "tree", w, http.StatusOK, "")
	tree := decodeBody[struct {
		Subjects []struct {
			Code      string
			SortOrder int
			Chapters  []struct {
				Code      string
				SortOrder int
				NameAr    *string
			}
		}
	}](t, w)
	if len(tree.Subjects) != 2 || tree.Subjects[1].Code != "test-math" || tree.Subjects[1].Chapters[0].SortOrder != 4 || tree.Subjects[0].Chapters == nil {
		t.Fatalf("tree %+v", tree)
	}
	wantStatus(t, "bad name", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-math/names", map[string]any{"nameFr": "a‮b"}), http.StatusUnprocessableEntity, "invalid_input")
	wantStatus(t, "empty name", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-math/names", map[string]any{"nameFr": "  "}), http.StatusUnprocessableEntity, "invalid_input")
	wantStatus(t, "no names", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-math/names", map[string]any{}), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "null name", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-math/names", []byte(`{"nameFr":null}`)), http.StatusBadRequest, "invalid_request")
	wantStatus(t, "bad order", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-math/order", map[string]any{"sortOrder": 10000}), http.StatusUnprocessableEntity, "invalid_input")
	wantStatus(t, "unknown subject", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-nope/order", map[string]any{"sortOrder": 1}), http.StatusNotFound, "not_found")
	wantStatus(t, "malformed code", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/Test/order", map[string]any{"sortOrder": 1}), http.StatusNotFound, "not_found")
	wantStatus(t, "unknown chapter", e.api(admin, http.MethodPut, "/api/admin/catalogue/subjects/test-math/chapters/ch-09/order", map[string]any{"sortOrder": 1}), http.StatusNotFound, "not_found")
}

// Invitation issuing is budgeted per administrator, like the other
// invitation and report throttles: past the limit the request is 429 with
// Retry-After and writes nothing; another administrator is unaffected.
func TestAdminInvitationBudget(t *testing.T) {
	e := newEnvConfig(t, func(c *auth.Config) { c.AdminInvitationLimit = 2 })
	first := e.newUser("budget-admin", 0, permissions.Administrator)
	second := e.newUser("budget-admin-two", 0, permissions.Administrator)
	invite := func(u apiUser, email string) *httptest.ResponseRecorder {
		return e.api(u, http.MethodPost, "/api/admin/invitations", map[string]any{"email": email})
	}
	wantStatus(t, "first", invite(first, "b1@example.test"), http.StatusCreated, "")
	// A refused issue (existing account) still counts.
	wantStatus(t, "existing", invite(first, testEmail), http.StatusConflict, "account_exists")
	w := invite(first, "b3@example.test")
	wantStatus(t, "over budget", w, http.StatusTooManyRequests, "rate_limited")
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After %q", ra)
	}
	if n := e.count(`SELECT count(*) FROM student_invitations WHERE email = 'b3@example.test'`); n != 0 {
		t.Fatalf("throttled request stored %d invitations", n)
	}
	wantStatus(t, "other administrator", invite(second, "b4@example.test"), http.StatusCreated, "")
	e.clock.Advance(16 * time.Minute)
	wantStatus(t, "after the window", invite(first, "b3@example.test"), http.StatusCreated, "")
}
