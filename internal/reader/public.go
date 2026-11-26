package reader

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/hadden-me/learnoo/internal/media"
)

// Public pages (LMS-B07, contract §9): the homepage, the catalogue, course
// overviews and course covers. They are the same for every visitor: they are
// mounted without the session middleware, so the cookie is never read (and
// never cleared), and their templates receive no user, CSRF token or other
// viewer-dependent value (publicView has no such field). They may therefore
// be stored by any cache, but every use revalidates (no-cache) against an
// ETag, so a suspension, withdrawal or unpublication shows on the next
// request.

// PublicCacheControl is sent on public pages and course covers.
const PublicCacheControl = "public, no-cache"

// publicView is the data every public template receives. It deliberately
// has no user or CSRF field.
type publicView struct {
	Title     string
	TitleLang string
	Data      any
}

var publicTemplates = sync.OnceValues(func() (map[string]*template.Template, error) {
	assets, err := embeddedAssets()
	if err != nil {
		return nil, err
	}
	base, err := template.New("").Funcs(template.FuncMap{"dir": Dir, "asset": assets.url}).
		ParseFS(files, "templates/layout.html", "templates/public_layout.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	for _, name := range []string{"public_home", "public_catalogue", "public_catalogue_section", "public_catalogue_subject", "public_course", "public_notice"} {
		clone, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if pages[name], err = clone.ParseFS(files, "templates/"+name+".html"); err != nil {
			return nil, err
		}
	}
	return pages, nil
})

// registerPublic mounts the public routes.
func (h *Handlers) registerPublic(mux *http.ServeMux) {
	if _, err := publicTemplates(); err != nil {
		panic(err) // a build defect, as in New
	}
	page := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, publicHeaders(fn)) }
	page("GET /{$}", h.home)
	page("GET /catalogue", h.catalogueIndex)
	page("GET /catalogue/{level}/{section}", h.catalogueSection)
	page("GET /catalogue/{level}/{section}/{subject}", h.catalogueSubject)
	page("GET /courses/{publicId}", h.courseOverview)
	page("GET /courses/{publicId}/cover", h.courseCover)
}

// publicHeaders applies the public cache rule. The response never depends
// on the cookie, so Vary: Cookie (set server-wide) is removed.
func publicHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Del("Vary")
		hdr.Set("Cache-Control", PublicCacheControl)
		hdr.Set("Content-Security-Policy", cspPage)
		next.ServeHTTP(w, r)
	})
}

// renderPublic renders a public page with a strong ETag of its bytes; a 200
// answers If-None-Match with 304 (http.ServeContent).
func (h *Handlers) renderPublic(w http.ResponseWriter, r *http.Request, status int, name string, v publicView) {
	pages, _ := publicTemplates() // checked in registerPublic
	var buf bytes.Buffer
	if err := pages[name].ExecuteTemplate(&buf, "public", v); err != nil {
		h.Log.Error("public template", "page", name, "err", err)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Erreur interne.\n"))
		return
	}
	sum := sha256.Sum256(buf.Bytes())
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	if status == http.StatusOK {
		hdr.Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(buf.Bytes()))
		return
	}
	hdr.Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = buf.WriteTo(w)
	}
}

// publicNotFound renders the 404 notice. A negative answer is never stored
// (no-store, no ETag), so a course that becomes readable shows at once.
func (h *Handlers) publicNotFound(w http.ResponseWriter, r *http.Request, heading, message string) {
	w.Header().Set("Cache-Control", "no-store")
	h.renderPublic(w, r, http.StatusNotFound, "public_notice", publicView{Title: heading,
		Data: struct{ Heading, Message string }{heading, message}})
}

// publicInternal answers a failure without storing it anywhere.
func (h *Handlers) publicInternal(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.Log.Error("public page failure", "op", op, "err", err)
	w.Header().Set("Cache-Control", "no-store")
	h.renderPublic(w, r, http.StatusInternalServerError, "public_notice", publicView{Title: "Erreur interne",
		Data: struct{ Heading, Message string }{"Erreur interne", "Un problème est survenu. Réessayez plus tard."}})
}

type homeData struct {
	Levels []PublicLevel
	// Studio is the sign-in link that leads teachers to the Studio.
	Studio string
}

// Available reports whether any section has a current edition.
func (d homeData) Available() bool {
	for _, lv := range d.Levels {
		for _, s := range lv.Sections {
			if s.Current != nil {
				return true
			}
		}
	}
	return false
}

func (h *Handlers) home(w http.ResponseWriter, r *http.Request) {
	levels, err := h.Store.Curriculum(r.Context())
	if err != nil {
		h.publicInternal(w, r, "curriculum", err)
		return
	}
	h.renderPublic(w, r, http.StatusOK, "public_home", publicView{Title: "Accueil",
		Data: homeData{Levels: levels, Studio: signInURL("/studio")}})
}

type catalogueData struct {
	Levels   []PublicLevel
	Unplaced []UnplacedSubject
}

func (h *Handlers) catalogueIndex(w http.ResponseWriter, r *http.Request) {
	levels, unplaced, err := h.Store.CatalogueIndex(r.Context())
	if err != nil {
		h.publicInternal(w, r, "catalogue index", err)
		return
	}
	h.renderPublic(w, r, http.StatusOK, "public_catalogue", publicView{Title: "Catalogue",
		Data: catalogueData{Levels: levels, Unplaced: unplaced}})
}

