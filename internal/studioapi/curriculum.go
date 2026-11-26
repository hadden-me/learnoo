package studioapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5/pgconn"
)

// Curriculum endpoints (LMS-B02, docs/LMS_CONTRACT.md §3 and §8),
// documented in docs/API.md.
//
// Reads (GET /api/catalogue/levels, …/sections/{id}/editions,
// …/editions/{id}) are open to every account with Studio access: authors,
// publishers, administrators and reviewers of any subject. Writes under
// /api/admin/curriculum/ need the global administrator permission, checked
// by the middleware before any lookup (403) and again inside the store's
// transaction; unsafe methods also need the session's CSRF token. Every
// write names the version of the level, section or edition it changes
// (expectedVersion); a mismatch is 409 version_conflict and changes nothing.
// Every response is private and never stored.

func (h *Handlers) registerCurriculum(mux *http.ServeMux) {
	chain := func(pattern string, guard func(http.Handler) http.Handler, fn http.HandlerFunc) {
		mux.Handle(pattern, h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(guard(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", privateNoStore)
				fn(w, r)
			}))))))
	}
	staff := func(next http.Handler) http.Handler {
		return h.Perms.RequireReviewerOrAny([]permissions.Permission{permissions.Author, permissions.Publisher, permissions.Administrator}, next)
	}
	admin := func(next http.Handler) http.Handler {
		return h.Perms.RequireAny([]permissions.Permission{permissions.Administrator}, next)
	}
	chain("GET /api/catalogue/levels", staff, h.listLevels)
	chain("GET /api/catalogue/sections/{sectionId}/editions", staff, h.listEditions)
	chain("GET /api/catalogue/editions/{editionId}", staff, h.getEdition)

	const base = "/api/admin/curriculum/"
	chain("POST "+base+"levels", admin, h.createLevel)
	chain("PUT "+base+"levels/{levelId}", admin, h.updateLevel)
	chain("POST "+base+"levels/{levelId}/retire", admin, h.retireLevel)
	chain("POST "+base+"levels/{levelId}/sections", admin, h.createSection)
	chain("PUT "+base+"sections/{sectionId}", admin, h.updateSection)
	chain("POST "+base+"sections/{sectionId}/retire", admin, h.retireSection)
	chain("POST "+base+"sections/{sectionId}/editions", admin, h.createEdition)
	chain("PUT "+base+"editions/{editionId}", admin, h.renameEdition)
	chain("POST "+base+"editions/{editionId}/assignments", admin, h.addAssignment)
	chain("DELETE "+base+"editions/{editionId}/assignments/{assignmentId}", admin, h.removeAssignment)
	chain("PUT "+base+"editions/{editionId}/assignments/{assignmentId}/chapters", admin, h.setChapters)
	chain("POST "+base+"editions/{editionId}/make-current", admin, h.makeCurrent)
	chain("POST "+base+"editions/{editionId}/retire", admin, h.retireEdition)
	chain("POST "+base+"subjects/{subjectId}/retire", admin, h.retireSubject)
	chain("POST "+base+"chapters/{chapterId}/retire", admin, h.retireChapter)
}

// ---- JSON ----

type sectionJSON struct {
	ID           int64      `json:"id"`
	LevelID      int64      `json:"levelId"`
	Code         string     `json:"code"`
	NameFr       string     `json:"nameFr"`
	NameAr       string     `json:"nameAr"`
	DisplayOrder int        `json:"displayOrder"`
	RetiredAt    *time.Time `json:"retiredAt"`
	Version      int        `json:"version"`
	CreatedAt    time.Time  `json:"createdAt"`
}

type levelJSON struct {
	ID           int64         `json:"id"`
	Code         string        `json:"code"`
	NameFr       string        `json:"nameFr"`
	NameAr       string        `json:"nameAr"`
	DisplayOrder int           `json:"displayOrder"`
	RetiredAt    *time.Time    `json:"retiredAt"`
	Version      int           `json:"version"`
	CreatedAt    time.Time     `json:"createdAt"`
	Sections     []sectionJSON `json:"sections"`
}

