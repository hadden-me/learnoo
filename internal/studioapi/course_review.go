package studioapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/permissions"
)

// Course submission, review, amendments, publication and lifecycle
// endpoints (LMS-B04, docs/LMS_CONTRACT.md §6-§8), documented in
// docs/API.md.
//
// Every route needs a session and the CSRF token on unsafe methods, then a
// role checked by the middleware before any lookup and again inside the
// store's transaction: author (submit, amendments, archive request),
// reviewer of any subject (decisions, queue; the revision's subject is
// checked in the transaction and a reviewer of another subject gets 404),
// publisher (publication), publisher or administrator (restore),
// administrator (suspend, archive, unarchive, transfer, withdrawal, draft
// discard, archive requests, blocked reviews). Every response is private
// and never stored.

func (h *Handlers) registerCourseReview(mux *http.ServeMux) {
	chain := func(pattern string, guard func(http.Handler) http.Handler, fn http.HandlerFunc) {
		mux.Handle(pattern, h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(guard(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", privateNoStore)
				fn(w, r)
			}))))))
	}
	anyOf := func(ps ...permissions.Permission) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler { return h.Perms.RequireAny(ps, next) }
	}
	reviewer := func(next http.Handler) http.Handler { return h.Perms.RequireAnyReviewer(next) }
	reviewerOr := func(ps ...permissions.Permission) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler { return h.Perms.RequireReviewerOrAny(ps, next) }
	}
	author, publisher, admin := anyOf(permissions.Author), anyOf(permissions.Publisher), anyOf(permissions.Administrator)

	chain("POST /api/course-revisions/{revisionId}/submit", author, h.submitCourse)
	chain("POST /api/courses/{courseId}/amendments", author, h.createAmendment)
	chain("POST /api/courses/{courseId}/archive-request", author, h.requestArchive)
	chain("GET /api/courses/{courseId}/newer-revisions",
		reviewerOr(permissions.Author, permissions.Publisher, permissions.Administrator), h.newerRevisions)

	chain("GET /api/review/course-revisions/{revisionId}",
		reviewerOr(permissions.Publisher, permissions.Administrator), h.reviewCourseRevision)
	chain("POST /api/review/course-revisions/{revisionId}/approve", reviewer, h.approveCourse)
	chain("POST /api/review/course-revisions/{revisionId}/request-changes", reviewer, h.requestCourseChanges)

	chain("POST /api/course-revisions/{revisionId}/publish", publisher, h.publishCourse)
	chain("POST /api/courses/{courseId}/restore", anyOf(permissions.Publisher, permissions.Administrator), h.restoreCourse)

	chain("POST /api/courses/{courseId}/suspend", admin, h.suspendCourse)
	chain("POST /api/courses/{courseId}/archive", admin, h.archiveCourse)
	chain("POST /api/courses/{courseId}/unarchive", admin, h.unarchiveCourse)
	chain("POST /api/courses/{courseId}/transfer", admin, h.transferCourse)
	chain("POST /api/course-revisions/{revisionId}/withdraw", admin, h.withdrawCourseRevision)
	chain("DELETE /api/admin/course-revisions/{revisionId}", admin, h.adminDiscardCourseDraft)
	chain("GET /api/admin/courses/archive-requests", admin, h.archiveRequests)
	chain("GET /api/admin/courses/blocked-reviews", admin, h.blockedReviews)
}

// ---- JSON ----

type reviewerProblemJSON struct {
	ReviewerID       *int64  `json:"reviewerId"`
	ReviewerName     *string `json:"reviewerName"`
	Reason           string  `json:"reason"`
	LessonID         *int64  `json:"lessonId"`
	LessonRevisionID *int64  `json:"lessonRevisionId"`
}

