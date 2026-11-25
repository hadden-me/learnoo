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

// Review and publication endpoints (LRN-007), documented in docs/API.md.
//
// Review routes need a session, the CSRF token on unsafe methods and a
// reviewer assignment for at least one subject (403 otherwise, before any
// lookup). The subject of the revision is then checked inside the store's
// transaction; a reviewer of another subject gets the same 404 as for a
// missing revision. Publication needs the global publisher permission,
// checked by the middleware and again inside the transaction.

func (h *Handlers) registerReview(mux *http.ServeMux) {
	reviewer := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(
			h.Perms.RequireAnyReviewer(fn)))))
	}
	reviewer("GET /api/review/queue", h.reviewQueue)
	reviewer("GET /api/review/revisions/{revisionId}", h.reviewRevision)
	reviewer("POST /api/review/revisions/{revisionId}/approve", h.approve)
	reviewer("POST /api/review/revisions/{revisionId}/request-changes", h.requestChanges)
	reviewer("GET /api/review/media/{mediaId}", h.reviewMedia)
	mux.Handle("POST /api/revisions/{revisionId}/publish", h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(
		h.Perms.Require(permissions.Publisher, http.HandlerFunc(h.publish))))))
}

// reviewableBy is the reviewer media rule for the user in $2 on asset row
// a: the asset is referenced by a revision in review whose subject the user
// is an active reviewer of.
const reviewableBy = `EXISTS (SELECT 1 FROM revision_media rm
	JOIN lesson_revisions r ON r.id = rm.revision_id AND r.status = 'in_review'
	JOIN reviewer_assignments ra ON ra.subject_id = r.subject_id AND ra.user_id = $2
	JOIN users u ON u.id = ra.user_id AND u.disabled_at IS NULL
	WHERE rm.media_asset_id = a.id)`

type decisionRequest struct {
	Comment *string `json:"comment"`
}

var decisionKeys = []string{"comment"}

type decisionJSON struct {
	Action     string    `json:"action"`
	ReviewerID int64     `json:"reviewerId"`
	Comment    *string   `json:"comment"`
	DecidedAt  time.Time `json:"decidedAt"`
}

func toDecisionJSON(d *lessons.Decision) *decisionJSON {
	if d == nil {
		return nil
	}
	out := &decisionJSON{Action: d.Action, ReviewerID: d.ReviewerID, DecidedAt: d.DecidedAt}
	if d.Comment != "" {
		c := d.Comment
		out.Comment = &c
	}
	return out
}

type queueItemJSON struct {
	RevisionID  int64     `json:"revisionId"`
	LessonID    int64     `json:"lessonId"`
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Language    string    `json:"language"`
	SubjectID   int64     `json:"subjectId"`
	ChapterID   *int64    `json:"chapterId"`
	AuthorID    int64     `json:"authorId"`
	SubmittedAt time.Time `json:"submittedAt"`
}

type publicationJSON struct {
	LessonID            int64     `json:"lessonId"`
	PublishedRevisionID int64     `json:"publishedRevisionId"`
	PreviousRevisionID  *int64    `json:"previousRevisionId"`
	PublishedBy         int64     `json:"publishedBy"`
	PublishedAt         time.Time `json:"publishedAt"`
}

func (h *Handlers) reviewQueue(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var subject int64
	switch {
	case len(q) == 0:
	case len(q) == 1 && len(q["subjectId"]) == 1:
		id, ok := parseID(q.Get("subjectId"))
		if !ok {
			badRequest(w, "subjectId must be a positive integer.")
			return
		}
		subject = id
	default:
		badRequest(w, "The only query parameter is an optional subjectId.")
		return
	}
	items, err := h.Review.ReviewQueue(r.Context(), actor(r), subject)
	if err != nil {
		h.writeReviewError(w, "review queue", err)
		return
	}
	out := make([]queueItemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, queueItemJSON{RevisionID: it.RevisionID, LessonID: it.LessonID, Number: it.Number,
			Title: it.Title, Language: it.Language, SubjectID: it.SubjectID, ChapterID: it.ChapterID,
			AuthorID: it.AuthorID, SubmittedAt: it.SubmittedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": out})
}

func (h *Handlers) reviewRevision(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("revisionId"))
	if !ok {
		notFound(w)
		return
	}
	rev, err := h.Review.ReviewRevision(r.Context(), actor(r), id)
	if err != nil {
		h.writeReviewError(w, "review revision", err)
		return
	}
	h.writeRevision(w, http.StatusOK, rev)
}

func (h *Handlers) approve(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, review.ActionApprove)
}

