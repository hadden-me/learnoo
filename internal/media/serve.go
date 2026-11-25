package media

import (
	"errors"
	"io"
	"net/http"
	"strconv"
)

// ServeCSP forbids everything a served image could try to do if a browser
// ever treated it as a document.
const ServeCSP = "default-src 'none'; frame-ancestors 'none'; sandbox"

// ErrUnavailable means a stored asset cannot be served: its key is invalid
// or its file is missing or unreadable. Callers answer it like a missing ID.
var ErrUnavailable = errors.New("media: asset unavailable")

// Serve writes the stored asset named by key (a media_assets.storage_key the
// caller has already authorized) as an inline image response, also for HEAD.
// The content type comes from the validated key, never from the request; the
// response is private and never stored, and cannot be sniffed or run as a
// document. Before anything is written, an unservable asset returns an error
// wrapping ErrUnavailable; the caller then answers 404.
func (s *Store) Serve(w http.ResponseWriter, r *http.Request, key string) error {
	ctype, err := ContentTypeForKey(key)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	f, err := s.Open(key)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	hd := w.Header()
	hd.Set("Content-Type", ctype)
	hd.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	hd.Set("Content-Disposition", "inline")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", ServeCSP)
	// Authenticated content: no shared or browser storage.
	hd.Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
	return nil
}
