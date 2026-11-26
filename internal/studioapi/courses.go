package studioapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// Course draft endpoints (LMS-B03, docs/LMS_CONTRACT.md §4 and §8),
// documented in docs/API.md.
//
// Every route needs a session, the CSRF token on unsafe methods and the
// global author permission, checked by the middleware before any lookup and
// again inside the store's transaction. Courses belong to their owner:
// another user's course, revision or unpublished lesson gets the same 404 as
// an ID that does not exist. Every response is private and never stored.

func (h *Handlers) registerCourses(mux *http.ServeMux) {
	route := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(
			h.Perms.Require(permissions.Author, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", privateNoStore)
				fn(w, r)
			}))))))
	}
	route("GET /api/courses", h.listCourses)
	route("POST /api/courses", h.createCourse)
	route("DELETE /api/courses/{courseId}", h.deleteCourse)
	route("POST /api/courses/{courseId}/drafts", h.createCourseDraft)
	route("GET /api/courses/{courseId}/attachable-lessons", h.attachableLessons)
	route("GET /api/course-revisions/{revisionId}", h.getCourseRevision)
	route("PUT /api/course-revisions/{revisionId}", h.saveCourseDraft)
	route("DELETE /api/course-revisions/{revisionId}", h.discardCourseDraft)
	route("GET /api/lessons/{lessonId}/eligible-revisions", h.eligibleRevisions)
}

// ---- JSON ----

type courseRevisionSummaryJSON struct {
	ID        int64          `json:"id"`
	Number    int            `json:"number"`
	Status    courses.Status `json:"status"`
	Title     string         `json:"title"`
	Version   int            `json:"version"`
	UpdatedAt time.Time      `json:"updatedAt"`
	Withdrawn bool           `json:"withdrawn"`
}

type courseJSON struct {
	ID                  int64                      `json:"id"`
	PublicID            string                     `json:"publicId"`
	OwnerID             int64                      `json:"ownerId"`
	SubjectID           int64                      `json:"subjectId"`
	Language            string                     `json:"language"`
	PublishedRevisionID *int64                     `json:"publishedRevisionId"`
	Archived            bool                       `json:"archived"`
	Suspended           bool                       `json:"suspended"`
	CreatedAt           time.Time                  `json:"createdAt"`
	LatestRevision      *courseRevisionSummaryJSON `json:"latestRevision,omitempty"`
}

type placementJSON struct {
	AssignmentID int64  `json:"assignmentId"`
	EditionID    int64  `json:"editionId"`
	EditionState string `json:"editionState"`
	Selectable   bool   `json:"selectable"`
	Retired      bool   `json:"retired"`
}

type stepJSON struct {
	Position       int    `json:"position"`
	Title          string `json:"title"`
	LessonID       int64  `json:"lessonId"`
	RevisionID     int64  `json:"revisionId"`
	RevisionNumber int    `json:"revisionNumber"`
	OwnLesson      bool   `json:"ownLesson"`
	Eligible       bool   `json:"eligible"`
}

type moduleJSON struct {
	Key       string     `json:"key"`
	Position  int        `json:"position"`
	Title     string     `json:"title"`
	ChapterID *int64     `json:"chapterId"`
	Steps     []stepJSON `json:"steps"`
}

type courseRevisionJSON struct {
	ID                int64          `json:"id"`
	CourseID          int64          `json:"courseId"`
	Number            int            `json:"number"`
	Status            courses.Status `json:"status"`
	Kind              string         `json:"kind"`
	AuthorID          int64          `json:"authorId"`
	BaseRevisionID    *int64         `json:"baseRevisionId"`
	CompareRevisionID *int64         `json:"compareRevisionId"`
	SubjectID         int64          `json:"subjectId"`
	Language          string         `json:"language"`
	Title             string         `json:"title"`
	Summary           string         `json:"summary"`
	Objectives        string         `json:"objectives"`
	Prerequisites     string         `json:"prerequisites"`
	CoverMediaID      *int64         `json:"coverMediaId"`
	CoverAlt          *string        `json:"coverAlt"`
	Version           int            `json:"version"`
	CreatedAt         time.Time      `json:"createdAt"`
	UpdatedAt         time.Time      `json:"updatedAt"`
	SubmittedAt       *time.Time     `json:"submittedAt"`
	WithdrawnAt       *time.Time     `json:"withdrawnAt"`
	// The review decision (null until decided) and, for a revision in
	// review that no active reviewer may decide, why (LMS-B04).
	Review      *courseDecisionJSON `json:"review"`
	Blocked     *blockedJSON        `json:"blocked"`
	Assignments []placementJSON     `json:"assignments"`
	Modules     []moduleJSON        `json:"modules"`
	// Only on POST /api/courses/{id}/drafts.
	DroppedSteps *[]droppedStepJSON `json:"droppedSteps,omitempty"`
}

