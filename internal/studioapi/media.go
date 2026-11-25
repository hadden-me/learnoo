package studioapi

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5"
)

// mediaCSP forbids everything a served image could try to do if a browser
// ever treated it as a document.
const mediaCSP = "default-src 'none'; frame-ancestors 'none'; sandbox"

type mediaJSON struct {
	ID          int64  `json:"id"`
	ContentType string `json:"contentType"`
	ByteSize    int64  `json:"byteSize"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	SHA256      string `json:"sha256"`
	URL         string `json:"url"`
}

// uploadMedia accepts one raw image body (Content-Type image/png, image/jpeg
// or image/webp), validates and re-encodes it through media.Process, stores
// it content-addressed and records it in media_assets. Identical canonical
// bytes reuse the existing asset.
func (h *Handlers) uploadMedia(w http.ResponseWriter, r *http.Request) {
	declared := r.Header.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(declared)
	if err != nil || (mt != media.ContentTypePNG && mt != media.ContentTypeJPEG && mt != media.ContentTypeWebP) {
		auth.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Upload a PNG, JPEG or WebP image as the raw request body with its Content-Type.")
		return
	}
	if r.ContentLength > media.MaxUploadBytes {
		tooLarge(w)
		return
	}

	// Bound how many bodies are held in memory at once. Waiting requests
	// have not read their body yet.
	wait, cancel := context.WithTimeout(r.Context(), h.uploadWait)
	defer cancel()
	select {
	case h.uploadSlots <- struct{}{}:
		defer func() { <-h.uploadSlots }()
	case <-wait.Done():
		busy(w)
		return
	}

	// MaxBytesReader stops reading after the limit and makes the server
	// close the connection instead of draining an oversized body.
	body := http.MaxBytesReader(w, r.Body, media.MaxUploadBytes)
	asset, err := media.Process(r.Context(), body, declared)
	if err != nil {
		h.writeMediaError(w, r, err)
		return
	}
	key, err := h.Media.Put(asset)
	if err != nil {
		h.internal(w, "store media", err)
		return
	}
	sum, err := hex.DecodeString(asset.SHA256())
	if err != nil {
		h.internal(w, "store media", err)
		return
	}
	res := mediaJSON{
		ContentType: asset.ContentType(), ByteSize: asset.ByteSize(), Width: asset.Width(),
		Height: asset.Height(), SHA256: asset.SHA256(),
	}
	// The file is stored before the row, so a row never names a missing file.
	// A file whose row is not written (failure, revoked permission) is
	// unreferenced and harmless: it is never served.
	err = pgx.BeginFunc(r.Context(), h.DB, func(tx pgx.Tx) error {
		if testHookBeforeRecord != nil {
			testHookBeforeRecord()
		}
		if err := permissions.RequireTx(r.Context(), tx, actor(r), permissions.Author); err != nil {
			return err
		}
		if err := insertAsset(r.Context(), tx, asset, key, sum, actor(r), &res.ID); err != nil {
			return err
		}
		// The uploader's access link, also for a deduplicated asset.
		_, err := tx.Exec(r.Context(), `
			INSERT INTO media_uploads (user_id, media_asset_id, created_at) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, actor(r), res.ID, time.Now().UTC())
		return err
	})
	switch {
	case isRetryable(err):
		writeRetry(w)
		return
	case deniedWithCause(err):
		h.internal(w, "record media", err)
		return
	case errors.Is(err, permissions.ErrDenied):
		auth.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
		return
	case err != nil:
		h.internal(w, "record media", err)
		return
	}
	res.URL = "/api/media/" + strconv.FormatInt(res.ID, 10)
	// 201 whether the asset is new or deduplicated, so the status does not
	// reveal whether someone else uploaded the same image.
	w.Header().Set("Location", res.URL)
	writeJSON(w, http.StatusCreated, res)
}

// testHookBeforeRecord, when set, runs inside the recording transaction
// before the permission recheck.
var testHookBeforeRecord func()