func (h *Handlers) requestChanges(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, review.ActionRequestChanges)
}

func (h *Handlers) decide(w http.ResponseWriter, r *http.Request, action string) {
	id, ok := parseID(r.PathValue("revisionId"))
	if !ok {
		notFound(w)
		return
	}
	var req decisionRequest
	if !decodeJSON(w, r, &req, decisionKeys) {
		return
	}
	comment := ""
	if req.Comment != nil {
		comment = *req.Comment
	}
	var rev lessons.Revision
	var err error
	if action == review.ActionApprove {
		rev, err = h.Review.Approve(r.Context(), actor(r), id, comment)
	} else {
		rev, err = h.Review.RequestChanges(r.Context(), actor(r), id, comment)
	}
	if err != nil {
		h.writeReviewError(w, action, err)
		return
	}
	h.writeRevision(w, http.StatusOK, rev)
}

func (h *Handlers) reviewMedia(w http.ResponseWriter, r *http.Request) {
	h.serveMediaWhere(w, r, reviewableBy)
}

func (h *Handlers) publish(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("revisionId"))
	if !ok {
		notFound(w)
		return
	}
	// The body is an empty JSON object; the expected base is the revision's
	// own base_revision_id, compared with the pointer under the lesson lock.
	var req struct{}
	if !decodeJSON(w, r, &req, nil) {
		return
	}
	pub, err := h.Review.Publish(r.Context(), actor(r), id)
	if err != nil {
		h.writePublishError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publicationJSON{LessonID: pub.LessonID, PublishedRevisionID: pub.RevisionID,
		PreviousRevisionID: pub.PreviousRevisionID, PublishedBy: pub.ActorID, PublishedAt: pub.PublishedAt})
}

// writeReviewError maps review errors on reviewer routes. A denial means
// the caller does not review the revision's subject (or lost the assignment
// mid-request) and is reported exactly like a missing revision.
func (h *Handlers) writeReviewError(w http.ResponseWriter, op string, err error) {
	switch {
	case isRetryable(err):
		writeRetry(w)
	case deniedWithCause(err):
		h.internal(w, op, err)
	case errors.Is(err, review.ErrDenied), errors.Is(err, review.ErrNotFound):
		notFound(w)
	case errors.Is(err, review.ErrSelfReview):
		auth.WriteError(w, http.StatusForbidden, "self_review", "You cannot review your own revision.")
	case errors.Is(err, review.ErrNotInReview):
		auth.WriteError(w, http.StatusConflict, "not_in_review", "The revision is no longer in review; another decision was recorded first.")
	case errors.Is(err, review.ErrCommentRequired):
		auth.WriteError(w, http.StatusUnprocessableEntity, "comment_required", "Explain the requested changes in a comment.")
	case errors.Is(err, review.ErrInvalidComment):
		auth.WriteError(w, http.StatusUnprocessableEntity, "invalid_comment", "The comment must be text of at most 5000 characters.")
	case errors.Is(err, review.ErrRejected):
		auth.WriteError(w, http.StatusConflict, "immutable", "The change was rejected by a database guard.")
	default:
		h.internal(w, op, err)
	}
}

// writePublishError maps publication errors. publisher is global, so
// publishers may learn which revisions exist; a denial (permission revoked
// mid-request) is 403.
func (h *Handlers) writePublishError(w http.ResponseWriter, err error) {
	conflict := func(code, msg string) { auth.WriteError(w, http.StatusConflict, code, msg) }
	switch {
	case isRetryable(err):
		writeRetry(w)
	case deniedWithCause(err):
		h.internal(w, "publish", err)
	case errors.Is(err, review.ErrDenied):
		auth.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.Is(err, review.ErrNotFound):
		notFound(w)
	case errors.Is(err, review.ErrNotApproved):
		conflict("not_approved", "Only an approved revision can be published.")
	case errors.Is(err, review.ErrAlreadyPublished):
		conflict("already_published", "This revision was already published. Restoring an earlier revision is a separate action.")
	case errors.Is(err, review.ErrStaleBase):
		conflict("stale_base", "The published lesson changed since this revision was drafted. Nothing was published; a reconciled revision must be reviewed.")
	case errors.Is(err, review.ErrArchived):
		conflict("archived", "The lesson is archived and accepts no changes.")
	case errors.Is(err, review.ErrRejected):
		conflict("immutable", "The change was rejected by a database guard.")
	default:
		h.internal(w, "publish", err)
	}
}
