// Package studioapi exposes the Studio ↔ server endpoints for lesson drafts
// and media (LRN-006 slice 3) and for review and publication (LRN-007,
// review.go), documented in docs/API.md.
//
// Draft and media routes require a session and the global author
// permission, checked on the server by permissions.Middleware before any
// lookup; lesson writes recheck it inside their transaction (lessons.Store).
// Unsafe methods also need the session's CSRF token. Revisions belong to
// their author: another author receives the same 404 as for an ID that does
// not exist, so revision IDs cannot be probed. Reader access is later work.
package studioapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/review"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxLessonBody bounds JSON request bodies: twice the 128 KiB document limit,
// leaving room for formatting, prerequisites and media IDs.
const MaxLessonBody = 256 << 10

// MaxConcurrentUploads bounds uploads being read or processed at once in this
// process, so at most this many request bodies (each at most
// media.MaxUploadBytes) are held in memory. Decoding is further limited to
// media.MaxConcurrentDecodes.
const MaxConcurrentUploads = 4

// UploadSlotWait is how long an upload waits for a free slot before the
// server answers 503 busy. Waiting requests have not read their body yet.
const UploadSlotWait = 5 * time.Second

// Config holds the dependencies of the handlers.
type Config struct {
	DB      *pgxpool.Pool
	Auth    *auth.Handlers
	Perms   *permissions.Middleware
	Lessons *lessons.Store
	Media   *media.Store
	Review  *review.Store
	Log     *slog.Logger
}

// Handlers serves the Studio API.
type Handlers struct {
	Config
	uploadSlots chan struct{}
	uploadWait  time.Duration
}

// New builds the handlers.
func New(cfg Config) *Handlers {
	return &Handlers{Config: cfg, uploadSlots: make(chan struct{}, MaxConcurrentUploads), uploadWait: UploadSlotWait}
}

// Register mounts the endpoints on mux.
func (h *Handlers) Register(mux *http.ServeMux) {
	route := func(pattern string, fn http.HandlerFunc) {
		// Session, then 401 for anonymous callers, then the CSRF token on
		// unsafe methods, then the author permission (403 without it). The
		// permission check runs before any ID is looked up, so a denial
		// reveals nothing about which resources exist.
		mux.Handle(pattern, h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(
			h.Perms.Require(permissions.Author, fn)))))
	}
	route("POST /api/lessons", h.createLesson)
	route("POST /api/lessons/{lessonId}/drafts", h.createDraft)
	route("GET /api/revisions/{revisionId}", h.getRevision)
	route("PUT /api/revisions/{revisionId}", h.saveDraft)
	route("POST /api/revisions/{revisionId}/submit", h.submit)
	route("DELETE /api/revisions/{revisionId}", h.discard)
	route("POST /api/media", h.uploadMedia)
	route("GET /api/media/{mediaId}", h.serveMedia)
	h.registerReview(mux)
}

// idPattern accepts positive decimal IDs without sign, leading zeros or
// anything that would overflow int64.
var idPattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

