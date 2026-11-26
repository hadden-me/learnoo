package studioapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/review"
)

// Read endpoints for the Studio's publisher and administrator views (issue
// #66 second slice), documented in docs/API.md: the publisher queue, a
// publisher's read of a revision awaiting publication, a lesson's
// publication state and history (publisher or administrator, who may both
// restore), and the administrator's lesson list. They change nothing. The
// permission is checked by the middleware before any lookup (403) and again
// inside the store's transaction; these are global permissions, so a
// missing ID is a plain 404.

func (h *Handlers) registerPublishing(mux *http.ServeMux) {
	guard := func(pattern string, perms []permissions.Permission, fn http.HandlerFunc) {
		mux.Handle(pattern, h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(
			h.Perms.RequireAny(perms, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", privateNoStore)
				fn(w, r)
			}))))))
	}
	publisher := []permissions.Permission{permissions.Publisher}
	guard("GET /api/publish/queue", publisher, h.publishQueue)
	guard("GET /api/publish/revisions/{revisionId}", publisher, h.publishRevision)
	guard("GET /api/publish/media/{mediaId}", publisher, h.publishMedia)
	guard("GET /api/lessons/{lesson}/publication",
		[]permissions.Permission{permissions.Publisher, permissions.Administrator}, h.lessonPublication)
	guard("GET /api/admin/lessons", []permissions.Permission{permissions.Administrator}, h.adminLessons)
}

type publishQueueItemJSON struct {
	RevisionID          int64     `json:"revisionId"`
	LessonID            int64     `json:"lessonId"`
	LessonPublicID      string    `json:"lessonPublicId"`
	Number              int       `json:"number"`
	Title               string    `json:"title"`
	Language            string    `json:"language"`
	SubjectID           int64     `json:"subjectId"`
	SubjectCode         string    `json:"subjectCode"`
	AuthorID            int64     `json:"authorId"`
	AuthorName          string    `json:"authorName"`
	ApprovedBy          int64     `json:"approvedBy"`
	ApproverName        string    `json:"approverName"`
	ApprovedAt          time.Time `json:"approvedAt"`
	BaseRevisionID      *int64    `json:"baseRevisionId"`
	PublishedRevisionID *int64    `json:"publishedRevisionId"`
	StaleBase           bool      `json:"staleBase"`
}

func toQueueItemJSON(it review.PublishQueueItem) publishQueueItemJSON {
	return publishQueueItemJSON{RevisionID: it.RevisionID, LessonID: it.LessonID, LessonPublicID: it.LessonPublicID,
		Number: it.Number, Title: it.Title, Language: it.Language, SubjectID: it.SubjectID, SubjectCode: it.SubjectCode,
		AuthorID: it.AuthorID, AuthorName: it.AuthorName, ApprovedBy: it.ApprovedBy, ApproverName: it.ApproverName,
		ApprovedAt: it.ApprovedAt, BaseRevisionID: it.BaseRevisionID, PublishedRevisionID: it.PublishedRevisionID,
		StaleBase: it.Stale()}
}

// noQuery refuses any query parameter on endpoints that take none.
func noQuery(w http.ResponseWriter, r *http.Request) bool {
	if len(r.URL.RawQuery) > 0 {
		badRequest(w, "This endpoint takes no query parameters.")
		return false
	}
	return true
}

func (h *Handlers) publishQueue(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r) {
		return
	}
	items, err := h.Review.PublishQueue(r.Context(), actor(r))
	if err != nil {
		h.writeReadError(w, "publish queue", err)
		return
	}
	out := make([]publishQueueItemJSON, len(items))
	for i, it := range items {
		out[i] = toQueueItemJSON(it)
	}
	writeJSON(w, http.StatusOK, struct {
		Revisions []publishQueueItemJSON `json:"revisions"`
	}{out})
}