// insertAsset records the asset row, or finds the existing row with the same
// canonical bytes, and stores its ID in id.
func insertAsset(ctx context.Context, tx pgx.Tx, asset *media.Asset, key string, sum []byte, uploader int64, id *int64) error {
	err := tx.QueryRow(ctx, `
		INSERT INTO media_assets (sha256, content_type, byte_size, width, height, storage_key, created_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		sum, asset.ContentType(), asset.ByteSize(), asset.Width(), asset.Height(), key,
		time.Now().UTC(), uploader).Scan(id)
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// Same canonical bytes already stored (a conflict on sha256 or on
	// storage_key, which is derived from it; a concurrent upload can hit
	// either first): assets are immutable, so the existing row is the same
	// image, and its created_by stays the first uploader.
	return tx.QueryRow(ctx, `SELECT id FROM media_assets WHERE sha256 = $1`, sum).Scan(id)
}

// isTimeout reports a body read that hit the server's read deadline.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout())
}

func tooLarge(w http.ResponseWriter) {
	auth.WriteError(w, http.StatusRequestEntityTooLarge, "too_large",
		fmt.Sprintf("Images may be at most %d MiB.", media.MaxUploadBytes>>20))
}

func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	auth.WriteError(w, http.StatusServiceUnavailable, "busy", "The server is processing other uploads. Try again shortly.")
}

func (h *Handlers) writeMediaError(w http.ResponseWriter, r *http.Request, err error) {
	var maxBytes *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytes), errors.Is(err, media.ErrTooLarge):
		tooLarge(w)
	case errors.Is(err, media.ErrEmpty):
		auth.WriteError(w, http.StatusBadRequest, "invalid_request", "The upload is empty.")
	case errors.Is(err, media.ErrUnsupportedType):
		auth.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Only PNG, JPEG and WebP images are accepted (no SVG, GIF or other formats).")
	case errors.Is(err, media.ErrTypeMismatch):
		auth.WriteError(w, http.StatusUnsupportedMediaType, "type_mismatch",
			"The file's content does not match its declared type.")
	case errors.Is(err, media.ErrDimensions):
		auth.WriteError(w, http.StatusUnprocessableEntity, "invalid_dimensions",
			fmt.Sprintf("Images may be at most %d pixels per side and %d megapixels.", media.MaxSide, media.MaxPixels/1_000_000))
	case errors.Is(err, media.ErrTrailingData):
		auth.WriteError(w, http.StatusUnprocessableEntity, "trailing_data",
			"The file has extra data after the end of the image, as many phone photos do. "+
				"Re-export the image (or take a screenshot of it) and upload that, or upload it as PNG.")
	case errors.Is(err, media.ErrCorrupt):
		auth.WriteError(w, http.StatusUnprocessableEntity, "invalid_image", "The image is corrupt or truncated.")
	case isTimeout(err):
		auth.WriteError(w, http.StatusRequestTimeout, "upload_timeout",
			"The upload took too long to arrive. Check the connection, or upload a smaller image.")
	case r.Context().Err() != nil, errors.Is(err, context.Canceled), errors.Is(err, io.ErrUnexpectedEOF):
		auth.WriteError(w, http.StatusBadRequest, "invalid_request", "The upload was interrupted.")
	default:
		// Read errors (client disconnect, read timeout) or encoder failures.
		h.Log.Warn("media upload rejected", "err", err)
		auth.WriteError(w, http.StatusBadRequest, "invalid_request", "The upload could not be read.")
	}
}

// readableBy is the media read rule for the user in parameter $2 on asset
// row a: the user uploaded the asset (media_uploads, also for a deduplicated
// upload) or authored a revision that references it. Reviewer and reader
// access come with LRN-007 and LRN-008.
const readableBy = `(EXISTS (SELECT 1 FROM media_uploads u WHERE u.media_asset_id = a.id AND u.user_id = $2)
	OR EXISTS (SELECT 1 FROM revision_media rm JOIN lesson_revisions r ON r.id = rm.revision_id
	            WHERE rm.media_asset_id = a.id AND r.author_id = $2))`

// checkMedia rejects media IDs the caller cannot read, with the same
// response as for IDs that do not exist, so a draft never references an
// asset its author could not see. Access only grows (links are never
// removed), except when the caller discards their own draft, so this check
// runs before the lessons transaction.
func (h *Handlers) checkMedia(w http.ResponseWriter, r *http.Request, ids []int64) bool {
	if len(ids) == 0 {
		return true
	}
	var unreadable bool
	err := h.DB.QueryRow(r.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM unnest($1::bigint[]) AS want(id)
			 WHERE NOT EXISTS (SELECT 1 FROM media_assets a WHERE a.id = want.id AND `+readableBy+`))`,
		ids, actor(r)).Scan(&unreadable)
	switch {
	case isRetryable(err):
		writeRetry(w)
		return false
	case err != nil:
		h.internal(w, "check media", err)
		return false
	case unreadable:
		auth.WriteError(w, http.StatusUnprocessableEntity, "unknown_media", "A referenced media asset does not exist.")
		return false
	}
	return true
}

// serveMedia returns a stored image to a signed-in author. The content type
// comes from the validated storage key, never from the request.
func (h *Handlers) serveMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("mediaId"))
	if !ok {
		notFound(w)
		return
	}
	var key string
	err := h.DB.QueryRow(r.Context(), `
		SELECT storage_key FROM media_assets a
		 WHERE id = $1 AND `+readableBy+``, id, actor(r)).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		notFound(w)
		return
	}
	if err != nil {
		h.internal(w, "load media", err)
		return
	}
	ctype, err := media.ContentTypeForKey(key)
	if err != nil {
		// Rows not written by the upload endpoint (or a corrupted key) are
		// never served.
		h.Log.Error("media row has an invalid storage key", "id", id)
		notFound(w)
		return
	}
	f, err := h.Media.Open(key)
	if err != nil {
		h.Log.Error("media file unavailable", "id", id, "err", err)
		notFound(w)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		h.internal(w, "stat media", err)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", ctype)
	hd.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	hd.Set("Content-Disposition", "inline")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", mediaCSP)
	// Authenticated content: no shared or browser storage.
	hd.Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}