func toProblemsJSON(ps []courses.ReviewerProblem, names bool) []reviewerProblemJSON {
	out := make([]reviewerProblemJSON, 0, len(ps))
	for _, p := range ps {
		j := reviewerProblemJSON{Reason: p.Reason, LessonID: p.LessonID, LessonRevisionID: p.LessonRevisionID}
		if p.ReviewerID != 0 {
			id := p.ReviewerID
			j.ReviewerID = &id
			if names {
				n := p.ReviewerName
				j.ReviewerName = &n
			}
		}
		out = append(out, j)
	}
	return out
}

type blockedJSON struct {
	Reason  string                `json:"reason"`
	Details []reviewerProblemJSON `json:"details"`
}

func toBlockedJSON(b *courses.Blocked) *blockedJSON {
	if b == nil {
		return nil
	}
	return &blockedJSON{Reason: "no_eligible_reviewer", Details: toProblemsJSON(b.Reasons, true)}
}

func toCourseDecisionJSON(d *courses.Decision) *decisionJSON {
	if d == nil {
		return nil
	}
	return &decisionJSON{Action: d.Action, ReviewerID: d.ReviewerID, Comment: d.Comment, DecidedAt: d.DecidedAt}
}

type ineligibleStepJSON struct {
	ModuleKey  string `json:"moduleKey"`
	Position   int    `json:"position"`
	Title      string `json:"title"`
	LessonID   int64  `json:"lessonId"`
	RevisionID int64  `json:"revisionId"`
}

// writeErrorWith writes the standard error object with one more key.
func writeErrorWith(w http.ResponseWriter, status int, code, message, key string, value any) {
	body := map[string]any{"code": code, "message": message, key: value}
	writeJSON(w, status, map[string]any{"error": body})
}

type coursePointerJSON struct {
	CourseID            int64     `json:"courseId"`
	Action              string    `json:"action"`
	PublishedRevisionID *int64    `json:"publishedRevisionId"`
	PreviousRevisionID  *int64    `json:"previousRevisionId"`
	ActorID             int64     `json:"actorId"`
	Reason              *string   `json:"reason"`
	OccurredAt          time.Time `json:"occurredAt"`
}

func toCoursePointerJSON(c courses.PointerChange) coursePointerJSON {
	return coursePointerJSON{CourseID: c.CourseID, Action: c.Action, PublishedRevisionID: c.RevisionID,
		PreviousRevisionID: c.PreviousRevisionID, ActorID: c.ActorID, Reason: c.Reason, OccurredAt: c.OccurredAt}
}

type lifecycleJSON struct {
	ID          int64     `json:"id"`
	CourseID    int64     `json:"courseId"`
	ActorID     int64     `json:"actorId"`
	Action      string    `json:"action"`
	Reason      string    `json:"reason"`
	RevisionID  *int64    `json:"revisionId"`
	FromOwnerID *int64    `json:"fromOwnerId"`
	ToOwnerID   *int64    `json:"toOwnerId"`
	OccurredAt  time.Time `json:"occurredAt"`
}

func toLifecycleJSON(e courses.LifecycleEvent) lifecycleJSON {
	return lifecycleJSON{ID: e.ID, CourseID: e.CourseID, ActorID: e.ActorID, Action: e.Action, Reason: e.Reason,
		RevisionID: e.RevisionID, FromOwnerID: e.FromOwnerID, ToOwnerID: e.ToOwnerID, OccurredAt: e.OccurredAt}
}

// ---- requests ----

type reasonRequest struct {
	Reason *string `json:"reason" api:"nonnull"`
}

type courseExpectedRequest struct {
	Reason   *string         `json:"reason" api:"nonnull"`
	Expected expectedPointer `json:"expectedPublishedRevisionId"`
}

type courseRestoreRequest struct {
	RevisionID *int64          `json:"revisionId" api:"nonnull"`
	Expected   expectedPointer `json:"expectedPublishedRevisionId"`
}

type publishCourseRequest struct {
	Expected expectedPointer `json:"expectedPublishedRevisionId"`
}