// editionParam returns the ?edition= value, or ok=false when it is repeated
// or empty-but-present (a malformed link).
func editionParam(r *http.Request) (string, bool) {
	vals, present := r.URL.Query()["edition"]
	if !present {
		return "", true
	}
	if len(vals) != 1 || vals[0] == "" {
		return "", false
	}
	return vals[0], true
}

func (h *Handlers) catalogueNotFound(w http.ResponseWriter, r *http.Request) {
	h.publicNotFound(w, r, "Page introuvable", "Cette partie du catalogue n’existe pas ou n’est pas publiée.")
}

func (h *Handlers) catalogueSection(w http.ResponseWriter, r *http.Request) {
	edition, ok := editionParam(r)
	if !ok {
		h.catalogueNotFound(w, r)
		return
	}
	p, err := h.Store.Section(r.Context(), r.PathValue("level"), r.PathValue("section"), edition)
	if errors.Is(err, ErrNotFound) {
		h.catalogueNotFound(w, r)
		return
	}
	if err != nil {
		h.publicInternal(w, r, "section", err)
		return
	}
	h.renderPublic(w, r, http.StatusOK, "public_catalogue_section", publicView{Title: p.Name, Data: p})
}

func (h *Handlers) catalogueSubject(w http.ResponseWriter, r *http.Request) {
	edition, ok := editionParam(r)
	if !ok {
		h.catalogueNotFound(w, r)
		return
	}
	p, err := h.Store.Subject(r.Context(), r.PathValue("level"), r.PathValue("section"), r.PathValue("subject"), edition)
	if errors.Is(err, ErrNotFound) {
		h.catalogueNotFound(w, r)
		return
	}
	if err != nil {
		h.publicInternal(w, r, "subject", err)
		return
	}
	label := p.Label()
	v := publicView{Title: label.Text, Data: p}
	if label.Lang == "ar" {
		v.TitleLang = "ar"
	}
	h.renderPublic(w, r, http.StatusOK, "public_catalogue_subject", v)
}

// signInURL is the sign-in page with a validated local return path: target
// if SafeNext accepts it, /learn otherwise.
func signInURL(target string) string {
	return "/login?next=" + url.QueryEscape(SafeNext(target))
}

// CourseReadPath is the signed-in course reading route (LMS-B08).
func CourseReadPath(publicID string) string { return "/learn/courses/" + publicID }

type courseData struct {
	CourseOverview
	SignIn string
}

// CoverURL is the public cover route of the course.
func (c courseData) CoverURL() string { return "/courses/" + c.PublicID + "/cover" }

func (h *Handlers) courseOverview(w http.ResponseWriter, r *http.Request) {
	c, err := h.Store.Course(r.Context(), r.PathValue("publicId"))
	if errors.Is(err, ErrNotFound) {
		// The same notice for unknown, never published, suspended,
		// archived and withdrawn courses; no earlier revision is shown.
		h.publicNotFound(w, r, "Cours indisponible", "Ce cours n’existe pas ou n’est pas disponible pour le moment.")
		return
	}
	if err != nil {
		h.publicInternal(w, r, "course overview", err)
		return
	}
	v := publicView{Title: c.Title, Data: courseData{CourseOverview: c, SignIn: signInURL(CourseReadPath(c.PublicID))}}
	if c.Language == "ar" {
		v.TitleLang = "ar"
	}
	h.renderPublic(w, r, http.StatusOK, "public_course", v)
}

// coverNotFound is the JSON 404 of the cover route; like every negative
// answer it must not be stored by any cache.
func coverNotFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("ETag")
	staticNotFound(w)
}

// courseCover serves the cover of a readable course's published revision
// (contract §4). Everything else, including the cover of a suspended,
// archived or withdrawn course and of an earlier revision, gets the JSON
// 404. Readability is checked before the ETag, so a revalidation after a
// suspension gets 404, not 304. The stored file never changes for a key, so
// the ETag derives from the key.
func (h *Handlers) courseCover(w http.ResponseWriter, r *http.Request) {
	key, err := h.Store.CourseCover(r.Context(), r.PathValue("publicId"))
	if errors.Is(err, ErrNotFound) {
		coverNotFound(w)
		return
	}
	if err != nil {
		h.Log.Error("public page failure", "op", "course cover", "err", err)
		coverNotFound(w)
		return
	}
	ctype, err := media.ContentTypeForKey(key)
	if err != nil {
		h.Log.Error("course cover unavailable", "err", err)
		coverNotFound(w)
		return
	}
	f, err := h.Media.Open(key)
	if err != nil {
		h.Log.Error("course cover unavailable", "err", err)
		coverNotFound(w)
		return
	}
	defer f.Close()
	hdr := w.Header()
	hdr.Set("Content-Type", ctype)
	hdr.Set("Content-Disposition", "inline")
	hdr.Set("Content-Security-Policy", media.ServeCSP)
	hdr.Set("ETag", `"`+digest([]byte("course-cover\x00" + key))[:32]+`"`)
	http.ServeContent(w, r, "", time.Time{}, f)
}
