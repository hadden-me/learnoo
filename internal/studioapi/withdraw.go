package studioapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/review"
)

// Withdrawal, emergency unpublication and restoration endpoints (LRN-009),
// documented in docs/API.md.
//
// Withdrawal and unpublication need the global administrator permission;
// restoration needs publisher or administrator. The permission is checked
// by the middleware before any lookup (403) and again inside the store's
// transaction. Every request names the published revision it expects to
// replace (expectedPublishedRevisionId); a mismatch is 409 stale_pointer and
// changes nothing.

func (h *Handlers) registerWithdrawal(mux *http.ServeMux) {
	guard := func(pattern string, perms []permissions.Permission, fn http.HandlerFunc) {
		mux.Handle(pattern, h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(
			h.Perms.RequireAny(perms, fn)))))
	}
	admin := []permissions.Permission{permissions.Administrator}
	guard("POST /api/revisions/{revisionId}/withdraw", admin, h.withdraw)
	guard("POST /api/lessons/{lessonId}/unpublish", admin, h.unpublish)
	guard("POST /api/revisions/{revisionId}/restore",
		[]permissions.Permission{permissions.Publisher, permissions.Administrator}, h.restore)
}

// expectedPointer is a required JSON field holding a revision ID or null.
type expectedPointer struct {
	set bool
	id  *int64
}

func (e *expectedPointer) UnmarshalJSON(b []byte) error {
	e.set = true
	if string(b) == "null" {
		e.id = nil
		return nil
	}
	var id int64
	if err := json.Unmarshal(b, &id); err != nil {
		return err
	}
	e.id = &id
	return nil
}

// valid reports a present field holding null (if nullable) or a positive ID.
func (e expectedPointer) valid(nullable bool) bool {
	if !e.set {
		return false
	}
	if e.id == nil {
		return nullable
	}
	return *e.id > 0
}

type withdrawRequest struct {
	Reason   *string         `json:"reason"`
	Expected expectedPointer `json:"expectedPublishedRevisionId"`
}

type restoreRequest struct {
	Expected expectedPointer `json:"expectedPublishedRevisionId"`
}

var (
	withdrawKeys = []string{"reason", "expectedPublishedRevisionId"}
	restoreKeys  = []string{"expectedPublishedRevisionId"}
)

const expectedPointerMessage = "expectedPublishedRevisionId is required: the lesson's current published revision ID, or null if it has none."

type pointerChangeJSON struct {
	LessonID            int64     `json:"lessonId"`
	Action              string    `json:"action"`
	PublishedRevisionID *int64    `json:"publishedRevisionId"`
	PreviousRevisionID  *int64    `json:"previousRevisionId"`
	ActorID             int64     `json:"actorId"`
	Reason              *string   `json:"reason"`
	OccurredAt          time.Time `json:"occurredAt"`
}

func toPointerChangeJSON(c review.PointerChange) pointerChangeJSON {
	out := pointerChangeJSON{LessonID: c.LessonID, Action: c.Action, PublishedRevisionID: c.RevisionID,
		PreviousRevisionID: c.PreviousRevisionID, ActorID: c.ActorID, OccurredAt: c.OccurredAt}
	if c.Reason != "" {
		r := c.Reason
		out.Reason = &r
	}
	return out
}

type withdrawalJSON struct {
	RevisionID  int64     `json:"revisionId"`
	LessonID    int64     `json:"lessonId"`
	WithdrawnBy int64     `json:"withdrawnBy"`
	Reason      string    `json:"reason"`
	WithdrawnAt time.Time `json:"withdrawnAt"`
	// The unpublication made in the same transaction, if the revision was
	// the published one; null otherwise.
	Unpublication *pointerChangeJSON `json:"unpublication"`
}