type droppedStepJSON struct {
	ModuleKey  string `json:"moduleKey"`
	Position   int    `json:"position"`
	Title      string `json:"title"`
	LessonID   int64  `json:"lessonId"`
	RevisionID int64  `json:"revisionId"`
}

func toCourseJSON(c courses.Course) courseJSON {
	return courseJSON{ID: c.ID, PublicID: c.PublicID, OwnerID: c.OwnerID, SubjectID: c.SubjectID, Language: c.Language,
		PublishedRevisionID: c.PublishedRevisionID, Archived: c.ArchivedAt != nil, Suspended: c.Suspended, CreatedAt: c.CreatedAt}
}

func toCourseRevisionJSON(r courses.Revision) courseRevisionJSON {
	out := courseRevisionJSON{ID: r.ID, CourseID: r.CourseID, Number: r.Number, Status: r.Status, Kind: r.Kind,
		AuthorID: r.AuthorID, BaseRevisionID: r.BaseRevisionID, CompareRevisionID: r.CompareRevisionID,
		SubjectID: r.SubjectID, Language: r.Language, Title: r.Title, Summary: r.Summary, Objectives: r.Objectives,
		Prerequisites: r.Prerequisites, CoverMediaID: r.CoverMediaID, CoverAlt: r.CoverAlt, Version: r.Version,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, SubmittedAt: r.SubmittedAt, WithdrawnAt: r.WithdrawnAt,
		Review: toCourseDecisionJSON(r.Decision), Blocked: toBlockedJSON(r.Blocked),
		Assignments: []placementJSON{}, Modules: []moduleJSON{}}
	for _, p := range r.Assignments {
		out.Assignments = append(out.Assignments, placementJSON{AssignmentID: p.AssignmentID, EditionID: p.EditionID,
			EditionState: p.EditionState, Selectable: p.Selectable, Retired: p.Retired})
	}
	for _, m := range r.Modules {
		mj := moduleJSON{Key: m.Key, Position: m.Position, Title: m.Title, ChapterID: m.ChapterID, Steps: []stepJSON{}}
		for _, s := range m.Steps {
			mj.Steps = append(mj.Steps, stepJSON{Position: s.Position, Title: s.Title, LessonID: s.LessonID,
				RevisionID: s.RevisionID, RevisionNumber: s.RevisionNumber, OwnLesson: s.OwnLesson, Eligible: s.Eligible})
		}
		out.Modules = append(out.Modules, mj)
	}
	return out
}

func writeCourseRevision(w http.ResponseWriter, status int, r courses.Revision) {
	if status == http.StatusCreated {
		w.Header().Set("Location", "/api/course-revisions/"+strconv.FormatInt(r.ID, 10))
	}
	writeJSON(w, status, toCourseRevisionJSON(r))
}

// ---- requests ----

type createCourseRequest struct {
	SubjectID *int64  `json:"subjectId" api:"nonnull"`
	Language  *string `json:"language" api:"nonnull"`
}

type saveCourseRequest struct {
	ExpectedVersion *int            `json:"expectedVersion" api:"nonnull"`
	Title           *string         `json:"title" api:"nonnull"`
	Summary         *string         `json:"summary" api:"nonnull"`
	Objectives      *string         `json:"objectives" api:"nonnull"`
	Prerequisites   *string         `json:"prerequisites" api:"nonnull"`
	CoverMediaID    *int64          `json:"coverMediaId"`
	CoverAlt        *string         `json:"coverAlt"`
	AssignmentIDs   []int64         `json:"assignmentIds"`
	Modules         json.RawMessage `json:"modules"`
}

type moduleRequest struct {
	Key       *string         `json:"key"`
	Title     *string         `json:"title" api:"nonnull"`
	ChapterID *int64          `json:"chapterId"`
	Steps     json.RawMessage `json:"steps"`
}

type stepRequest struct {
	LessonID   *int64  `json:"lessonId" api:"nonnull"`
	RevisionID *int64  `json:"revisionId" api:"nonnull"`
	Title      *string `json:"title" api:"nonnull"`
}

var (
	createCourseKeys = []string{"subjectId", "language"}
	saveCourseKeys   = []string{"expectedVersion", "title", "summary", "objectives", "prerequisites", "coverMediaId", "coverAlt", "assignmentIds", "modules"}
	moduleKeys       = []string{"key", "title", "chapterId", "steps"}
	stepKeys         = []string{"lessonId", "revisionId", "title"}
)

