package review_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/review"
	"github.com/hadden-me/learnoo/internal/server"
)

// Through the real server handler: a revocation committed after the
// middleware check but before the in-transaction recheck denies the write
// (404 for a reviewer, as for a revision they cannot see; 403 for a
// publisher) and records nothing.
func TestHTTPRevocationDuringRequest(t *testing.T) {
	e := newEnv(t)
	cfg := auth.DefaultConfig
	cfg.Argon2 = testArgon2
	svc, err := auth.New(e.db, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := media.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()
	h := server.New(e.db, svc, ms, slog.New(slog.NewTextHandler(io.Discard, nil)))

	type session struct {
		cookie *http.Cookie
		csrf   string
	}
	login := func(name string) session {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "http://learnoo.test/api/auth/login",
			strings.NewReader(`{"email":"`+name+`@example.test","password":"simulated-password-1"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var b struct{ CSRFToken string }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &b) != nil {
			t.Fatalf("login %s: %d %s", name, w.Code, w.Body)
		}
		return session{cookie: w.Result().Cookies()[0], csrf: b.CSRFToken}
	}
	post := func(s session, path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "http://learnoo.test"+path, strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(auth.CSRFHeader, s.csrf)
		r.AddCookie(s.cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	code := func(w *httptest.ResponseRecorder) string {
		var b struct{ Error struct{ Code string } }
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		return b.Error.Code
	}

	reviewer := e.reviewer("reviewer", []int64{e.subject})
	publisher := e.user("publisher", permissions.Publisher)
	rev := e.submitted(e.author, e.subject, "Revoked over HTTP")
	before := e.state(rev)

	restore := review.SetTestHooks(func() {
		if _, err := e.perms.RevokeReviewer(e.ctx, testOperator, reviewer, e.subject); err != nil {
			t.Error(err)
		}
	}, nil)
	w := post(login("reviewer"), fmt.Sprintf("/api/review/revisions/%d/approve", rev.ID))
	restore()
	if w.Code != http.StatusNotFound || code(w) != "not_found" {
		t.Fatalf("approve during revocation: %d %s", w.Code, w.Body)
	}
	if after := e.state(rev); after != before {
		t.Fatalf("revoked approval changed state: %s -> %s", before, after)
	}

	other := e.reviewer("other", []int64{e.subject})
	if _, err := e.review.Approve(e.ctx, other, rev.ID, ""); err != nil {
		t.Fatal(err)
	}
	before = e.state(rev)
	restore = review.SetTestHooks(func() {
		if _, err := e.perms.Revoke(e.ctx, testOperator, publisher, permissions.Publisher); err != nil {
			t.Error(err)
		}
	}, nil)
	w = post(login("publisher"), fmt.Sprintf("/api/revisions/%d/publish", rev.ID))
	restore()
	if w.Code != http.StatusForbidden || code(w) != "forbidden" {
		t.Fatalf("publish during revocation: %d %s", w.Code, w.Body)
	}
	if after := e.state(rev); after != before {
		t.Fatalf("revoked publication changed state: %s -> %s", before, after)
	}
}