type transferRequest struct {
	ToUserID *int64  `json:"toUserId" api:"nonnull"`
	Reason   *string `json:"reason" api:"nonnull"`
}

type amendmentRequest struct {
	Steps json.RawMessage `json:"steps"`
}

type replacementRequest struct {
	LessonID   *int64 `json:"lessonId" api:"nonnull"`
	RevisionID *int64 `json:"revisionId" api:"nonnull"`
}

const courseExpectedMessage = "expectedPublishedRevisionId is required: the course's current published revision ID, or null if it has none."

func reasonOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---- author ----

func (h *Handlers) submitCourse(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "revisionId")
	if !ok || !noQuery(w, r) {
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
	rev, err := h.Courses.Submit(r.Context(), actor(r), id, *req.ExpectedVersion)
	if err != nil {
		h.writeCourseError(w, "submit course", err)
		return
	}
	writeCourseRevision(w, http.StatusOK, rev)
}

func (h *Handlers) createAmendment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "courseId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req amendmentRequest
	if !decodeJSON(w, r, &req, []string{"steps"}) {
		return
	}
	if req.Steps == nil {
		badRequest(w, `"steps" is required (an empty array takes every newer eligible revision).`)
		return
	}
	items, err := objectList(req.Steps, "steps", courses.MaxSteps)
	if err != nil {
		badRequest(w, "The request body is not valid JSON for this endpoint: "+err.Error()+".")
		return
	}
	var choices []courses.Replacement
	for i, raw := range items {
		var rr replacementRequest
		if err := strictDecode(raw, &rr, []string{"lessonId", "revisionId"}); err != nil || rr.LessonID == nil || rr.RevisionID == nil {
			badRequest(w, "steps["+strconv.Itoa(i)+"] needs lessonId and revisionId, and nothing else.")
			return
		}
		choices = append(choices, courses.Replacement{LessonID: *rr.LessonID, RevisionID: *rr.RevisionID})
	}
	rev, err := h.Courses.CreateAmendment(r.Context(), actor(r), id, choices)
	if err != nil {
		h.writeCourseError(w, "create amendment", err)
		return
	}
	writeCourseRevision(w, http.StatusCreated, rev)
}

func (h *Handlers) requestArchive(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "courseId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req reasonRequest
	if !decodeJSON(w, r, &req, []string{"reason"}) {
		return
	}
	ev, err := h.Courses.RequestArchive(r.Context(), actor(r), id, reasonOf(req.Reason))
	if err != nil {
		h.writeCourseError(w, "request archive", err)
		return
	}
	writeJSON(w, http.StatusCreated, toLifecycleJSON(ev))
}

type newerRevisionJSON struct {
	ModuleKey        string `json:"moduleKey"`
	Position         int    `json:"position"`
	Title            string `json:"title"`
	LessonID         int64  `json:"lessonId"`
	PinnedRevisionID int64  `json:"pinnedRevisionId"`
	PinnedNumber     int    `json:"pinnedNumber"`
	NewerRevisionID  int64  `json:"newerRevisionId"`
	NewerNumber      int    `json:"newerNumber"`
}

func (h *Handlers) newerRevisions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "courseId")
	if !ok || !noQuery(w, r) {
		return
	}
	n, err := h.Courses.NewerRevisions(r.Context(), actor(r), id)
	if err != nil {
		h.writeCourseError(w, "newer revisions", err)
		return
	}
	steps := make([]newerRevisionJSON, len(n.Steps))
	for i, s := range n.Steps {
		steps[i] = newerRevisionJSON{ModuleKey: s.ModuleKey, Position: s.Position, Title: s.Title, LessonID: s.LessonID,
			PinnedRevisionID: s.PinnedRevisionID, PinnedNumber: s.PinnedNumber, NewerRevisionID: s.NewerRevisionID, NewerNumber: s.NewerNumber}
	}
	writeJSON(w, http.StatusOK, struct {
		CourseID           int64               `json:"courseId"`
		ComparisonRevision *int64              `json:"comparisonRevisionId"`
		Steps              []newerRevisionJSON `json:"steps"`
	}{n.CourseID, n.ComparisonRevision, steps})
}