// objectList splits a JSON array of objects, refusing anything else
// (null, a non-array, non-object elements).
func objectList(raw json.RawMessage, name string, max int) ([]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, fmt.Errorf("%q must be an array of objects", name)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%q must be an array of objects", name)
	}
	if len(items) > max {
		return nil, fmt.Errorf("%q has more than %d entries", name, max)
	}
	for _, it := range items {
		if t := bytes.TrimSpace(it); len(t) == 0 || t[0] != '{' {
			return nil, fmt.Errorf("%q must be an array of objects", name)
		}
	}
	return items, nil
}

// draft converts a decoded save request, decoding modules and steps with
// the strict rules of the top level. Missing required keys are reported.
func (req saveCourseRequest) draft() (courses.Draft, error) {
	if req.ExpectedVersion == nil || req.Title == nil || req.Summary == nil || req.Objectives == nil ||
		req.Prerequisites == nil || req.AssignmentIDs == nil || req.Modules == nil {
		return courses.Draft{}, errors.New("expectedVersion, title, summary, objectives, prerequisites, assignmentIds and modules are required")
	}
	d := courses.Draft{Title: *req.Title, Summary: *req.Summary, Objectives: *req.Objectives, Prerequisites: *req.Prerequisites,
		CoverMediaID: req.CoverMediaID, CoverAlt: req.CoverAlt, AssignmentIDs: req.AssignmentIDs}
	modules, err := objectList(req.Modules, "modules", courses.MaxModules)
	if err != nil {
		return d, err
	}
	steps := 0
	for i, raw := range modules {
		var mr moduleRequest
		if err := strictDecode(raw, &mr, moduleKeys); err != nil {
			return d, fmt.Errorf("modules[%d]: %v", i, err)
		}
		if mr.Title == nil || mr.Steps == nil {
			return d, fmt.Errorf("modules[%d]: title and steps are required", i)
		}
		m := courses.ModuleInput{Title: *mr.Title, ChapterID: mr.ChapterID}
		if mr.Key != nil {
			m.Key = *mr.Key
			if m.Key == "" {
				return d, fmt.Errorf("modules[%d]: key must be a UUID or null", i)
			}
		}
		items, err := objectList(mr.Steps, fmt.Sprintf("modules[%d].steps", i), courses.MaxSteps)
		if err != nil {
			return d, err
		}
		for j, sraw := range items {
			steps++
			if steps > courses.MaxSteps {
				return d, fmt.Errorf("a revision has at most %d steps", courses.MaxSteps)
			}
			var sr stepRequest
			if err := strictDecode(sraw, &sr, stepKeys); err != nil {
				return d, fmt.Errorf("modules[%d].steps[%d]: %v", i, j, err)
			}
			if sr.LessonID == nil || sr.RevisionID == nil || sr.Title == nil {
				return d, fmt.Errorf("modules[%d].steps[%d]: lessonId, revisionId and title are required", i, j)
			}
			m.Steps = append(m.Steps, courses.StepInput{LessonID: *sr.LessonID, RevisionID: *sr.RevisionID, Title: *sr.Title})
		}
		d.Modules = append(d.Modules, m)
	}
	return d, nil
}

// ---- handlers ----

func (h *Handlers) listCourses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var before int64
	ok := len(q["mine"]) == 1 && q.Get("mine") == "1"
	for key, vals := range q {
		if (key != "mine" && key != "cursor") || len(vals) != 1 {
			ok = false
		}
	}
	if raw, present := q["cursor"]; ok && present {
		before, ok = parseID(raw[0])
	}
	if !ok {
		badRequest(w, "Pass ?mine=1 (only the caller's own courses can be listed) and optionally the nextCursor of a previous page as cursor.")
		return
	}
	list, next, err := h.Courses.Owned(r.Context(), actor(r), before)
	if err != nil {
		h.writeCourseError(w, "list courses", err)
		return
	}
	out := make([]courseJSON, len(list))
	for i, c := range list {
		out[i] = toCourseJSON(c.Course)
		if c.Latest != nil {
			l := c.Latest
			out[i].LatestRevision = &courseRevisionSummaryJSON{ID: l.ID, Number: l.Number, Status: l.Status, Title: l.Title,
				Version: l.Version, UpdatedAt: l.UpdatedAt, Withdrawn: l.Withdrawn}
		}
	}
	var cursor *string
	if next != nil {
		c := strconv.FormatInt(*next, 10)
		cursor = &c
	}
	writeJSON(w, http.StatusOK, struct {
		Courses    []courseJSON `json:"courses"`
		NextCursor *string      `json:"nextCursor"`
	}{out, cursor})
}