// publishRevision answers {"item": queue item, "revision": revision object}.
func (h *Handlers) publishRevision(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r) {
		return
	}
	id, ok := parseID(r.PathValue("revisionId"))
	if !ok {
		id = 0 // the store checks the permission first, then answers 404
	}
	item, rev, err := h.Review.PublishRevision(r.Context(), actor(r), id)
	if err != nil {
		h.writeReadError(w, "publish revision", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Item     publishQueueItemJSON `json:"item"`
		Revision revisionJSON         `json:"revision"`
	}{toQueueItemJSON(item), toRevisionJSON(rev)})
}

type withdrawalInfoJSON struct {
	WithdrawnBy     int64     `json:"withdrawnBy"`
	WithdrawnByName string    `json:"withdrawnByName"`
	Reason          string    `json:"reason"`
	WithdrawnAt     time.Time `json:"withdrawnAt"`
}

type submissionInfoJSON struct {
	SubmittedBy int64     `json:"submittedBy"`
	SubmittedAt time.Time `json:"submittedAt"`
	Backfilled  bool      `json:"backfilled"`
}

type lessonRevisionJSON struct {
	ID             int64               `json:"id"`
	Number         int                 `json:"number"`
	Status         lessons.Status      `json:"status"`
	Title          string              `json:"title"`
	AuthorID       *int64              `json:"authorId"`
	AuthorName     *string             `json:"authorName"`
	BaseRevisionID *int64              `json:"baseRevisionId"`
	SubmittedAt    *time.Time          `json:"submittedAt"`
	ApprovedBy     *int64              `json:"approvedBy"`
	ApproverName   *string             `json:"approverName"`
	ApprovedAt     *time.Time          `json:"approvedAt"`
	EverPublished  bool                `json:"everPublished"`
	Candidate      bool                `json:"candidate"`
	Withdrawn      bool                `json:"withdrawn"`
	Withdrawal     *withdrawalInfoJSON `json:"withdrawal"`
	Submission     *submissionInfoJSON `json:"submission"`
}

type pointerEventJSON struct {
	Action              string    `json:"action"`
	PublishedRevisionID *int64    `json:"publishedRevisionId"`
	PreviousRevisionID  *int64    `json:"previousRevisionId"`
	ActorID             int64     `json:"actorId"`
	ActorName           string    `json:"actorName"`
	Reason              *string   `json:"reason"`
	OccurredAt          time.Time `json:"occurredAt"`
}

type lessonPublicationJSON struct {
	// View is "administrator" (full) or "publisher" (what restoration needs).
	View                string               `json:"view"`
	LessonID            int64                `json:"lessonId"`
	PublicID            string               `json:"publicId"`
	SubjectID           int64                `json:"subjectId"`
	SubjectCode         string               `json:"subjectCode"`
	Language            string               `json:"language"`
	Archived            bool                 `json:"archived"`
	PublishedRevisionID *int64               `json:"publishedRevisionId"`
	Revisions           []lessonRevisionJSON `json:"revisions"`
	History             []pointerEventJSON   `json:"history"`
}