// ---- review ----

type fieldChangeJSON struct {
	Name string          `json:"name"`
	Old  json.RawMessage `json:"old"`
	New  json.RawMessage `json:"new"`
}

type blockChangeJSON struct {
	Op    string          `json:"op"`
	Block json.RawMessage `json:"block"`
}

type contentDiffJSON struct {
	Fields []fieldChangeJSON `json:"fields"`
	Blocks []blockChangeJSON `json:"blocks"`
}

type stepChangeJSON struct {
	ModuleKey              string           `json:"moduleKey"`
	Position               int              `json:"position"`
	Title                  string           `json:"title"`
	LessonID               int64            `json:"lessonId"`
	RevisionID             int64            `json:"revisionId"`
	RevisionNumber         int              `json:"revisionNumber"`
	Change                 string           `json:"change"`
	PreviousRevisionID     *int64           `json:"previousRevisionId"`
	PreviousRevisionNumber *int             `json:"previousRevisionNumber"`
	PreviousModuleKey      *string          `json:"previousModuleKey"`
	PreviousPosition       *int             `json:"previousPosition"`
	PreviousTitle          *string          `json:"previousTitle"`
	StructureChanged       bool             `json:"structureChanged"`
	Unavailable            bool             `json:"unavailable"`
	PreviousUnavailable    bool             `json:"previousUnavailable"`
	Diff                   *contentDiffJSON `json:"diff"`
}

func (h *Handlers) reviewCourseRevision(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "revisionId")
	if !ok || !noQuery(w, r) {
		return
	}
	p, err := h.Courses.ReviewRevision(r.Context(), actor(r), id)
	if err != nil {
		h.writeCourseError(w, "review course revision", err)
		return
	}
	steps := make([]stepChangeJSON, len(p.Steps))
	for i, s := range p.Steps {
		steps[i] = stepChangeJSON{ModuleKey: s.ModuleKey, Position: s.Position, Title: s.Title, LessonID: s.LessonID,
			RevisionID: s.RevisionID, RevisionNumber: s.RevisionNumber, Change: s.Change,
			PreviousRevisionID: s.PreviousRevisionID, PreviousRevisionNumber: s.PreviousRevisionNumber,
			PreviousModuleKey: s.PreviousModuleKey, PreviousPosition: s.PreviousPosition, PreviousTitle: s.PreviousTitle,
			StructureChanged: s.StructureChanged, Unavailable: s.Unavailable, PreviousUnavailable: s.PreviousUnavailable}
		if d := s.Diff; d != nil {
			dj := &contentDiffJSON{Fields: []fieldChangeJSON{}, Blocks: []blockChangeJSON{}}
			for _, f := range d.Fields {
				dj.Fields = append(dj.Fields, fieldChangeJSON{Name: f.Name, Old: f.Old, New: f.New})
			}
			for _, b := range d.Blocks {
				dj.Blocks = append(dj.Blocks, blockChangeJSON{Op: b.Op, Block: b.Block})
			}
			steps[i].Diff = dj
		}
	}
	removed := make([]ineligibleStepJSON, len(p.Removed))
	for i, s := range p.Removed {
		removed[i] = ineligibleStepJSON{ModuleKey: s.ModuleKey, Position: s.Position, Title: s.Title, LessonID: s.LessonID, RevisionID: s.RevisionID}
	}
	writeJSON(w, http.StatusOK, struct {
		Revision        courseRevisionJSON    `json:"revision"`
		Eligible        bool                  `json:"eligible"`
		Problems        []reviewerProblemJSON `json:"problems"`
		MetadataChanged bool                  `json:"metadataChanged"`
		Steps           []stepChangeJSON      `json:"steps"`
		RemovedSteps    []ineligibleStepJSON  `json:"removedSteps"`
	}{toCourseRevisionJSON(p.Revision), p.Eligible, toProblemsJSON(p.Problems, false), p.MetadataChanged, steps, removed})
}