func (h *Handlers) createCourse(w http.ResponseWriter, r *http.Request) {
	var req createCourseRequest
	if !decodeJSON(w, r, &req, createCourseKeys) {
		return
	}
	if req.SubjectID == nil || req.Language == nil {
		badRequest(w, "subjectId and language are required.")
		return
	}
	c, rev, err := h.Courses.CreateCourse(r.Context(), actor(r), *req.SubjectID, *req.Language)
	if err != nil {
		h.writeCourseError(w, "create course", err)
		return
	}
	w.Header().Set("Location", "/api/course-revisions/"+strconv.FormatInt(rev.ID, 10))
	writeJSON(w, http.StatusCreated, struct {
		Course   courseJSON         `json:"course"`
		Revision courseRevisionJSON `json:"revision"`
	}{toCourseJSON(c), toCourseRevisionJSON(rev)})
}

func (h *Handlers) deleteCourse(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "courseId")
	if !ok || !noQuery(w, r) {
		return
	}
	if err := h.Courses.DeleteCourse(r.Context(), actor(r), id); err != nil {
		h.writeCourseError(w, "delete course", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) createCourseDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "courseId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req emptyRequest
	if !decodeJSON(w, r, &req, nil) {
		return
	}
	rev, err := h.Courses.CreateDraft(r.Context(), actor(r), id)
	if err != nil {
		h.writeCourseError(w, "create course draft", err)
		return
	}
	out := toCourseRevisionJSON(rev)
	dropped := []droppedStepJSON{}
	out.DroppedSteps = &dropped
	for _, d := range rev.DroppedSteps {
		dropped = append(dropped, droppedStepJSON{ModuleKey: d.ModuleKey, Position: d.Position, Title: d.Title,
			LessonID: d.LessonID, RevisionID: d.RevisionID})
	}
	w.Header().Set("Location", "/api/course-revisions/"+strconv.FormatInt(rev.ID, 10))
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) getCourseRevision(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "revisionId")
	if !ok || !noQuery(w, r) {
		return
	}
	rev, err := h.Courses.Revision(r.Context(), actor(r), id)
	if err != nil {
		h.writeCourseError(w, "get course revision", err)
		return
	}
	writeCourseRevision(w, http.StatusOK, rev)
}

func (h *Handlers) saveCourseDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "revisionId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req saveCourseRequest
	if !decodeJSON(w, r, &req, saveCourseKeys) {
		return
	}
	d, err := req.draft()
	if err != nil {
		badRequest(w, "The request body is not valid JSON for this endpoint: "+err.Error()+".")
		return
	}
	rev, err := h.Courses.SaveDraft(r.Context(), actor(r), id, *req.ExpectedVersion, d)
	if err != nil {
		h.writeCourseError(w, "save course draft", err)
		return
	}
	writeCourseRevision(w, http.StatusOK, rev)
}

