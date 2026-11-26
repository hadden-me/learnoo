// Package studioweb serves the built Studio bundle (web/studio/dist, built
// by Vite with base /studio/) under /studio to signed-in staff: users holding
// author, publisher or administrator, or assigned to review a subject.
//
// Every response, assets included, needs a session and that access, and is
// sent with the strict Content Security Policy below and Cache-Control:
// private, no-store, except Vite's fingerprinted files under /studio/assets/
// (AssetCacheControl: kept by the browser, never by a shared cache). Anonymous navigations are redirected to the sign-in page;
// signed-in users without access get the reader's 403 page. Asset requests
// (a last path segment with a dot) are answered 404 in both cases, so the
// bundle's file names are not revealed. Other paths under /studio/ are client
// routes and get index.html (SPA fallback). Client routes must therefore
// never have a dot in their last segment (/studio/lessons/12 works,
// /studio/lessons/v1.2 would be treated as a missing file and get 404).
//
// The bundle comes from an fs.FS: in production an os.Root on
// LEARNOO_STUDIO_DIR, which refuses paths and symbolic links leaving it.
// The renderer experiment (cmd/renderer) has no part in this.
package studioweb

import (
	"bytes"
	"errors"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/i18n"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// CacheControl is sent on every Studio response but fingerprinted assets.
const CacheControl = "private, no-store"

// AssetCacheControl is sent on a fingerprinted bundle file (isFingerprinted)
// served to a signed-in staff member: its name changes with its content, so
// the browser may keep it without revalidating. It stays private (never in
// a shared cache) because the Studio is staff-only; the files themselves
// contain no user or lesson data, so one left in a browser cache after
// access is revoked reveals nothing. index.html, client routes, errors and
// 404s stay CacheControl.
const AssetCacheControl = "private, max-age=31536000, immutable"

// fingerprinted matches the names Vite gives built files under assets/
// (assets/[name]-[hash][extname], the hash at least 8 URL-safe characters).
var fingerprinted = regexp.MustCompile(`^assets/[A-Za-z0-9._-]+-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$`)

// CSP allows only same-origin scripts, styles, images, fonts and API calls.
// The Vite build emits an external module script and stylesheet and no
// inline script; the editor is built with TipTap's injectCSS disabled (its
// base ProseMirror rules are in the Studio stylesheet), because style-src
// 'self' refuses injected <style> elements. Inline style attributes are
// allowed for the editor (ProseMirror) and for KaTeX in previews, as on the
// reader's lesson page; inline scripts and <style> elements, eval and
// framing are not. tests/browser-reader/studio.spec.mjs loads the built
// Studio from the server and fails on any CSP violation.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; " +
	"img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// MaxFileBytes bounds a served file read into memory when the file system
// cannot seek.
const MaxFileBytes = 16 << 20

// Config wires the Studio shell.
type Config struct {
	Auth  *auth.Handlers
	Perms *permissions.Store
	// Files is the built bundle; it must contain index.html.
	Files fs.FS
	// ErrorPage renders an HTML error page (the reader's); heading and
	// message are message keys of internal/i18n.
	ErrorPage func(w http.ResponseWriter, r *http.Request, status int, heading, message string)
	// Meta is added to index.html as <meta name content> elements before
	// </head>: the application's fingerprinted asset URLs the Studio loads
	// at run time (KaTeX for the preview, see web/studio/src/preview.ts).
	Meta map[string]string
	Log  *slog.Logger
}

// Handler serves the Studio.
type Handler struct{ Config }

// New builds the handler.
func New(cfg Config) *Handler { return &Handler{Config: cfg} }

// Register mounts GET (and HEAD) /studio and /studio/{path...}.
func (h *Handler) Register(mux *http.ServeMux) {
	// i18n.Middleware: ?lang= sets the interface locale cookie, which the
	// Studio reads (web/studio/src/i18n); the shell's <html lang dir> follow it.
	handler := i18n.Middleware(h.Auth.Middleware(http.HandlerFunc(h.serve)))
	mux.Handle("GET /studio", handler)
	mux.Handle("GET /studio/{path...}", handler)
}

// isAsset reports a request for a file rather than a client route: its last
// segment contains a dot (index.html, assets/index-abc.js).
func isAsset(name string) bool { return strings.Contains(path.Base(name), ".") }

// validName accepts a relative path of plain segments, none hidden or empty.
func validName(name string) bool {
	if !fs.ValidPath(name) || strings.Contains(name, `\`) {
		return false
	}
	for seg := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Cache-Control", CacheControl)
	hdr.Set("Content-Security-Policy", CSP)
	hdr.Set("X-Content-Type-Options", "nosniff")
	name := r.PathValue("path")
	asset := name != "" && isAsset(name)

	sess, ok := auth.SessionFrom(r.Context())
	if !ok {
		if asset {
			notFound(w)
			return
		}
		next := "/studio"
		if name != "" && validName(name) {
			next += "/" + name
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	set, err := h.Perms.Load(r.Context(), sess.User.ID)
	if err != nil {
		h.Log.Error("studio: load permissions", "err", err)
		h.ErrorPage(w, r, http.StatusInternalServerError, "error.internal", "error.internalText")
		return
	}
	if !allowed(set) {
		if asset {
			notFound(w)
			return
		}
		h.ErrorPage(w, r, http.StatusForbidden, "studio.restricted", "studio.restrictedText")
		return
	}
	if !asset {
		name = "index.html"
	}
	if !validName(name) {
		notFound(w)
		return
	}
	h.serveFile(w, r, name)
}

// allowed reports Studio access: author, publisher or administrator, or a
// reviewer assignment for at least one subject.
func allowed(set permissions.Set) bool {
	return set.Has(permissions.Author) || set.Has(permissions.Publisher) ||
		set.Has(permissions.Administrator) || len(set.ReviewerSubjects()) > 0
}

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := h.Files.Open(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// Includes os.Root refusing a path that leaves the directory:
			// client-controlled, so only a debug record.
			h.Log.Debug("studio: open file refused", "err", err)
		}
		notFound(w)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		notFound(w)
		return
	}
	content, ok := f.(io.ReadSeeker)
	if name == "index.html" {
		ok = false // read into memory to add the meta elements and the locale
	}
	if !ok {
		data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
		if err != nil || len(data) > MaxFileBytes {
			h.Log.Error("studio: read file", "err", err)
			auth.WriteError(w, http.StatusInternalServerError, "internal", "Something went wrong. Try again later.")
			return
		}
		if name == "index.html" {
			data = withLocale(h.withMeta(data), i18n.From(r))
		}
		content = bytes.NewReader(data)
	}
	// A zero modification time sends no Last-Modified header: responses are
	// either never stored or immutable, so there is nothing to revalidate.
	if fingerprinted.MatchString(name) {
		w.Header().Set("Cache-Control", AssetCacheControl)
	}
	http.ServeContent(w, r, path.Base(name), time.Time{}, content)
}

// withMeta inserts the Meta elements, sorted by name, before the first
// </head>; a page without one is returned unchanged.
func (h *Handler) withMeta(page []byte) []byte {
	i := bytes.Index(page, []byte("</head>"))
	if i < 0 || len(h.Meta) == 0 {
		return page
	}
	var b bytes.Buffer
	b.Write(page[:i])
	for _, name := range slices.Sorted(maps.Keys(h.Meta)) {
		b.WriteString(`<meta name="` + html.EscapeString(name) + `" content="` + html.EscapeString(h.Meta[name]) + `">`)
	}
	b.Write(page[i:])
	return b.Bytes()
}

// withLocale sets the shell's <html lang dir> to the interface locale, so
// the page has the right language and direction before the script runs.
// Vite emits <html lang="fr">; another start tag is left unchanged.
func withLocale(page []byte, l i18n.Locale) []byte {
	return bytes.Replace(page, []byte(`<html lang="fr">`), []byte(`<html lang="`+string(l)+`" dir="`+l.Dir()+`">`), 1)
}

func notFound(w http.ResponseWriter) {
	auth.WriteError(w, http.StatusNotFound, "not_found", "Not found.")
}