func (h *Handlers) lessonPublication(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r) {
		return
	}
	// A malformed reference is a 404 like a missing lesson; the store checks
	// the permission first either way.
	lp, err := h.Review.LessonPublication(r.Context(), actor(r), r.PathValue("lesson"))
	if err != nil {
		h.writeReadError(w, "lesson publication", err)
		return
	}
	view := "publisher"
	if lp.Full {
		view = "administrator"
	}
	out := lessonPublicationJSON{View: view, LessonID: lp.LessonID, PublicID: lp.PublicID, SubjectID: lp.SubjectID,
		SubjectCode: lp.SubjectCode, Language: lp.Language, Archived: lp.Archived, PublishedRevisionID: lp.PublishedRevisionID,
		Revisions: make([]lessonRevisionJSON, len(lp.Revisions)), History: make([]pointerEventJSON, len(lp.History))}
	for i, rv := range lp.Revisions {
		out.Revisions[i] = lessonRevisionJSON{ID: rv.ID, Number: rv.Number, Status: rv.Status, Title: rv.Title,
			BaseRevisionID: rv.BaseRevisionID, SubmittedAt: rv.SubmittedAt,
			ApprovedBy: rv.ApprovedBy, ApproverName: rv.ApproverName, ApprovedAt: rv.ApprovedAt,
			EverPublished: rv.EverPublished, Candidate: rv.Candidate, Withdrawn: rv.Withdrawn}
		if lp.Full {
			id, name := rv.AuthorID, rv.AuthorName
			out.Revisions[i].AuthorID, out.Revisions[i].AuthorName = &id, &name
		}
		if sub := rv.Submission; sub != nil {
			out.Revisions[i].Submission = &submissionInfoJSON{SubmittedBy: sub.ActorID, SubmittedAt: sub.SubmittedAt,
				Backfilled: sub.Backfilled}
		}
		if wd := rv.Withdrawal; wd != nil {
			out.Revisions[i].Withdrawal = &withdrawalInfoJSON{WithdrawnBy: wd.ActorID, WithdrawnByName: rv.WithdrawnByName,
				Reason: wd.Reason, WithdrawnAt: wd.WithdrawnAt}
		}
	}
	for i, e := range lp.History {
		out.History[i] = pointerEventJSON{Action: e.Action, PublishedRevisionID: e.RevisionID,
			PreviousRevisionID: e.PreviousRevisionID, ActorID: e.ActorID, ActorName: e.ActorName, OccurredAt: e.OccurredAt}
		if e.Reason != "" {
			reason := e.Reason
			out.History[i].Reason = &reason
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type adminLessonJSON struct {
	LessonID            int64     `json:"lessonId"`
	PublicID            string    `json:"publicId"`
	SubjectCode         string    `json:"subjectCode"`
	Language            string    `json:"language"`
	Archived            bool      `json:"archived"`
	PublishedRevisionID *int64    `json:"publishedRevisionId"`
	Title               string    `json:"title"`
	PendingRevisionID   *int64    `json:"pendingRevisionId"`
	LastChange          time.Time `json:"lastChange"`
}

func (h *Handlers) adminLessons(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r) {
		return
	}
	list, err := h.Review.AdminLessons(r.Context(), actor(r))
	if err != nil {
		h.writeReadError(w, "admin lessons", err)
		return
	}
	out := make([]adminLessonJSON, len(list))
	for i, l := range list {
		out[i] = adminLessonJSON{LessonID: l.LessonID, PublicID: l.PublicID, SubjectCode: l.SubjectCode, Language: l.Language,
			Archived: l.Archived, PublishedRevisionID: l.PublishedRevisionID, Title: l.Title,
			PendingRevisionID: l.PendingRevisionID, LastChange: l.LastChange}
	}
	writeJSON(w, http.StatusOK, struct {
		Lessons []adminLessonJSON `json:"lessons"`
	}{out})
}

// writeReadError maps errors of the read endpoints above. Their permissions
// are global, so a denial (revoked between the middleware and the
// transaction) is 403 and a missing resource 404.
func (h *Handlers) writeReadError(w http.ResponseWriter, op string, err error) {
	switch {
	case isRetryable(err):
		writeRetry(w)
	case deniedWithCause(err):
		h.internal(w, op, err)
	case errors.Is(err, review.ErrDenied):
		auth.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.Is(err, review.ErrNotFound):
		notFound(w)
	default:
		h.internal(w, op, err)
	}
}

// publishableBy is the publisher media rule for the user in $2 on asset row
// a: the user holds the publisher permission and the asset is referenced by
// a revision awaiting publication (review.AwaitingPublication, the rule of
// GET /api/publish/revisions/{id}), so a publisher sees the images of what
// they publish and nothing else. Published lessons' images are read through
// the reader.
const publishableBy = `(EXISTS (SELECT 1 FROM user_permissions up JOIN users u ON u.id = up.user_id AND u.disabled_at IS NULL
	                      WHERE up.user_id = $2 AND up.permission = 'publisher')
	AND EXISTS (SELECT 1 FROM revision_media rm
	              JOIN lesson_revisions r ON r.id = rm.revision_id
	              JOIN lessons l ON l.id = r.lesson_id
	             WHERE rm.media_asset_id = a.id AND ` + review.AwaitingPublication + `))`

func (h *Handlers) publishMedia(w http.ResponseWriter, r *http.Request) {
	h.serveMediaWhere(w, r, publishableBy)
}