func (h *Handlers) discardCourseDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "revisionId")
	if !ok {
		return
	}
	q := r.URL.Query()
	raw := q.Get("expectedVersion")
	version, err := strconv.Atoi(raw)
	if len(q) != 1 || len(q["expectedVersion"]) != 1 || err != nil || raw != strconv.Itoa(version) {
		badRequest(w, "Pass the draft's version as the only query parameter: ?expectedVersion=N.")
		return
	}
	if err := h.Courses.DiscardDraft(r.Context(), actor(r), id, version); err != nil {
		h.writeCourseError(w, "discard course draft", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type attachableLessonJSON struct {
	LessonID            int64                      `json:"lessonId"`
	PublicID            string                     `json:"publicId"`
	Own                 bool                       `json:"own"`
	Title               string                     `json:"title"`
	PublishedRevisionID *int64                     `json:"publishedRevisionId"`
	LatestRevision      *courseRevisionSummaryJSON `json:"latestRevision"`
}

func (h *Handlers) attachableLessons(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "courseId")
	if !ok {
		return
	}
	q := r.URL.Query()
	var after int64
	for key, vals := range q {
		if (key != "q" && key != "cursor") || len(vals) != 1 {
			badRequest(w, "Only q and cursor are accepted, once each.")
			return
		}
	}
	if raw, present := q["cursor"]; present {
		c, ok := parseID(raw[0])
		if !ok {
			badRequest(w, "cursor must be the nextCursor of a previous page.")
			return
		}
		after = c
	}
	list, next, err := h.Courses.AttachableLessons(r.Context(), actor(r), id, q.Get("q"), after)
	if err != nil {
		h.writeCourseError(w, "attachable lessons", err)
		return
	}
	out := make([]attachableLessonJSON, len(list))
	for i, l := range list {
		out[i] = attachableLessonJSON{LessonID: l.LessonID, PublicID: l.PublicID, Own: l.Own, Title: l.Title,
			PublishedRevisionID: l.PublishedRevisionID}
		if l.Latest != nil {
			out[i].LatestRevision = &courseRevisionSummaryJSON{ID: l.Latest.ID, Number: l.Latest.Number, Status: l.Latest.Status,
				Title: l.Latest.Title, Version: l.Latest.Version, UpdatedAt: l.Latest.UpdatedAt}
		}
	}
	var cursor *string
	if next != nil {
		s := strconv.FormatInt(*next, 10)
		cursor = &s
	}
	writeJSON(w, http.StatusOK, struct {
		Lessons    []attachableLessonJSON `json:"lessons"`
		NextCursor *string                `json:"nextCursor"`
	}{out, cursor})
}

type pinnableRevisionJSON struct {
	ID       int64          `json:"id"`
	Number   int            `json:"number"`
	Status   courses.Status `json:"status"`
	Title    string         `json:"title"`
	Eligible bool           `json:"eligible"`
}

func (h *Handlers) eligibleRevisions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "lessonId")
	if !ok || !noQuery(w, r) {
		return
	}
	list, err := h.Courses.EligibleRevisions(r.Context(), actor(r), id)
	if err != nil {
		h.writeCourseError(w, "eligible revisions", err)
		return
	}
	out := make([]pinnableRevisionJSON, len(list))
	for i, lr := range list {
		out[i] = pinnableRevisionJSON{ID: lr.ID, Number: lr.Number, Status: lr.Status, Title: lr.Title, Eligible: lr.Eligible}
	}
	writeJSON(w, http.StatusOK, struct {
		Revisions []pinnableRevisionJSON `json:"revisions"`
	}{out})
}

// writeCourseError maps courses errors to the codes in docs/API.md.
func (h *Handlers) writeCourseError(w http.ResponseWriter, op string, err error) {
	conflict := func(code, msg string) { auth.WriteError(w, http.StatusConflict, code, msg) }
	unprocessable := func(code, msg string) { auth.WriteError(w, http.StatusUnprocessableEntity, code, msg) }
	switch {
	case isRetryable(err), errors.Is(err, courses.ErrRetry):
		writeRetry(w)
	case deniedWithCause(err):
		h.internal(w, op, err)
	case errors.Is(err, permissions.ErrDenied):
		auth.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.Is(err, courses.ErrNotFound):
		notFound(w)
	case writeCourseWorkflowError(w, err):
	case errors.Is(err, courses.ErrInvalidStep):
		unprocessable("invalid_step", detail(err, courses.ErrInvalidStep, "A step cannot be attached to this course."))
	case errors.Is(err, courses.ErrPlacementInvalid):
		unprocessable("placement_invalid", detail(err, courses.ErrPlacementInvalid, "The placement is not allowed by the curriculum rules."))
	case errors.Is(err, courses.ErrInvalidInput):
		unprocessable("invalid_input", detail(err, courses.ErrInvalidInput, "A field is not valid."))
	case errors.Is(err, courses.ErrUnknownSubject):
		unprocessable("unknown_subject", "The subject does not exist.")
	case errors.Is(err, courses.ErrUnknownMedia):
		unprocessable("unknown_media", "The cover must be an image you uploaded.")
	case errors.Is(err, courses.ErrVersionConflict):
		conflict("version_conflict", "The draft was changed elsewhere since it was loaded. Reload it before saving.")
	case errors.Is(err, courses.ErrActiveCandidateExists):
		conflict("active_candidate_exists", "The course already has a draft, a revision in review, or an approved revision awaiting publication.")
	case errors.Is(err, courses.ErrNotDraft):
		conflict("not_draft", "The revision was submitted and can no longer change.")
	case errors.Is(err, courses.ErrHasHistory):
		conflict("has_history", "A revision of this course was submitted, so the course is kept. Discard the open draft or request archival instead.")
	case errors.Is(err, courses.ErrArchived):
		conflict("archived", "The course is archived and accepts no new drafts.")
	case errors.Is(err, courses.ErrImmutable):
		conflict("immutable", "A database guard refused the change. Reload and try again.")
	default:
		h.internal(w, op, err)
	}
}
