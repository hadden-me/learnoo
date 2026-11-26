package studioapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/rendering"
)

// Studio integration endpoints (issue #65), documented in docs/API.md:
// the catalogue the Studio picks subjects and chapters from, the author's
// own lessons, and the canonical preview.
//
// The catalogue is read by authors and by reviewers of any subject (403
// otherwise). The lesson listing needs the author permission and rechecks
// it inside its transaction; the preview is open to every account with
// Studio access (author, publisher, administrator or any reviewer). Every
// response is private and never stored.

// privateNoStore is the Cache-Control of the Studio endpoints.
const privateNoStore = "private, no-store"

func (h *Handlers) registerStudio(mux *http.ServeMux) {
	chain := func(pattern string, guard func(http.Handler) http.Handler, fn http.HandlerFunc) {
		mux.Handle(pattern, h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(guard(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", privateNoStore)
				fn(w, r)
			}))))))
	}
	catalogueReader := func(next http.Handler) http.Handler {
		return h.Perms.RequireReviewerOrAny([]permissions.Permission{permissions.Author}, next)
	}
	author := func(next http.Handler) http.Handler { return h.Perms.Require(permissions.Author, next) }
	chain("GET /api/catalogue/subjects", catalogueReader, h.listSubjects)
	chain("GET /api/catalogue/subjects/{code}/chapters", catalogueReader, h.listChapters)
	chain("GET /api/lessons/mine", author, h.listOwned)
	// Every account with Studio access (the /studio page rule) may preview:
	// reviewers, publishers and administrators read submitted revisions
	// through it. It renders the posted document and stores nothing.
	studioAccess := func(next http.Handler) http.Handler {
		return h.Perms.RequireReviewerOrAny([]permissions.Permission{permissions.Author, permissions.Publisher, permissions.Administrator}, next)
	}
	chain("POST /api/preview", studioAccess, h.preview)
}

// ---- catalogue ----

// A catalogue entry: its immutable code and ID, its display names (null
// until an operator sets them; show the code instead) and its display order.
type entryJSON struct {
	ID        int64   `json:"id"`
	Code      string  `json:"code"`
	NameFr    *string `json:"nameFr"`
	NameAr    *string `json:"nameAr"`
	SortOrder int     `json:"sortOrder"`
}

func subjectEntry(s catalogue.Subject) entryJSON {
	return entryJSON{ID: s.ID, Code: s.Code, NameFr: s.Names.Fr, NameAr: s.Names.Ar, SortOrder: s.SortOrder}
}

func chapterEntry(c catalogue.Chapter) entryJSON {
	return entryJSON{ID: c.ID, Code: c.Code, NameFr: c.Names.Fr, NameAr: c.Names.Ar, SortOrder: c.SortOrder}
}

func (h *Handlers) listSubjects(w http.ResponseWriter, r *http.Request) {
	subjects, err := h.Catalogue.Subjects(r.Context())
	if err != nil {
		h.internal(w, "list subjects", err)
		return
	}
	out := make([]entryJSON, len(subjects))
	for i, s := range subjects {
		out[i] = subjectEntry(s)
	}
	writeJSON(w, http.StatusOK, struct {
		Subjects []entryJSON `json:"subjects"`
	}{out})
}

func (h *Handlers) listChapters(w http.ResponseWriter, r *http.Request) {
	subject, chapters, err := h.Catalogue.Chapters(r.Context(), r.PathValue("code"))
	if errors.Is(err, catalogue.ErrNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		h.internal(w, "list chapters", err)
		return
	}
	out := make([]entryJSON, len(chapters))
	for i, c := range chapters {
		out[i] = chapterEntry(c)
	}
	writeJSON(w, http.StatusOK, struct {
		Subject  entryJSON   `json:"subject"`
		Chapters []entryJSON `json:"chapters"`
	}{subjectEntry(subject), out})
}

// ---- the author's own lessons ----

type ownedLessonJSON struct {
	ID                  int64               `json:"id"`
	PublicID            string              `json:"publicId"`
	SubjectID           int64               `json:"subjectId"`
	SubjectCode         string              `json:"subjectCode"`
	SubjectNameFr       *string             `json:"subjectNameFr"`
	SubjectNameAr       *string             `json:"subjectNameAr"`
	Language            string              `json:"language"`
	TranslationGroupID  string              `json:"translationGroupId"`
	Archived            bool                `json:"archived"`
	PublishedRevisionID *int64              `json:"publishedRevisionId"`
	CreatedAt           time.Time           `json:"createdAt"`
	HasFeedback         bool                `json:"hasFeedback"`
	FeedbackRevisionID  *int64              `json:"feedbackRevisionId"`
	LatestRevision      *latestRevisionJSON `json:"latestRevision"`
}