type editionSummaryJSON struct {
	ID           int64                   `json:"id"`
	SectionID    int64                   `json:"sectionId"`
	Code         string                  `json:"code"`
	LabelFr      string                  `json:"labelFr"`
	LabelAr      string                  `json:"labelAr"`
	State        curriculum.EditionState `json:"state"`
	CurrentSince *time.Time              `json:"currentSince"`
	SupersededAt *time.Time              `json:"supersededAt"`
	RetiredAt    *time.Time              `json:"retiredAt"`
	Version      int                     `json:"version"`
	CreatedAt    time.Time               `json:"createdAt"`
}

type assignedChapterJSON struct {
	ChapterID int64  `json:"chapterId"`
	Code      string `json:"code"`
	Position  int    `json:"position"`
	Retired   bool   `json:"retired"`
}

type assignmentJSON struct {
	ID          int64                 `json:"id"`
	SubjectID   int64                 `json:"subjectId"`
	SubjectCode string                `json:"subjectCode"`
	Selectable  bool                  `json:"selectable"`
	Chapters    []assignedChapterJSON `json:"chapters"`
}

type editionJSON struct {
	editionSummaryJSON
	Assignments []assignmentJSON `json:"assignments"`
}

func toSectionJSON(s curriculum.Section) sectionJSON {
	return sectionJSON{ID: s.ID, LevelID: s.LevelID, Code: s.Code, NameFr: s.NameFR, NameAr: s.NameAR,
		DisplayOrder: s.DisplayOrder, RetiredAt: s.RetiredAt, Version: s.Version, CreatedAt: s.CreatedAt}
}

func toLevelJSON(l curriculum.Level) levelJSON {
	out := levelJSON{ID: l.ID, Code: l.Code, NameFr: l.NameFR, NameAr: l.NameAR, DisplayOrder: l.DisplayOrder,
		RetiredAt: l.RetiredAt, Version: l.Version, CreatedAt: l.CreatedAt, Sections: []sectionJSON{}}
	for _, s := range l.Sections {
		out.Sections = append(out.Sections, toSectionJSON(s))
	}
	return out
}

func toEditionSummaryJSON(e curriculum.Edition) editionSummaryJSON {
	return editionSummaryJSON{ID: e.ID, SectionID: e.SectionID, Code: e.Code, LabelFr: e.LabelFR, LabelAr: e.LabelAR,
		State: e.State, CurrentSince: e.CurrentSince, SupersededAt: e.SupersededAt, RetiredAt: e.RetiredAt,
		Version: e.Version, CreatedAt: e.CreatedAt}
}

func toEditionJSON(e curriculum.Edition) editionJSON {
	out := editionJSON{editionSummaryJSON: toEditionSummaryJSON(e), Assignments: []assignmentJSON{}}
	for _, a := range e.Assignments {
		aj := assignmentJSON{ID: a.ID, SubjectID: a.SubjectID, SubjectCode: a.SubjectCode, Selectable: a.Selectable,
			Chapters: []assignedChapterJSON{}}
		for _, c := range a.Chapters {
			aj.Chapters = append(aj.Chapters, assignedChapterJSON{ChapterID: c.ChapterID, Code: c.Code, Position: c.Position, Retired: c.Retired})
		}
		out.Assignments = append(out.Assignments, aj)
	}
	return out
}

// ---- requests ----

type entryRequest struct {
	Code         *string `json:"code" api:"nonnull"`
	NameFr       *string `json:"nameFr" api:"nonnull"`
	NameAr       *string `json:"nameAr" api:"nonnull"`
	DisplayOrder *int    `json:"displayOrder" api:"nonnull"`
}

type displayRequest struct {
	ExpectedVersion *int    `json:"expectedVersion" api:"nonnull"`
	NameFr          *string `json:"nameFr" api:"nonnull"`
	NameAr          *string `json:"nameAr" api:"nonnull"`
	DisplayOrder    *int    `json:"displayOrder" api:"nonnull"`
}

type versionRequest struct {
	ExpectedVersion *int `json:"expectedVersion" api:"nonnull"`
}

type newEditionRequest struct {
	Code    *string `json:"code" api:"nonnull"`
	LabelFr *string `json:"labelFr" api:"nonnull"`
	LabelAr *string `json:"labelAr" api:"nonnull"`
}

type labelsRequest struct {
	ExpectedVersion *int    `json:"expectedVersion" api:"nonnull"`
	LabelFr         *string `json:"labelFr" api:"nonnull"`
	LabelAr         *string `json:"labelAr" api:"nonnull"`
}

