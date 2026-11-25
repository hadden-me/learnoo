package studioapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Uploads beyond MaxConcurrentUploads wait briefly, then get 503 without
// their body being read.
func TestUploadSlotsBoundConcurrentBodies(t *testing.T) {
	h := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	h.uploadWait = 10 * time.Millisecond
	for range MaxConcurrentUploads {
		h.uploadSlots <- struct{}{}
	}
	body := bytes.NewReader([]byte("not read"))
	r := httptest.NewRequest(http.MethodPost, "/api/media", body)
	r.Header.Set("Content-Type", "image/png")
	w := httptest.NewRecorder()
	h.uploadMedia(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"busy"`) || w.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if body.Len() != len("not read") {
		t.Fatal("a waiting upload read its body")
	}
}