func (h *Handlers) approveCourse(w http.ResponseWriter, r *http.Request) {
	h.decideCourse(w, r, true)
}

func (h *Handlers) requestCourseChanges(w http.ResponseWriter, r *http.Request) {
	h.decideCourse(w, r, false)
}

func (h *Handlers) decideCourse(w http.ResponseWriter, r *http.Request, approve bool) {
	id, ok := pathID(w, r, "revisionId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req decisionRequest
	if !decodeJSON(w, r, &req, decisionKeys) {
		return
	}
	var rev courses.Revision
	var err error
	if approve {
		rev, err = h.Courses.Approve(r.Context(), actor(r), id, reasonOf(req.Comment))
	} else {
		rev, err = h.Courses.RequestChanges(r.Context(), actor(r), id, reasonOf(req.Comment))
	}
	if err != nil {
		h.writeCourseError(w, "decide course", err)
		return
	}
	writeCourseRevision(w, http.StatusOK, rev)
}

type courseReviewItemJSON struct {
	RevisionID  int64        `json:"revisionId"`
	CourseID    int64        `json:"courseId"`
	Number      int          `json:"number"`
	Kind        string       `json:"kind"`
	Title       string       `json:"title"`
	Language    string       `json:"language"`
	SubjectID   int64        `json:"subjectId"`
	AuthorID    int64        `json:"authorId"`
	AuthorName  string       `json:"authorName"`
	SubmittedAt time.Time    `json:"submittedAt"`
	Eligible    bool         `json:"eligible"`
	Blocked     *blockedJSON `json:"blocked"`
}

func toReviewItemsJSON(items []courses.ReviewItem) []courseReviewItemJSON {
	out := make([]courseReviewItemJSON, len(items))
	for i, it := range items {
		out[i] = courseReviewItemJSON{RevisionID: it.RevisionID, CourseID: it.CourseID, Number: it.Number, Kind: it.Kind,
			Title: it.Title, Language: it.Language, SubjectID: it.SubjectID, AuthorID: it.AuthorID, AuthorName: it.AuthorName,
			SubmittedAt: it.SubmittedAt, Eligible: it.Eligible, Blocked: toBlockedJSON(it.Blocked)}
	}
	return out
}

// courseReviewQueue serves GET /api/review/queue?type=course[&subjectId=].
func (h *Handlers) courseReviewQueue(w http.ResponseWriter, r *http.Request, subject int64) {
	items, err := h.Courses.ReviewQueue(r.Context(), actor(r), subject)
	if err != nil {
		h.writeCourseError(w, "course review queue", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": toReviewItemsJSON(items)})
}

func (h *Handlers) blockedReviews(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var after int64
	for key, vals := range q {
		if key != "cursor" || len(vals) != 1 {
			badRequest(w, "The only query parameter is an optional cursor (the nextCursor of a previous page).")
			return
		}
		c, ok := parseID(vals[0])
		if !ok {
			badRequest(w, "cursor must be the nextCursor of a previous page.")
			return
		}
		after = c
	}
	items, next, err := h.Courses.BlockedReviews(r.Context(), actor(r), after)
	if err != nil {
		h.writeCourseError(w, "blocked reviews", err)
		return
	}
	var cursor *string
	if next != nil {
		c := strconv.FormatInt(*next, 10)
		cursor = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": toReviewItemsJSON(items), "nextCursor": cursor})
}

// ---- publication ----

type coursePublishItemJSON struct {
	RevisionID          int64     `json:"revisionId"`
	CourseID            int64     `json:"courseId"`
	CoursePublicID      string    `json:"coursePublicId"`
	Number              int       `json:"number"`
	Kind                string    `json:"kind"`
	Title               string    `json:"title"`
	Language            string    `json:"language"`
	SubjectID           int64     `json:"subjectId"`
	AuthorID            int64     `json:"authorId"`
	AuthorName          string    `json:"authorName"`
	ApprovedBy          int64     `json:"approvedBy"`
	ApprovedAt          time.Time `json:"approvedAt"`
	BaseRevisionID      *int64    `json:"baseRevisionId"`
	PublishedRevisionID *int64    `json:"publishedRevisionId"`
	StaleBase           bool      `json:"staleBase"`
	Archived            bool      `json:"archived"`
	IneligibleSteps     int       `json:"ineligibleSteps"`
}

// coursePublishQueue serves GET /api/publish/queue?type=course.
func (h *Handlers) coursePublishQueue(w http.ResponseWriter, r *http.Request) {
	items, err := h.Courses.PublishQueue(r.Context(), actor(r))
	if err != nil {
		h.writeCourseError(w, "course publish queue", err)
		return
	}
	out := make([]coursePublishItemJSON, len(items))
	for i, p := range items {
		out[i] = coursePublishItemJSON{RevisionID: p.RevisionID, CourseID: p.CourseID, CoursePublicID: p.CoursePublicID,
			Number: p.Number, Kind: p.Kind, Title: p.Title, Language: p.Language, SubjectID: p.SubjectID, AuthorID: p.AuthorID,
			AuthorName: p.AuthorName, ApprovedBy: p.ApprovedBy, ApprovedAt: p.ApprovedAt, BaseRevisionID: p.BaseRevisionID,
			PublishedRevisionID: p.PublishedRevisionID, StaleBase: p.StaleBase(), Archived: p.Archived, IneligibleSteps: p.IneligibleSteps}
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": out})
}

func (h *Handlers) publishCourse(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "revisionId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req publishCourseRequest
	if !decodeJSON(w, r, &req, []string{"expectedPublishedRevisionId"}) {
		return
	}
	if !req.Expected.valid(true) {
		badRequest(w, courseExpectedMessage)
		return
	}
	pc, err := h.Courses.Publish(r.Context(), actor(r), id, req.Expected.id)
	if err != nil {
		h.writeCourseError(w, "publish course", err)
		return
	}
	writeJSON(w, http.StatusOK, toCoursePointerJSON(pc))
}

func (h *Handlers) restoreCourse(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "courseId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req courseRestoreRequest
	if !decodeJSON(w, r, &req, []string{"revisionId", "expectedPublishedRevisionId"}) {
		return
	}
	if req.RevisionID == nil || !req.Expected.valid(true) {
		badRequest(w, "revisionId is required. "+courseExpectedMessage)
		return
	}
	pc, err := h.Courses.Restore(r.Context(), actor(r), id, *req.RevisionID, req.Expected.id)
	if err != nil {
		h.writeCourseError(w, "restore course", err)
		return
	}
	writeJSON(w, http.StatusOK, toCoursePointerJSON(pc))
}

// ---- administrator ----

func (h *Handlers) suspendCourse(w http.ResponseWriter, r *http.Request) {
	h.pointerAdmin(w, r, false)
}

func (h *Handlers) archiveCourse(w http.ResponseWriter, r *http.Request) {
	h.pointerAdmin(w, r, true)
}

func (h *Handlers) pointerAdmin(w http.ResponseWriter, r *http.Request, archive bool) {
	id, ok := pathID(w, r, "courseId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req courseExpectedRequest
	if !decodeJSON(w, r, &req, []string{"reason", "expectedPublishedRevisionId"}) {
		return
	}
	if !req.Expected.valid(true) {
		badRequest(w, courseExpectedMessage)
		return
	}
	var pc courses.PointerChange
	var err error
	if archive {
		pc, err = h.Courses.Archive(r.Context(), actor(r), id, req.Expected.id, reasonOf(req.Reason))
	} else {
		pc, err = h.Courses.Suspend(r.Context(), actor(r), id, req.Expected.id, reasonOf(req.Reason))
	}
	if err != nil {
		h.writeCourseError(w, "suspend or archive course", err)
		return
	}
	writeJSON(w, http.StatusOK, toCoursePointerJSON(pc))
}

func (h *Handlers) unarchiveCourse(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "courseId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req reasonRequest
	if !decodeJSON(w, r, &req, []string{"reason"}) {
		return
	}
	ev, err := h.Courses.Unarchive(r.Context(), actor(r), id, reasonOf(req.Reason))
	if err != nil {
		h.writeCourseError(w, "unarchive course", err)
		return
	}
	writeJSON(w, http.StatusOK, toLifecycleJSON(ev))
}

func (h *Handlers) transferCourse(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "courseId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req transferRequest
	if !decodeJSON(w, r, &req, []string{"toUserId", "reason"}) {
		return
	}
	if req.ToUserID == nil {
		badRequest(w, "toUserId is required.")
		return
	}
	ev, err := h.Courses.Transfer(r.Context(), actor(r), id, *req.ToUserID, reasonOf(req.Reason))
	if err != nil {
		h.writeCourseError(w, "transfer course", err)
		return
	}
	writeJSON(w, http.StatusOK, toLifecycleJSON(ev))
}

func (h *Handlers) withdrawCourseRevision(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "revisionId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req reasonRequest
	if !decodeJSON(w, r, &req, []string{"reason"}) {
		return
	}
	wd, err := h.Courses.Withdraw(r.Context(), actor(r), id, reasonOf(req.Reason))
	if err != nil {
		h.writeCourseError(w, "withdraw course revision", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		CourseID    int64     `json:"courseId"`
		RevisionID  int64     `json:"revisionId"`
		WithdrawnBy int64     `json:"withdrawnBy"`
		Reason      string    `json:"reason"`
		WithdrawnAt time.Time `json:"withdrawnAt"`
	}{wd.CourseID, wd.RevisionID, wd.ActorID, wd.Reason, wd.OccurredAt})
}

func (h *Handlers) adminDiscardCourseDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "revisionId")
	if !ok || !noQuery(w, r) {
		return
	}
	var req reasonRequest
	if !decodeJSON(w, r, &req, []string{"reason"}) {
		return
	}
	ev, err := h.Courses.DiscardDraftAsAdmin(r.Context(), actor(r), id, reasonOf(req.Reason))
	if err != nil {
		h.writeCourseError(w, "discard course draft", err)
		return
	}
	writeJSON(w, http.StatusOK, toLifecycleJSON(ev))
}

func (h *Handlers) archiveRequests(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r) {
		return
	}
	list, err := h.Courses.ArchiveRequests(r.Context(), actor(r))
	if err != nil {
		h.writeCourseError(w, "archive requests", err)
		return
	}
	type requestJSON struct {
		lifecycleJSON
		Archived            bool   `json:"archived"`
		PublishedRevisionID *int64 `json:"publishedRevisionId"`
		Title               string `json:"title"`
	}
	out := make([]requestJSON, len(list))
	for i, a := range list {
		out[i] = requestJSON{toLifecycleJSON(a.LifecycleEvent), a.Archived, a.PublishedRevisionID, a.Title}
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

// writeCourseWorkflowError maps the B04 errors; it returns false for an
// error it does not know (writeCourseError handles the rest).
func writeCourseWorkflowError(w http.ResponseWriter, err error) bool {
	conflict := func(code, msg string) { auth.WriteError(w, http.StatusConflict, code, msg) }
	unprocessable := func(code, msg string) { auth.WriteError(w, http.StatusUnprocessableEntity, code, msg) }
	var inc *courses.IncompleteError
	var ine *courses.IneligibleError
	var rev *courses.ReviewerError
	switch {
	case errors.As(err, &inc):
		writeErrorWith(w, http.StatusUnprocessableEntity, "incomplete",
			"The course revision is incomplete: it needs a title, a summary, objectives and at least one module, each with a step.",
			"fields", inc.Fields)
	case errors.As(err, &ine):
		steps := make([]ineligibleStepJSON, len(ine.Steps))
		for i, s := range ine.Steps {
			steps[i] = ineligibleStepJSON{ModuleKey: s.ModuleKey, Position: s.Position, Title: s.Title, LessonID: s.LessonID, RevisionID: s.RevisionID}
		}
		writeErrorWith(w, http.StatusConflict, "ineligible",
			"A step pins a lesson revision that is not eligible (approved, published at least once, not withdrawn, its lesson published and not archived).",
			"steps", steps)
	case errors.As(err, &rev):
		writeErrorWith(w, http.StatusForbidden, "not_eligible_reviewer",
			"You may not decide on this course revision: you authored it, or a lesson revision it adds or replaces.",
			"reasons", toProblemsJSON(rev.Reasons, false))
	case errors.Is(err, courses.ErrNotInReview):
		conflict("not_in_review", "The course revision is not in review.")
	case errors.Is(err, courses.ErrWithdrawn):
		conflict("withdrawn", "The course revision was withdrawn.")
	case errors.Is(err, courses.ErrCommentRequired):
		unprocessable("comment_required", "A comment is required to request changes.")
	case errors.Is(err, courses.ErrInvalidComment):
		unprocessable("invalid_comment", "The comment must be valid text of at most 5000 characters.")
	case errors.Is(err, courses.ErrReasonRequired):
		unprocessable("reason_required", "A reason is required.")
	case errors.Is(err, courses.ErrInvalidReason):
		unprocessable("invalid_reason", "The reason must be valid text of at most 2000 characters.")
	case errors.Is(err, courses.ErrDraftExists):
		conflict("draft_exists", "The course has an open draft. Discard it, or add the replacement to it.")
	case errors.Is(err, courses.ErrNotPublished):
		conflict("not_published", "The course has no published revision.")
	case errors.Is(err, courses.ErrNothingToAmend):
		conflict("no_newer_revisions", "No step of the published course has a newer eligible lesson revision.")
	case errors.Is(err, courses.ErrNotAmendable):
		conflict("not_amendable", detail(err, courses.ErrNotAmendable, "These replacements cannot form an amendment; start an ordinary revision."))
	case errors.Is(err, courses.ErrStalePointer):
		conflict("stale_pointer", "The course's published revision is not the expected one. Reload and try again.")
	case errors.Is(err, courses.ErrStaleBase):
		conflict("stale_base", "The course's published revision changed since this revision was drafted.")
	case errors.Is(err, courses.ErrNotApproved):
		conflict("not_approved", "The course revision is not approved.")
	case errors.Is(err, courses.ErrAlreadyPublished):
		conflict("already_published", "The course revision was already published; restore it instead.")
	case errors.Is(err, courses.ErrNeverPublished):
		conflict("never_published", "The course revision was never published and cannot be restored.")
	case errors.Is(err, courses.ErrAlreadyCurrent):
		conflict("already_current", "The course revision is already the published one.")
	case errors.Is(err, courses.ErrPublished):
		conflict("published", "The revision is the published one; suspend the course instead.")
	case errors.Is(err, courses.ErrNotWithdrawable):
		conflict("not_withdrawable", "Only revisions in review or approved can be withdrawn.")
	case errors.Is(err, courses.ErrNotArchived):
		conflict("not_archived", "The course is not archived.")
	case errors.Is(err, courses.ErrNeverSubmitted):
		conflict("never_submitted", "Nothing of this course was submitted; delete it instead.")
	case errors.Is(err, courses.ErrUnknownUser):
		unprocessable("unknown_user", "The new owner must be an active author.")
	default:
		return false
	}
	return true
}
