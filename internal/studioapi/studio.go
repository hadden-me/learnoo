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
// otherwise). The lesson listing and the preview need the author
// permission; the listing rechecks it inside its transaction. Every
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
	chain("POST /api/preview", author, h.preview)
}

// ---- catalogue ----

type subjectJSON struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
}

type chapterJSON struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
}

func (h *Handlers) listSubjects(w http.ResponseWriter, r *http.Request) {
	subjects, err := h.Catalogue.Subjects(r.Context())
	if err != nil {
		h.internal(w, "list subjects", err)
		return
	}
	out := make([]subjectJSON, len(subjects))
	for i, s := range subjects {
		out[i] = subjectJSON{ID: s.ID, Code: s.Code}
	}
	writeJSON(w, http.StatusOK, struct {
		Subjects []subjectJSON `json:"subjects"`
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
	out := make([]chapterJSON, len(chapters))
	for i, c := range chapters {
		out[i] = chapterJSON{ID: c.ID, Code: c.Code}
	}
	writeJSON(w, http.StatusOK, struct {
		Subject  subjectJSON   `json:"subject"`
		Chapters []chapterJSON `json:"chapters"`
	}{subjectJSON{ID: subject.ID, Code: subject.Code}, out})
}

// ---- the author's own lessons ----

type ownedLessonJSON struct {
	ID                  int64               `json:"id"`
	PublicID            string              `json:"publicId"`
	SubjectID           int64               `json:"subjectId"`
	SubjectCode         string              `json:"subjectCode"`
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
	ID          int64          `json:"id"`
	Number      int            `json:"number"`
	Status      lessons.Status `json:"status"`
	Title       string         `json:"title"`
	ChapterID   *int64         `json:"chapterId"`
	ChapterCode *string        `json:"chapterCode"`
	Version     int            `json:"version"`
	UpdatedAt   time.Time      `json:"updatedAt"`
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
			Language: l.Language, TranslationGroupID: l.TranslationGroupID, Archived: l.Archived,
			PublishedRevisionID: l.PublishedRevisionID, CreatedAt: l.CreatedAt,
			HasFeedback: l.FeedbackRevisionID != nil, FeedbackRevisionID: l.FeedbackRevisionID,
		}
		if lr := l.Latest; lr != nil {
			out[i].LatestRevision = &latestRevisionJSON{
				ID: lr.ID, Number: lr.Number, Status: lr.Status, Title: lr.Title, ChapterID: lr.ChapterID,
				ChapterCode: lr.ChapterCode, Version: lr.Version, UpdatedAt: lr.UpdatedAt,
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
}

var previewKeys = []string{"content"}

// preview validates a schema v1 document exactly as a draft save does and
// returns the reader's markup for it (rendering.RenderLesson). Nothing is
// stored.
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
	if !checkFields(w, f) {
		return
	}
	doc, err := lessons.ValidateContent(req.Content)
	if err != nil {
		h.writeLessonError(w, "preview", err, false)
		return
	}
	parts, err := rendering.RenderLesson(doc)
	if err != nil {
		h.internal(w, "preview", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		HTML string `json:"html"`
	}{string(parts.HTML())})
}