type assignmentRequest struct {
	ExpectedVersion *int   `json:"expectedVersion" api:"nonnull"`
	SubjectID       *int64 `json:"subjectId" api:"nonnull"`
}

type chaptersRequest struct {
	ExpectedVersion *int    `json:"expectedVersion" api:"nonnull"`
	ChapterIDs      []int64 `json:"chapterIds"`
}

type makeCurrentRequest struct {
	ExpectedVersion *int            `json:"expectedVersion" api:"nonnull"`
	ExpectedCurrent expectedPointer `json:"expectedCurrentEditionId"`
}

type emptyRequest struct{}

var (
	entryKeys       = []string{"code", "nameFr", "nameAr", "displayOrder"}
	displayKeys     = []string{"expectedVersion", "nameFr", "nameAr", "displayOrder"}
	versionKeys     = []string{"expectedVersion"}
	newEditionKeys  = []string{"code", "labelFr", "labelAr"}
	labelsKeys      = []string{"expectedVersion", "labelFr", "labelAr"}
	assignmentKeys  = []string{"expectedVersion", "subjectId"}
	chaptersKeys    = []string{"expectedVersion", "chapterIds"}
	makeCurrentKeys = []string{"expectedVersion", "expectedCurrentEditionId"}
)

func adminActor(r *http.Request) permissions.Actor { return permissions.UserActor(actor(r)) }

// pathID parses a path ID or answers 404.
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, ok := parseID(r.PathValue(name))
	if !ok {
		notFound(w)
	}
	return id, ok
}

// ---- reads ----

func (h *Handlers) listLevels(w http.ResponseWriter, r *http.Request) {
	levels, err := h.Curriculum.Levels(r.Context())
	if err != nil {
		h.internal(w, "list levels", err)
		return
	}
	out := make([]levelJSON, len(levels))
	for i, l := range levels {
		out[i] = toLevelJSON(l)
	}
	writeJSON(w, http.StatusOK, struct {
		Levels []levelJSON `json:"levels"`
	}{out})
}

func (h *Handlers) listEditions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "sectionId")
	if !ok {
		return
	}
	sec, editions, err := h.Curriculum.Editions(r.Context(), id)
	if err != nil {
		h.writeCurriculumError(w, "list editions", err)
		return
	}
	out := make([]editionSummaryJSON, len(editions))
	for i, e := range editions {
		out[i] = toEditionSummaryJSON(e)
	}
	writeJSON(w, http.StatusOK, struct {
		Section  sectionJSON          `json:"section"`
		Editions []editionSummaryJSON `json:"editions"`
	}{toSectionJSON(sec), out})
}

func (h *Handlers) getEdition(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "editionId")
	if !ok {
		return
	}
	e, err := h.Curriculum.Edition(r.Context(), id)
	if err != nil {
		h.writeCurriculumError(w, "get edition", err)
		return
	}
	writeJSON(w, http.StatusOK, toEditionJSON(e))
}

// ---- levels and sections ----

func (r entryRequest) entry() (curriculum.Entry, bool) {
	if r.Code == nil || r.NameFr == nil || r.NameAr == nil {
		return curriculum.Entry{}, false
	}
	e := curriculum.Entry{Code: *r.Code, NameFR: *r.NameFr, NameAR: *r.NameAr}
	if r.DisplayOrder != nil {
		e.DisplayOrder = *r.DisplayOrder
	}
	return e, true
}

func (r displayRequest) display() (curriculum.Display, bool) {
	if r.ExpectedVersion == nil || r.NameFr == nil || r.NameAr == nil || r.DisplayOrder == nil {
		return curriculum.Display{}, false
	}
	return curriculum.Display{NameFR: *r.NameFr, NameAR: *r.NameAr, DisplayOrder: *r.DisplayOrder}, true
}

func (h *Handlers) createLevel(w http.ResponseWriter, r *http.Request) {
	var req entryRequest
	if !decodeJSON(w, r, &req, entryKeys) {
		return
	}
	in, ok := req.entry()
	if !ok {
		badRequest(w, "code, nameFr and nameAr are required.")
		return
	}
	l, err := h.Curriculum.CreateLevel(r.Context(), adminActor(r), in)
	if err != nil {
		h.writeCurriculumError(w, "create level", err)
		return
	}
	writeJSON(w, http.StatusCreated, toLevelJSON(l))
}

