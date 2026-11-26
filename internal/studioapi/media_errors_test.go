package studioapi

import (
	"bytes"
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
		"cmyk":          {fmt.Errorf("process: %w", media.ErrUnsupportedCMYK), http.StatusUnsupportedMediaType, "unsupported_cmyk", "RGB"},
		"too complex":   {fmt.Errorf("%w: decoding needs 300 MiB", media.ErrTooComplex), http.StatusUnprocessableEntity, "image_too_complex", "memory"},
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

// headerOnlyJPEG is a JPEG whose frame header declares w x h with the given
// marker (SOF0 or SOF2) and components (id, sampling), with an empty scan:
// enough for the checks that run before decoding.
func headerOnlyJPEG(sof byte, w, h int, adobe bool, comps ...[2]byte) []byte {
	seg := func(m byte, p []byte) []byte {
		return append([]byte{0xFF, m, byte((len(p) + 2) >> 8), byte(len(p) + 2)}, p...)
	}
	out := []byte{0xFF, 0xD8}
	if adobe {
		out = append(out, seg(0xEE, []byte("Adobe\x00\x64\x00\x00\x00\x00\x00"))...)
	}
	frame := []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(len(comps))}
	scan := []byte{byte(len(comps))}
	for _, c := range comps {
		frame = append(frame, c[0], c[1], 0)
		scan = append(scan, c[0], 0)
	}
	out = append(out, seg(sof, frame)...)
	out = append(out, seg(0xDA, append(scan, 0, 63, 0))...)
	return append(out, 0x00, 0xFF, 0xD9)
}

// CMYK JPEGs and images whose decoding would exceed the memory budget are
// refused with their own codes, from the headers alone.
func TestUploadRejectsExpensiveImages(t *testing.T) {
	h := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for name, c := range map[string]struct {
		body   []byte
		status int
		code   string
	}{
		"cmyk": {headerOnlyJPEG(0xC0, 64, 64, true, [2]byte{1, 0x11}, [2]byte{2, 0x11}, [2]byte{3, 0x11}, [2]byte{4, 0x11}),
			http.StatusUnsupportedMediaType, "unsupported_cmyk"},
		"progressive 4:4:4, 16 MP": {headerOnlyJPEG(0xC2, 4000, 4000, false, [2]byte{1, 0x11}, [2]byte{2, 0x11}, [2]byte{3, 0x11}),
			http.StatusUnprocessableEntity, "image_too_complex"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/media", bytes.NewReader(c.body))
		r.Header.Set("Content-Type", "image/jpeg")
		w := httptest.NewRecorder()
		h.uploadMedia(w, r)
		var b struct{ Error struct{ Code string } }
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		if w.Code != c.status || b.Error.Code != c.code {
			t.Errorf("%s: %d %s", name, w.Code, b.Error.Code)
		}
	}
}