type latestRevisionJSON struct {
	ID            int64          `json:"id"`
	Number        int            `json:"number"`
	Status        lessons.Status `json:"status"`
	Title         string         `json:"title"`
	ChapterID     *int64         `json:"chapterId"`
	ChapterCode   *string        `json:"chapterCode"`
	ChapterNameFr *string        `json:"chapterNameFr"`
	ChapterNameAr *string        `json:"chapterNameAr"`
	Version       int            `json:"version"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

func (h *Handlers) listOwned(w http.ResponseWriter, r *http.Request) {
	owned, err := h.Lessons.Owned(r.Context(), actor(r))
	if err != nil {
		h.writeLessonError(w, "list owned lessons", err, false)
		return
	}
	out := make([]ownedLessonJSON, len(owned))
	for i, l := range owned {
		out[i] = ownedLessonJSON{
			ID: l.ID, PublicID: l.PublicID, SubjectID: l.SubjectID, SubjectCode: l.SubjectCode,
			SubjectNameFr: l.SubjectNames.Fr, SubjectNameAr: l.SubjectNames.Ar,
			Language: l.Language, TranslationGroupID: l.TranslationGroupID, Archived: l.Archived,
			PublishedRevisionID: l.PublishedRevisionID, CreatedAt: l.CreatedAt,
			HasFeedback: l.FeedbackRevisionID != nil, FeedbackRevisionID: l.FeedbackRevisionID,
		}
		if lr := l.Latest; lr != nil {
			out[i].LatestRevision = &latestRevisionJSON{
				ID: lr.ID, Number: lr.Number, Status: lr.Status, Title: lr.Title, ChapterID: lr.ChapterID,
				ChapterCode: lr.ChapterCode, ChapterNameFr: lr.ChapterNames.Fr, ChapterNameAr: lr.ChapterNames.Ar,
				Version: lr.Version, UpdatedAt: lr.UpdatedAt,
			}
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Lessons []ownedLessonJSON `json:"lessons"`
	}{out})
}

// ---- canonical preview ----

type previewRequest struct {
	Content json.RawMessage `json:"content"`
	// MediaRoute picks the route embedded uploaded images are linked to:
	// "author" (the default, GET /api/media/{id}), "review"
	// (GET /api/review/media/{id}) or "publish" (GET /api/publish/media/{id}).
	// It only changes those URLs; each route checks its own access when the
	// image is fetched.
	MediaRoute *string `json:"mediaRoute" api:"nonnull"`
}

var previewKeys = []string{"content", "mediaRoute"}

// previewMedia maps previewRequest.MediaRoute to the Studio's media routes.
var previewMedia = map[string]rendering.MediaURL{
	"author":  rendering.MediaPrefix("/api/media/"),
	"review":  rendering.MediaPrefix("/api/review/media/"),
	"publish": rendering.MediaPrefix("/api/publish/media/"),
}

// preview validates a schema v1 document exactly as a draft save does and
// returns the reader's markup for it (rendering.RenderLesson). Nothing is
// stored. The markup equals the reader's except for the URLs of embedded
// uploaded images, which point to the Studio's author or reviewer media
// route instead of the reader's.
func (h *Handlers) preview(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if !decodeJSON(w, r, &req, previewKeys) {
		return
	}
	f := fieldsJSON{Content: req.Content}
	if !f.valid() {
		badRequest(w, "content is required.")
		return
	}
	route := "author"
	if req.MediaRoute != nil {
		route = *req.MediaRoute
	}
	mediaURL, ok := previewMedia[route]
	if !ok {
		badRequest(w, `mediaRoute must be "author", "review" or "publish".`)
		return
	}
	if !checkFields(w, f) {
		return
	}
	doc, err := lessons.ValidateContent(req.Content)
	if err != nil {
		h.writeLessonError(w, "preview", err, false)
		return
	}
	parts, err := rendering.RenderLesson(doc, mediaURL)
	if err != nil {
		h.internal(w, "preview", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		HTML string `json:"html"`
	}{string(parts.HTML())})
}