func (h *Handlers) updateLevel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "levelId")
	if !ok {
		return
	}
	var req displayRequest
	if !decodeJSON(w, r, &req, displayKeys) {
		return
	}
	d, ok := req.display()
	if !ok {
		badRequest(w, "expectedVersion, nameFr, nameAr and displayOrder are required.")
		return
	}
	l, err := h.Curriculum.UpdateLevel(r.Context(), adminActor(r), id, *req.ExpectedVersion, d)
	if err != nil {
		h.writeCurriculumError(w, "update level", err)
		return
	}
	l.Sections = nil
	writeJSON(w, http.StatusOK, toLevelJSON(l))
}

// decodeVersion reads a body holding only expectedVersion.
func decodeVersion(w http.ResponseWriter, r *http.Request) (int, bool) {
	var req versionRequest
	if !decodeJSON(w, r, &req, versionKeys) {
		return 0, false
	}
	if req.ExpectedVersion == nil {
		badRequest(w, "expectedVersion is required.")
		return 0, false
	}
	return *req.ExpectedVersion, true
}

func (h *Handlers) retireLevel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "levelId")
	if !ok {
		return
	}
	version, ok := decodeVersion(w, r)
	if !ok {
		return
	}
	l, err := h.Curriculum.RetireLevel(r.Context(), adminActor(r), id, version)
	if err != nil {
		h.writeCurriculumError(w, "retire level", err)
		return
	}
	writeJSON(w, http.StatusOK, toLevelJSON(l))
}

func (h *Handlers) createSection(w http.ResponseWriter, r *http.Request) {
	levelID, ok := pathID(w, r, "levelId")
	if !ok {
		return
	}
	var req entryRequest
	if !decodeJSON(w, r, &req, entryKeys) {
		return
	}
	in, ok := req.entry()
	if !ok {
		badRequest(w, "code, nameFr and nameAr are required.")
		return
	}
	s, err := h.Curriculum.CreateSection(r.Context(), adminActor(r), levelID, in)
	if err != nil {
		h.writeCurriculumError(w, "create section", err)
		return
	}
	writeJSON(w, http.StatusCreated, toSectionJSON(s))
}

func (h *Handlers) updateSection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "sectionId")
	if !ok {
		return
	}
	var req displayRequest
	if !decodeJSON(w, r, &req, displayKeys) {
		return
	}
	d, ok := req.display()
	if !ok {
		badRequest(w, "expectedVersion, nameFr, nameAr and displayOrder are required.")
		return
	}
	s, err := h.Curriculum.UpdateSection(r.Context(), adminActor(r), id, *req.ExpectedVersion, d)
	if err != nil {
		h.writeCurriculumError(w, "update section", err)
		return
	}
	writeJSON(w, http.StatusOK, toSectionJSON(s))
}

func (h *Handlers) retireSection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "sectionId")
	if !ok {
		return
	}
	version, ok := decodeVersion(w, r)
	if !ok {
		return
	}
	s, err := h.Curriculum.RetireSection(r.Context(), adminActor(r), id, version)
	if err != nil {
		h.writeCurriculumError(w, "retire section", err)
		return
	}
	writeJSON(w, http.StatusOK, toSectionJSON(s))
}

// ---- editions ----

func (h *Handlers) createEdition(w http.ResponseWriter, r *http.Request) {
	sectionID, ok := pathID(w, r, "sectionId")
	if !ok {
		return
	}
	var req newEditionRequest
	if !decodeJSON(w, r, &req, newEditionKeys) {
		return
	}
	if req.Code == nil || req.LabelFr == nil || req.LabelAr == nil {
		badRequest(w, "code, labelFr and labelAr are required.")
		return
	}
	e, err := h.Curriculum.CreateEdition(r.Context(), adminActor(r), sectionID,
		curriculum.NewEdition{Code: *req.Code, LabelFR: *req.LabelFr, LabelAR: *req.LabelAr})
	if err != nil {
		h.writeCurriculumError(w, "create edition", err)
		return
	}
	writeJSON(w, http.StatusCreated, toEditionJSON(e))
}

