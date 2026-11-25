package studioapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/media"
)

func TestWriteMediaErrorCodes(t *testing.T) {
	h := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r := httptest.NewRequest(http.MethodPost, "/api/media", nil)
	for name, c := range map[string]struct {
		err     error
		status  int
		code    string
		message string
	}{
		"heic":          {fmt.Errorf("process: %w", media.ErrUnsupportedHEIC), http.StatusUnsupportedMediaType, "unsupported_heic", "JPEG"},
		"other type":    {fmt.Errorf("%w: only PNG", media.ErrUnsupportedType), http.StatusUnsupportedMediaType, "unsupported_media_type", "PNG"},
		"trailing data": {fmt.Errorf("%w: undeclared data", media.ErrTrailingData), http.StatusUnprocessableEntity, "trailing_data", "PNG"},
		"dimensions":    {media.ErrDimensions, http.StatusUnprocessableEntity, "invalid_dimensions", "4096"},
		"corrupt":       {media.ErrCorrupt, http.StatusUnprocessableEntity, "invalid_image", "corrupt"},
	} {
		w := httptest.NewRecorder()
		h.writeMediaError(w, r, c.err)
		var b struct {
			Error struct{ Code, Message string }
		}
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		if w.Code != c.status || b.Error.Code != c.code || !strings.Contains(b.Error.Message, c.message) {
			t.Errorf("%s: %d %s %q", name, w.Code, b.Error.Code, b.Error.Message)
		}
	}
}

// A declared HEIC/HEIF type is answered before any body is read.
func TestUploadDeclaredHEIC(t *testing.T) {
	h := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, ct := range []string{"image/heic", "image/heif", "image/heic-sequence", "image/heif-sequence; foo=bar"} {
		body := &failingReader{}
		r := httptest.NewRequest(http.MethodPost, "/api/media", body)
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		h.uploadMedia(w, r)
		var b struct{ Error struct{ Code string } }
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		if w.Code != http.StatusUnsupportedMediaType || b.Error.Code != "unsupported_heic" || body.read {
			t.Errorf("%s: %d %s (body read: %v)", ct, w.Code, b.Error.Code, body.read)
		}
	}
}

type failingReader struct{ read bool }

func (f *failingReader) Read([]byte) (int, error) {
	f.read = true
	return 0, io.ErrUnexpectedEOF
}
