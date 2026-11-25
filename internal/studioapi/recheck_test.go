package studioapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/server"
	"github.com/hadden-me/learnoo/internal/studioapi"
	"github.com/hadden-me/learnoo/internal/users"
)

// testOperator is the local operator actor the tests grant and revoke with.
var testOperator = func() permissions.Actor {
	a, err := permissions.OperatorAs("test")
	if err != nil {
		panic(err)
	}
	return a
}()

// The upload path rechecks the author permission inside its transaction:
// a revocation after the middleware check but before the row is written
// denies the upload and records nothing. Simulated user and image only.
func TestUploadRechecksAuthorInTransaction(t *testing.T) {
	ctx := context.Background()
	db := dbtest.Migrated(t)
	cfg := auth.DefaultConfig
	cfg.Argon2 = auth.Argon2Params{MemoryKiB: 1024, Iterations: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}
	svc, err := auth.New(db, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := media.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()
	h := server.New(db, svc, ms, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const password = "simulated-password-1"
	hash, err := auth.HashPassword(password, cfg.Argon2)
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.Create(ctx, db, "author@example.test", "Simulated Author", hash)
	if err != nil {
		t.Fatal(err)
	}
	perms := permissions.NewStore(db)
	if _, err := perms.Grant(ctx, testOperator, u.ID, permissions.Author); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "http://learnoo.test/api/auth/login",
		strings.NewReader(`{"email":"author@example.test","password":"`+password+`"}`))
	login.Header.Set("Content-Type", "application/json")
	lw := httptest.NewRecorder()
	h.ServeHTTP(lw, login)
	var sess struct{ CSRFToken string }
	if lw.Code != http.StatusOK || json.Unmarshal(lw.Body.Bytes(), &sess) != nil {
		t.Fatalf("login: %d %s", lw.Code, lw.Body)
	}
	cookie := lw.Result().Cookies()[0]

	var img bytes.Buffer
	if err := png.Encode(&img, image.NewGray(image.Rect(0, 0, 3, 3))); err != nil {
		t.Fatal(err)
	}
	restore := studioapi.SetTestHookBeforeRecord(func() {
		if _, err := perms.Revoke(ctx, testOperator, u.ID, permissions.Author); err != nil {
			t.Error(err)
		}
	})
	defer restore()
	r := httptest.NewRequest(http.MethodPost, "http://learnoo.test/api/media", bytes.NewReader(img.Bytes()))
	r.Header.Set("Content-Type", "image/png")
	r.Header.Set(auth.CSRFHeader, sess.CSRFToken)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"forbidden"`) {
		t.Fatalf("upload after revocation: %d %s", w.Code, w.Body)
	}
	var rows int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM media_assets) + (SELECT count(*) FROM media_uploads)`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("revoked upload recorded %d rows", rows)
	}
}