func (h *Handlers) renameEdition(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "editionId")
	if !ok {
		return
	}
	var req labelsRequest
	if !decodeJSON(w, r, &req, labelsKeys) {
		return
	}
	if req.ExpectedVersion == nil || req.LabelFr == nil || req.LabelAr == nil {
		badRequest(w, "expectedVersion, labelFr and labelAr are required.")
		return
	}
	e, err := h.Curriculum.RenameEdition(r.Context(), adminActor(r), id, *req.ExpectedVersion, *req.LabelFr, *req.LabelAr)
	if err != nil {
		h.writeCurriculumError(w, "rename edition", err)
		return
	}
	writeJSON(w, http.StatusOK, toEditionJSON(e))
}

func (h *Handlers) addAssignment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "editionId")
	if !ok {
		return
	}
	var req assignmentRequest
	if !decodeJSON(w, r, &req, assignmentKeys) {
		return
	}
	if req.ExpectedVersion == nil || req.SubjectID == nil {
		badRequest(w, "expectedVersion and subjectId are required.")
		return
	}
	if *req.SubjectID <= 0 {
		h.writeCurriculumError(w, "add assignment", curriculum.ErrUnknownSubject)
		return
	}
	e, _, err := h.Curriculum.AddAssignment(r.Context(), adminActor(r), id, *req.ExpectedVersion, *req.SubjectID)
	if err != nil {
		h.writeCurriculumError(w, "add assignment", err)
		return
	}
	writeJSON(w, http.StatusCreated, toEditionJSON(e))
}

func (h *Handlers) removeAssignment(w http.ResponseWriter, r *http.Request) {
	editionID, ok := pathID(w, r, "editionId")
	if !ok {
		return
	}
	assignmentID, ok := pathID(w, r, "assignmentId")
	if !ok {
		return
	}
	// The request carries no body: the version is the only input.
	if n, _ := r.Body.Read(make([]byte, 1)); n > 0 || r.ContentLength > 0 {
		badRequest(w, "This request takes no body; pass the edition's version as ?expectedVersion=N.")
		return
	}
	q := r.URL.Query()
	raw := q.Get("expectedVersion")
	version, err := strconv.Atoi(raw)
	if len(q) != 1 || len(q["expectedVersion"]) != 1 || err != nil || raw != strconv.Itoa(version) {
		badRequest(w, "Pass the edition's version as the only query parameter: ?expectedVersion=N.")
		return
	}
	e, err := h.Curriculum.RemoveAssignment(r.Context(), adminActor(r), editionID, assignmentID, version)
	if err != nil {
		h.writeCurriculumError(w, "remove assignment", err)
		return
	}
	writeJSON(w, http.StatusOK, toEditionJSON(e))
}

func (h *Handlers) setChapters(w http.ResponseWriter, r *http.Request) {
	editionID, ok := pathID(w, r, "editionId")
	if !ok {
		return
	}
	assignmentID, ok := pathID(w, r, "assignmentId")
	if !ok {
		return
	}
	var req chaptersRequest
	if !decodeJSON(w, r, &req, chaptersKeys) {
		return
	}
	// An empty list decodes to an empty slice; nil means the key is missing.
	if req.ExpectedVersion == nil || req.ChapterIDs == nil {
		badRequest(w, "expectedVersion and chapterIds are required.")
		return
	}
	e, err := h.Curriculum.SetChapters(r.Context(), adminActor(r), editionID, assignmentID, *req.ExpectedVersion, req.ChapterIDs)
	if err != nil {
		h.writeCurriculumError(w, "set chapters", err)
		return
	}
	writeJSON(w, http.StatusOK, toEditionJSON(e))
}

func (h *Handlers) makeCurrent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "editionId")
	if !ok {
		return
	}
	var req makeCurrentRequest
	if !decodeJSON(w, r, &req, makeCurrentKeys) {
		return
	}
	if req.ExpectedVersion == nil || !req.ExpectedCurrent.valid(true) {
		badRequest(w, "expectedVersion and expectedCurrentEditionId (the section's current edition ID, or null if it has none) are required.")
		return
	}
	e, superseded, err := h.Curriculum.MakeCurrent(r.Context(), adminActor(r), id, *req.ExpectedVersion, req.ExpectedCurrent.id)
	if err != nil {
		h.writeCurriculumError(w, "make current", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Edition             editionJSON `json:"edition"`
		SupersededEditionID *int64      `json:"supersededEditionId"`
	}{toEditionJSON(e), superseded})
}