func (h *Handlers) withdraw(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("revisionId"))
	if !ok {
		notFound(w)
		return
	}
	var req withdrawRequest
	if !decodeJSON(w, r, &req, withdrawKeys) {
		return
	}
	if !req.Expected.valid(true) {
		badRequest(w, expectedPointerMessage)
		return
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}
	res, err := h.Review.Withdraw(r.Context(), actor(r), id, reason, req.Expected.id)
	if err != nil {
		h.writeWithdrawalError(w, "withdraw", err)
		return
	}
	out := withdrawalJSON{RevisionID: res.RevisionID, LessonID: res.LessonID, WithdrawnBy: res.ActorID,
		Reason: res.Reason, WithdrawnAt: res.WithdrawnAt}
	if res.Unpublished != nil {
		c := toPointerChangeJSON(*res.Unpublished)
		out.Unpublication = &c
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) unpublish(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("lessonId"))
	if !ok {
		notFound(w)
		return
	}
	var req withdrawRequest
	if !decodeJSON(w, r, &req, withdrawKeys) {
		return
	}
	if !req.Expected.valid(false) {
		badRequest(w, "expectedPublishedRevisionId is required: the ID of the published revision to remove.")
		return
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}
	ch, err := h.Review.Unpublish(r.Context(), actor(r), id, reason, *req.Expected.id)
	if err != nil {
		h.writeWithdrawalError(w, "unpublish", err)
		return
	}
	writeJSON(w, http.StatusOK, toPointerChangeJSON(ch))
}

func (h *Handlers) restore(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("revisionId"))
	if !ok {
		notFound(w)
		return
	}
	var req restoreRequest
	if !decodeJSON(w, r, &req, restoreKeys) {
		return
	}
	if !req.Expected.valid(true) {
		badRequest(w, expectedPointerMessage)
		return
	}
	ch, err := h.Review.Restore(r.Context(), actor(r), id, req.Expected.id)
	if err != nil {
		h.writeWithdrawalError(w, "restore", err)
		return
	}
	writeJSON(w, http.StatusOK, toPointerChangeJSON(ch))
}

// writeWithdrawalError maps withdrawal, unpublication and restoration
// errors. The permissions are global, so callers may learn which revisions
// exist; a denial (permission revoked mid-request) is 403.
func (h *Handlers) writeWithdrawalError(w http.ResponseWriter, op string, err error) {
	conflict := func(code, msg string) { auth.WriteError(w, http.StatusConflict, code, msg) }
	switch {
	case isRetryable(err):
		writeRetry(w)
	case deniedWithCause(err):
		h.internal(w, op, err)
	case errors.Is(err, review.ErrDenied):
		auth.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.Is(err, review.ErrNotFound):
		notFound(w)
	case errors.Is(err, review.ErrReasonRequired):
		auth.WriteError(w, http.StatusUnprocessableEntity, "reason_required", "Give the reason for this action.")
	case errors.Is(err, review.ErrInvalidReason):
		auth.WriteError(w, http.StatusUnprocessableEntity, "invalid_reason", "The reason must be text of at most 2000 characters.")
	case errors.Is(err, review.ErrStalePointer):
		conflict("stale_pointer", "The published revision is not the one expected. Nothing was changed; reload and retry.")
	case errors.Is(err, review.ErrNotApproved):
		conflict("not_approved", "Only an approved revision can be withdrawn or restored.")
	case errors.Is(err, review.ErrAlreadyWithdrawn):
		conflict("already_withdrawn", "This revision is already withdrawn.")
	case errors.Is(err, review.ErrWithdrawn):
		conflict("withdrawn", "This revision was withdrawn and can never be published or restored.")
	case errors.Is(err, review.ErrNotPublished):
		conflict("not_published", "The lesson has no published revision.")
	case errors.Is(err, review.ErrNeverPublished):
		conflict("never_published", "Only a revision that was published before can be restored. Publish an approved candidate instead.")
	case errors.Is(err, review.ErrAlreadyCurrent):
		conflict("already_current", "This revision is already the published revision.")
	case errors.Is(err, review.ErrArchived):
		conflict("archived", "The lesson is archived and accepts no changes.")
	case errors.Is(err, review.ErrIneligible):
		conflict("ineligible", "This revision no longer passes the content or media checks and cannot be restored.")
	case errors.Is(err, review.ErrRejected):
		conflict("immutable", "The change was rejected by a database guard.")
	default:
		h.internal(w, op, err)
	}
}