func parseID(s string) (int64, bool) {
	if !idPattern.MatchString(s) {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}

func actor(r *http.Request) int64 {
	s, _ := auth.SessionFrom(r.Context())
	return s.User.ID
}

// Request bodies.

type fieldsJSON struct {
	Content       json.RawMessage `json:"content"`
	Prerequisites string          `json:"prerequisites"`
	ChapterID     *int64          `json:"chapterId"`
	MediaIDs      []int64         `json:"mediaIds"`
}

func (f fieldsJSON) fields() lessons.Fields {
	return lessons.Fields{Content: f.Content, Prerequisites: f.Prerequisites, ChapterID: f.ChapterID, MediaIDs: f.MediaIDs}
}

func (f fieldsJSON) valid() bool {
	c := strings.TrimSpace(string(f.Content))
	return c != "" && c != "null"
}

type createLessonRequest struct {
	SubjectID     int64  `json:"subjectId"`
	Language      string `json:"language"`
	TranslationOf *int64 `json:"translationOf"`
	fieldsJSON
}

// ExpectedVersion is a pointer only to tell a missing key from 0; api:"nonnull"
// makes strictDecode refuse null for it like any other required value.
type saveRequest struct {
	ExpectedVersion *int `json:"expectedVersion" api:"nonnull"`
	fieldsJSON
}

type submitRequest struct {
	ExpectedVersion *int `json:"expectedVersion" api:"nonnull"`
}

// Responses.

type revisionJSON struct {
	ID             int64           `json:"id"`
	LessonID       int64           `json:"lessonId"`
	Number         int             `json:"number"`
	Status         lessons.Status  `json:"status"`
	AuthorID       int64           `json:"authorId"`
	BaseRevisionID *int64          `json:"baseRevisionId"`
	Title          string          `json:"title"`
	Objectives     string          `json:"objectives"`
	Prerequisites  string          `json:"prerequisites"`
	Content        json.RawMessage `json:"content"`
	SchemaVersion  int             `json:"schemaVersion"`
	Language       string          `json:"language"`
	SubjectID      int64           `json:"subjectId"`
	ChapterID      *int64          `json:"chapterId"`
	Version        int             `json:"version"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	SubmittedAt    *time.Time      `json:"submittedAt"`
	MediaIDs       []int64         `json:"mediaIds"`
	// The review decision (null until decided) and, for approved
	// revisions, the approving reviewer.
	Review     *decisionJSON `json:"review"`
	ApprovedBy *int64        `json:"approvedBy"`
}

func toRevisionJSON(r lessons.Revision) revisionJSON {
	ids := r.MediaIDs
	if ids == nil {
		ids = []int64{}
	}
	return revisionJSON{
		ID: r.ID, LessonID: r.LessonID, Number: r.Number, Status: r.Status, AuthorID: r.AuthorID,
		BaseRevisionID: r.BaseRevisionID, Title: r.Title, Objectives: r.Objectives,
		Prerequisites: r.Prerequisites, Content: json.RawMessage(r.Content), SchemaVersion: r.SchemaVersion,
		Language: r.Language, SubjectID: r.SubjectID, ChapterID: r.ChapterID, Version: r.Version,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, SubmittedAt: r.SubmittedAt, MediaIDs: ids,
		Review: toDecisionJSON(r.Decision), ApprovedBy: r.ApprovedBy(),
	}
}

func (h *Handlers) writeRevision(w http.ResponseWriter, status int, r lessons.Revision) {
	if status == http.StatusCreated {
		w.Header().Set("Location", "/api/revisions/"+strconv.FormatInt(r.ID, 10))
	}
	writeJSON(w, status, toRevisionJSON(r))
}

// Allowed top-level fields per request body. Keys are matched exactly
// (encoding/json alone would match them case-insensitively).
var (
	fieldsKeys       = []string{"content", "prerequisites", "chapterId", "mediaIds"}
	createLessonKeys = append([]string{"subjectId", "language", "translationOf"}, fieldsKeys...)
	saveKeys         = append([]string{"expectedVersion"}, fieldsKeys...)
	submitKeys       = []string{"expectedVersion"}
)

// decodeJSON reads a strict, size-limited JSON body into v: exactly one JSON
// object, whose top-level keys are each in allowed (case-sensitive) and
// appear once, with nothing after it, and whose values follow strictDecode's
// null and type rules. It writes the error response and
// returns false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, allowed []string) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		auth.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send the request as application/json.")
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxLessonBody))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		auth.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "The request body exceeds 256 KiB.")
		return false
	case err != nil:
		badRequest(w, "The request body could not be read.")
		return false
	}
	if err := strictDecode(data, v, allowed); err != nil {
		badRequest(w, "The request body is not valid JSON for this endpoint: "+err.Error()+".")
		return false
	}
	return true
}

// containsNUL reports whether any JSON string (key or value) in data
// contains U+0000. PostgreSQL text and jsonb cannot store it.
func containsNUL(data []byte) bool {
	if !bytes.Contains(data, []byte(`\u0000`)) {
		return false // a raw NUL byte is invalid JSON and never decodes
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if s, ok := tok.(string); ok && strings.ContainsRune(s, 0) {
			return true
		}
	}
}

// checkFields rejects field values that would only fail in PostgreSQL with
// an unhelpful message.
func checkFields(w http.ResponseWriter, f fieldsJSON) bool {
	if containsNUL(f.Content) || strings.ContainsRune(f.Prerequisites, 0) {
		auth.WriteError(w, http.StatusUnprocessableEntity, "invalid_content", "The lesson is not valid: it contains a NUL character.")
		return false
	}
	return true
}

// goTypeDetail matches encoding/json type errors as rendered by the lesson
// validator, which name Go types; clients get the JSON path instead.
var goTypeDetail = regexp.MustCompile(`json: cannot unmarshal (\w+) into Go (?:struct field \w+\.(\S+)|value) of type \S+`)

func contentMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), lessons.ErrInvalidContent.Error()+": ")
	return goTypeDetail.ReplaceAllStringFunc(msg, func(m string) string {
		sub := goTypeDetail.FindStringSubmatch(m)
		if sub[2] == "" {
			return "the document must be a JSON object, not " + sub[1]
		}
		return fmt.Sprintf("%q must not be %s", sub[2], sub[1])
	})
}

func badRequest(w http.ResponseWriter, msg string) {
	auth.WriteError(w, http.StatusBadRequest, "invalid_request", msg)
}

func notFound(w http.ResponseWriter) {
	auth.WriteError(w, http.StatusNotFound, "not_found", "Not found.")
}

func (h *Handlers) createLesson(w http.ResponseWriter, r *http.Request) {
	var req createLessonRequest
	if !decodeJSON(w, r, &req, createLessonKeys) {
		return
	}
	if req.SubjectID <= 0 || req.Language == "" || !req.valid() {
		badRequest(w, "subjectId, language and content are required.")
		return
	}
	if !checkFields(w, req.fieldsJSON) || !h.checkMedia(w, r, req.MediaIDs) {
		return
	}
	nl := lessons.NewLesson{SubjectID: req.SubjectID, Language: req.Language, Fields: req.fields()}
	if req.TranslationOf != nil {
		if *req.TranslationOf <= 0 {
			h.writeLessonError(w, "create lesson", lessons.ErrNotFound, false)
			return
		}
		nl.TranslationOf = *req.TranslationOf
	}
	rev, err := h.Lessons.CreateLesson(r.Context(), actor(r), nl)
	if err != nil {
		h.writeLessonError(w, "create lesson", err, false)
		return
	}
	h.writeRevision(w, http.StatusCreated, rev)
}

func (h *Handlers) createDraft(w http.ResponseWriter, r *http.Request) {
	lessonID, ok := parseID(r.PathValue("lessonId"))
	if !ok {
		notFound(w)
		return
	}
	var req fieldsJSON
	if !decodeJSON(w, r, &req, fieldsKeys) {
		return
	}
	if !req.valid() {
		badRequest(w, "content is required.")
		return
	}
	if !checkFields(w, req) || !h.checkMedia(w, r, req.MediaIDs) {
		return
	}
	rev, err := h.Lessons.CreateDraft(r.Context(), actor(r), lessonID, req.fields())
	if err != nil {
		h.writeLessonError(w, "create draft", err, false)
		return
	}
	h.writeRevision(w, http.StatusCreated, rev)
}

func (h *Handlers) getRevision(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("revisionId"))
	if !ok {
		notFound(w)
		return
	}
	rev, err := h.Lessons.Revision(r.Context(), id)
	if err != nil {
		h.writeLessonError(w, "get revision", err, true)
		return
	}
	// Only the revision's author may read it in this slice; reviewers
	// (LRN-007) and readers (LRN-008) get their own endpoints. Anyone else
	// sees the response for a missing ID.
	if rev.AuthorID != actor(r) {
		notFound(w)
		return
	}
	h.writeRevision(w, http.StatusOK, rev)
}

func (h *Handlers) saveDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("revisionId"))
	if !ok {
		notFound(w)
		return
	}
	var req saveRequest
	if !decodeJSON(w, r, &req, saveKeys) {
		return
	}
	if req.ExpectedVersion == nil || !req.valid() {
		badRequest(w, "expectedVersion and content are required.")
		return
	}
	if !checkFields(w, req.fieldsJSON) || !h.checkMedia(w, r, req.MediaIDs) {
		return
	}
	rev, err := h.Lessons.SaveDraft(r.Context(), actor(r), id, *req.ExpectedVersion, req.fields())
	if err != nil {
		h.writeLessonError(w, "save draft", err, true)
		return
	}
	h.writeRevision(w, http.StatusOK, rev)
}

func (h *Handlers) submit(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("revisionId"))
	if !ok {
		notFound(w)
		return
	}
	var req submitRequest
	if !decodeJSON(w, r, &req, submitKeys) {
		return
	}
	if req.ExpectedVersion == nil {
		badRequest(w, "expectedVersion is required.")
		return
	}
	rev, err := h.Lessons.Submit(r.Context(), actor(r), id, *req.ExpectedVersion)
	if err != nil {
		h.writeLessonError(w, "submit", err, true)
		return
	}
	h.writeRevision(w, http.StatusOK, rev)
}

func (h *Handlers) discard(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("revisionId"))
	if !ok {
		notFound(w)
		return
	}
	q := r.URL.Query()
	raw := q.Get("expectedVersion")
	version, err := strconv.Atoi(raw)
	if len(q) != 1 || len(q["expectedVersion"]) != 1 || err != nil || raw != strconv.Itoa(version) {
		badRequest(w, "Pass the draft's version as the only query parameter: ?expectedVersion=N.")
		return
	}
	if err := h.Lessons.DiscardDraft(r.Context(), actor(r), id, version); err != nil {
		h.writeLessonError(w, "discard draft", err, true)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeLessonError maps lessons errors to the codes in docs/API.md. On
// revision endpoints (ownRevision) a denial is reported exactly like a
// missing revision: the middleware has already confirmed the author
// permission, so ErrDenied there means another author's revision (or a
// permission revoked mid-request), and revealing it would let authors probe
// revision IDs.
func (h *Handlers) writeLessonError(w http.ResponseWriter, op string, err error, ownRevision bool) {
	unprocessable := func(code, msg string) {
		auth.WriteError(w, http.StatusUnprocessableEntity, code, msg)
	}
	conflict := func(code, msg string) {
		auth.WriteError(w, http.StatusConflict, code, msg)
	}
	switch {
	case isRetryable(err):
		writeRetry(w)
	case deniedWithCause(err):
		// permissions.RequireTx joins ErrDenied with the database error
		// that prevented the check: a failure, not a denial.
		h.internal(w, op, err)
	case errors.Is(err, lessons.ErrDenied) && ownRevision, errors.Is(err, lessons.ErrNotFound) && ownRevision:
		notFound(w)
	// Another user's lesson looks exactly like a missing one, so lesson IDs
	// cannot be probed: unknown_lesson for a translation source, 404 for a
	// new draft.
	case errors.Is(err, lessons.ErrNotOwner) && op == "create lesson":
		unprocessable("unknown_lesson", "The lesson named by translationOf does not exist.")
	case errors.Is(err, lessons.ErrNotOwner):
		notFound(w)
	case errors.Is(err, lessons.ErrDenied):
		auth.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.Is(err, lessons.ErrNotFound) && op == "create lesson":
		unprocessable("unknown_lesson", "The lesson named by translationOf does not exist.")
	case errors.Is(err, lessons.ErrNotFound):
		notFound(w)
	case errors.Is(err, lessons.ErrInvalidContent):
		unprocessable("invalid_content", "The lesson is not valid: "+contentMessage(err))
	case errors.Is(err, lessons.ErrLanguageMismatch):
		unprocessable("language_mismatch", "The document's language does not match the lesson's language.")
	case errors.Is(err, lessons.ErrUnknownSubject):
		unprocessable("unknown_subject", "The subject does not exist.")
	case errors.Is(err, lessons.ErrUnknownChapter):
		unprocessable("unknown_chapter", "The chapter does not exist in this lesson's subject.")
	case errors.Is(err, lessons.ErrUnknownMedia):
		unprocessable("unknown_media", "A referenced media asset does not exist.")
	case errors.Is(err, lessons.ErrTranslationSubject):
		unprocessable("translation_subject", "A translation must have the subject of its source lesson.")
	case errors.Is(err, lessons.ErrTranslationExists):
		conflict("translation_exists", "This lesson already has a translation in that language.")
	case errors.Is(err, lessons.ErrArchived):
		conflict("archived", "The lesson is archived and accepts no changes.")
	case errors.Is(err, lessons.ErrActiveCandidateExists):
		conflict("active_candidate_exists", "The lesson already has a draft, a revision in review, or an approved revision awaiting publication.")
	case errors.Is(err, lessons.ErrNotDraft):
		conflict("not_draft", "The revision was submitted and can no longer change. Start a new draft instead.")
	case errors.Is(err, lessons.ErrVersionConflict):
		conflict("version_conflict", "The draft was changed elsewhere since it was loaded. Reload it before saving.")
	case errors.Is(err, lessons.ErrImmutable):
		conflict("immutable", "This snapshot can no longer change.")
	default:
		h.internal(w, op, err)
	}
}

// isRetryable reports a transaction that PostgreSQL aborted to resolve a
// deadlock (40P01) or a serialization failure (40001). It rolled back, so the
// request can be sent again unchanged.
func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

// deniedWithCause reports an ErrDenied carrying another cause (a database
// error from the permission check), which must not be reported as a denial.
func deniedWithCause(err error) bool {
	if !errors.Is(err, permissions.ErrDenied) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return true
	}
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		for _, e := range joined.Unwrap() {
			if !errors.Is(e, permissions.ErrDenied) {
				return true
			}
		}
	}
	return false
}

func writeRetry(w http.ResponseWriter) {
	auth.WriteError(w, http.StatusConflict, "retry",
		"The change collided with another one and was not saved. Try again.")
}

func (h *Handlers) internal(w http.ResponseWriter, op string, err error) {
	// Never log request bodies, tokens or document content.
	h.Log.Error("studio api failure", "op", op, "err", err)
	auth.WriteError(w, http.StatusInternalServerError, "internal", "Something went wrong. Try again later.")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// truncateKey bounds how much of an unknown request key is echoed back.
func truncateKey(key string) string {
	const max = 64
	if len(key) <= max {
		return key
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(key[cut]) {
		cut--
	}
	return key[:cut] + "…"
}
