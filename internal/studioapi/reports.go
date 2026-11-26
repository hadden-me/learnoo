package studioapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/reports"
)

// Error report endpoints (LRN-015), documented in docs/API.md.
//
// Routes need a session, the CSRF token on unsafe methods and either a
// reviewer assignment for at least one subject or the administrator
// permission (403 otherwise, before any lookup). Students therefore cannot
// list, read or resolve reports. The report's subject is then checked
// inside the store's transaction: a reviewer of another subject gets the
// same 404 as for a missing report; administrators see every report.

func (h *Handlers) registerReports(mux *http.ServeMux) {
	route := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(
			h.Perms.RequireReviewerOrAny([]permissions.Permission{permissions.Administrator}, fn)))))
	}
	route("GET /api/review/error-reports", h.listReports)
	route("GET /api/review/error-reports/{reportId}", h.getReport)
	route("POST /api/review/error-reports/{reportId}/resolve", h.resolveReport)
	route("POST /api/review/error-reports/{reportId}/dismiss", h.dismissReport)
}

type resolutionJSON struct {
	Action     string    `json:"action"`
	ActorID    int64     `json:"actorId"`
	Note       string    `json:"note"`
	OccurredAt time.Time `json:"occurredAt"`
}

type reportJSON struct {
	ID                 int64  `json:"id"`
	LessonID           int64  `json:"lessonId"`
	LessonPublicID     string `json:"lessonPublicId"`
	SubjectID          int64  `json:"subjectId"`
	RevisionID         int64  `json:"revisionId"`
	RevisionNumber     int    `json:"revisionNumber"`
	RevisionTitle      string `json:"revisionTitle"`
	CurrentlyPublished bool   `json:"currentlyPublished"`
	// Set only for a report filed in course context (LMS-B08).
	CourseRevisionID     *int64          `json:"courseRevisionId,omitempty"`
	CoursePublicID       string          `json:"coursePublicId,omitempty"`
	CourseRevisionNumber int             `json:"courseRevisionNumber,omitempty"`
	Message              string          `json:"message"`
	Status               reports.Status  `json:"status"`
	CreatedAt            time.Time       `json:"createdAt"`
	Resolution           *resolutionJSON `json:"resolution"`
}

func toReportJSON(r reports.Report) reportJSON {
	out := reportJSON{ID: r.ID, LessonID: r.LessonID, LessonPublicID: r.LessonPublicID, SubjectID: r.SubjectID,
		RevisionID: r.RevisionID, RevisionNumber: r.RevisionNumber, RevisionTitle: r.RevisionTitle,
		CurrentlyPublished: r.Current, Message: r.Message, Status: r.Status, CreatedAt: r.CreatedAt,
		CourseRevisionID: r.CourseRevisionID, CoursePublicID: r.CoursePublicID, CourseRevisionNumber: r.CourseRevisionNumber}
	if x := r.Resolution; x != nil {
		out.Resolution = &resolutionJSON{Action: x.Action, ActorID: x.ActorID, Note: x.Note, OccurredAt: x.OccurredAt}
	}
	return out
}

type resolveRequest struct {
	Note *string `json:"note"`
}

var resolveKeys = []string{"note"}

func (h *Handlers) listReports(w http.ResponseWriter, r *http.Request) {
	var f reports.Filter
	for key, values := range r.URL.Query() {
		if len(values) != 1 {
			badRequest(w, "Each query parameter may appear once.")
			return
		}
		switch key {
		case "status":
			f.Status = reports.Status(values[0])
			if f.Status == "" {
				badRequest(w, "status must be open, resolved or dismissed.")
				return
			}
		case "subjectId":
			id, ok := parseID(values[0])
			if !ok {
				badRequest(w, "subjectId must be a positive integer.")
				return
			}
			f.SubjectID = id
		default:
			badRequest(w, "The only query parameters are an optional status and subjectId.")
			return
		}
	}
	list, err := h.Reports.List(r.Context(), actor(r), f)
	if errors.Is(err, reports.ErrInvalidStatus) {
		badRequest(w, "status must be open, resolved or dismissed.")
		return
	}
	if err != nil {
		h.writeReportError(w, "list error reports", err)
		return
	}
	out := make([]reportJSON, 0, len(list))
	for _, rep := range list {
		out = append(out, toReportJSON(rep))
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": out})
}

func (h *Handlers) getReport(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("reportId"))
	if !ok {
		notFound(w)
		return
	}
	rep, err := h.Reports.Get(r.Context(), actor(r), id)
	if err != nil {
		h.writeReportError(w, "get error report", err)
		return
	}
	writeJSON(w, http.StatusOK, toReportJSON(rep))
}

func (h *Handlers) resolveReport(w http.ResponseWriter, r *http.Request) {
	h.closeReport(w, r, reports.ActionResolve)
}

func (h *Handlers) dismissReport(w http.ResponseWriter, r *http.Request) {
	h.closeReport(w, r, reports.ActionDismiss)
}

func (h *Handlers) closeReport(w http.ResponseWriter, r *http.Request, action string) {
	id, ok := parseID(r.PathValue("reportId"))
	if !ok {
		notFound(w)
		return
	}
	var req resolveRequest
	if !decodeJSON(w, r, &req, resolveKeys) {
		return
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}
	rep, err := h.Reports.Resolve(r.Context(), actor(r), id, action, note)
	if err != nil {
		h.writeReportError(w, action+" error report", err)
		return
	}
	writeJSON(w, http.StatusOK, toReportJSON(rep))
}

// writeReportError maps report errors. A denial on a report's subject is
// reported exactly like a missing report.
func (h *Handlers) writeReportError(w http.ResponseWriter, op string, err error) {
	switch {
	case isRetryable(err):
		writeRetry(w)
	case deniedWithCause(err):
		h.internal(w, op, err)
	case errors.Is(err, reports.ErrNotFound):
		notFound(w)
	case errors.Is(err, reports.ErrDenied):
		auth.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.Is(err, reports.ErrTextRequired):
		auth.WriteError(w, http.StatusUnprocessableEntity, "note_required", "Explain the resolution in a note.")
	case errors.Is(err, reports.ErrInvalidText):
		auth.WriteError(w, http.StatusUnprocessableEntity, "invalid_note",
			"The note must be text of at most "+strconv.Itoa(reports.MaxNote)+" characters, without control or bidirectional formatting characters.")
	case errors.Is(err, reports.ErrNotOpen):
		auth.WriteError(w, http.StatusConflict, "not_open", "The report was already resolved or dismissed.")
	case errors.Is(err, reports.ErrRejected):
		auth.WriteError(w, http.StatusConflict, "immutable", "The change was rejected by a database guard.")
	default:
		h.internal(w, op, err)
	}
}