func (h *Handlers) retireEdition(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "editionId")
	if !ok {
		return
	}
	version, ok := decodeVersion(w, r)
	if !ok {
		return
	}
	e, err := h.Curriculum.RetireEdition(r.Context(), adminActor(r), id, version)
	if err != nil {
		h.writeCurriculumError(w, "retire edition", err)
		return
	}
	writeJSON(w, http.StatusOK, toEditionJSON(e))
}

// ---- subjects and chapters ----

func (h *Handlers) retireSubject(w http.ResponseWriter, r *http.Request) {
	h.retireCatalogueEntry(w, r, "subjectId", h.Curriculum.RetireSubject)
}

func (h *Handlers) retireChapter(w http.ResponseWriter, r *http.Request) {
	h.retireCatalogueEntry(w, r, "chapterId", h.Curriculum.RetireChapter)
}

func (h *Handlers) retireCatalogueEntry(w http.ResponseWriter, r *http.Request, param string,
	retire func(context.Context, permissions.Actor, int64) error) {
	id, ok := pathID(w, r, param)
	if !ok {
		return
	}
	var req emptyRequest
	if !decodeJSON(w, r, &req, nil) {
		return
	}
	if err := retire(r.Context(), adminActor(r), id); err != nil {
		h.writeCurriculumError(w, "retire "+strings.TrimSuffix(param, "Id"), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeCurriculumError maps curriculum errors to the codes in docs/API.md.
func (h *Handlers) writeCurriculumError(w http.ResponseWriter, op string, err error) {
	conflict := func(code, msg string) { auth.WriteError(w, http.StatusConflict, code, msg) }
	unprocessable := func(code, msg string) { auth.WriteError(w, http.StatusUnprocessableEntity, code, msg) }
	var pgErr *pgconn.PgError
	switch {
	case isRetryable(err):
		writeRetry(w)
	case deniedWithCause(err):
		h.internal(w, op, err)
	case errors.Is(err, permissions.ErrDenied):
		auth.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.Is(err, curriculum.ErrNotFound):
		notFound(w)
	case errors.Is(err, curriculum.ErrInvalidInput):
		unprocessable("invalid_input", detail(err, curriculum.ErrInvalidInput, "A code, name or display order is not valid."))
	case errors.Is(err, curriculum.ErrPlacementInvalid):
		unprocessable("placement_invalid", detail(err, curriculum.ErrPlacementInvalid, "The selection is not allowed by the curriculum rules."))
	case errors.Is(err, curriculum.ErrUnknownSubject):
		unprocessable("unknown_subject", "The subject does not exist.")
	case errors.Is(err, curriculum.ErrCodeExists):
		conflict("code_exists", "This code is already used here. Codes are never reused.")
	case errors.Is(err, curriculum.ErrVersionConflict):
		conflict("version_conflict", "This entry changed since it was loaded. Reload it before changing it.")
	case errors.Is(err, curriculum.ErrEditionNotDraft):
		conflict("edition_not_draft", "The edition is no longer a draft; its assignments and chapters can no longer change. Create a new edition instead.")
	case errors.Is(err, curriculum.ErrStateConflict):
		conflict("state_conflict", "The edition cannot make this state change now (for example, the section's current edition changed). Reload and try again.")
	case errors.Is(err, curriculum.ErrAlreadyRetired):
		conflict("already_retired", "This entry is already retired.")
	case errors.Is(err, curriculum.ErrParentRetired):
		conflict("parent_retired", "The level or section is retired; nothing new can be placed under it.")
	case errors.Is(err, curriculum.ErrAssignmentExists):
		conflict("assignment_exists", "The edition already assigns this subject.")
	case errors.As(err, &pgErr) && pgErr.Code == "LRN01":
		conflict("immutable", "A database guard refused the change. Reload and try again.")
	default:
		h.internal(w, op, err)
	}
}

// detail returns the text an error adds after its sentinel, or fallback.
func detail(err, sentinel error, fallback string) string {
	msg := strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
	if msg == err.Error() || msg == "" {
		return fallback
	}
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}
