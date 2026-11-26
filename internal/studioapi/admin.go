package studioapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/invitations"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/users"
)

// Administration endpoints (LMS-B10), documented in docs/API.md
// "Administration (LMS-B10)": the account directory, global and reviewer
// grants, account status, student invitations and catalogue display names
// and order.
//
// This is the only HTTP code allowed to call the permission and account
// status change methods (permissions.Store Grant, Revoke, GrantReviewer,
// RevokeReviewer; accounts.Store Disable, Enable), and every such call must
// pass permissions.UserActor(actor(r)) as its actor, the signed-in
// administrator: internal/server TestNoHTTPPathChangesPermissions enforces
// both. The stores keep every rule of the operator path (administrator
// recheck inside the transaction, administrators lock, last-active-
// administrator guard, same-transaction audit); this file adds only the
// refusal of self-grants and self-disable.
//
// Every route needs a session, the CSRF token on unsafe methods and the
// administrator permission, checked by the middleware before any path value
// or body is read, so other accounts get 403 for existing and missing
// accounts alike.

func (h *Handlers) registerAdmin(mux *http.ServeMux) {
	route := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, h.Auth.Middleware(h.Auth.RequireUser(h.Auth.RequireCSRF(
			h.Perms.Require(permissions.Administrator, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", privateNoStore)
				fn(w, r)
			}))))))
	}
	route("GET /api/admin/users", h.listUsers)
	route("GET /api/admin/users/{userId}", h.getUser)
	route("POST /api/admin/users/{userId}/grant", h.grantPermission)
	route("POST /api/admin/users/{userId}/revoke", h.revokePermission)
	route("POST /api/admin/users/{userId}/disable", h.disableUser)
	route("POST /api/admin/users/{userId}/enable", h.enableUser)
	route("POST /api/admin/invitations", h.issueInvitation)
	route("GET /api/admin/catalogue", h.adminCatalogue)
	route("PUT /api/admin/catalogue/subjects/{subjectCode}/names", h.setSubjectNames)
	route("PUT /api/admin/catalogue/subjects/{subjectCode}/order", h.setSubjectOrder)
	route("PUT /api/admin/catalogue/subjects/{subjectCode}/chapters/{chapterCode}/names", h.setChapterNames)
	route("PUT /api/admin/catalogue/subjects/{subjectCode}/chapters/{chapterCode}/order", h.setChapterOrder)
}

// ---- JSON ----

type reviewerSubjectJSON struct {
	ID        int64      `json:"id"`
	Code      string     `json:"code"`
	NameFr    *string    `json:"nameFr"`
	NameAr    *string    `json:"nameAr"`
	RetiredAt *time.Time `json:"retiredAt"`
}

type adminUserJSON struct {
	ID               int64                 `json:"id"`
	Email            string                `json:"email"`
	DisplayName      string                `json:"displayName"`
	CreatedAt        time.Time             `json:"createdAt"`
	Disabled         bool                  `json:"disabled"`
	Permissions      []string              `json:"permissions"`
	ReviewerSubjects []reviewerSubjectJSON `json:"reviewerSubjects"`
}

func toAdminUserJSON(a users.Account) adminUserJSON {
	out := adminUserJSON{ID: a.ID, Email: a.Email, DisplayName: a.DisplayName, CreatedAt: a.CreatedAt, Disabled: a.Disabled,
		Permissions: a.Permissions, ReviewerSubjects: []reviewerSubjectJSON{}}
	for _, s := range a.ReviewerSubjects {
		out.ReviewerSubjects = append(out.ReviewerSubjects, reviewerSubjectJSON(s))
	}
	return out
}

type historyEventJSON struct {
	Kind          string    `json:"kind"`
	Action        string    `json:"action"`
	Permission    *string   `json:"permission"`
	SubjectID     *int64    `json:"subjectId"`
	SubjectCode   *string   `json:"subjectCode"`
	ActorID       *int64    `json:"actorId"`
	ActorName     *string   `json:"actorName"`
	OperatorLabel *string   `json:"operatorLabel"`
	OccurredAt    time.Time `json:"occurredAt"`
}

type grantRequest struct {
	Permission *string `json:"permission" api:"nonnull"`
	SubjectID  *int64  `json:"subjectId" api:"nonnull"`
}

type invitationRequest struct {
	Email    *string `json:"email" api:"nonnull"`
	TTLHours *int    `json:"ttlHours" api:"nonnull"`
}

type namesRequest struct {
	NameFr *string `json:"nameFr" api:"nonnull"`
	NameAr *string `json:"nameAr" api:"nonnull"`
}

type orderRequest struct {
	SortOrder *int `json:"sortOrder" api:"nonnull"`
}

var (
	grantKeys      = []string{"permission", "subjectId"}
	invitationKeys = []string{"email", "ttlHours"}
	namesKeys      = []string{"nameFr", "nameAr"}
	orderKeys      = []string{"sortOrder"}
)

// maxUserQuery bounds the search text of the account directory.
const maxUserQuery = 100

// ---- users ----

func (h *Handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	var f users.Filter
	ok := true
	for key, values := range r.URL.Query() {
		if len(values) != 1 || values[0] == "" {
			ok = false
			break
		}
		v := values[0]
		switch key {
		case "q":
			if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxUserQuery || strings.ContainsRune(v, 0) {
				ok = false
			}
			f.Query = v
		case "permission":
			f.Permission = v
			ok = v == "author" || v == "publisher" || v == "administrator" || v == "reviewer" || v == "none"
		case "status":
			f.Status = v
			ok = v == "active" || v == "disabled"
		case "after":
			f.After, ok = parseID(v)
		default:
			ok = false
		}
		if !ok {
			break
		}
	}
	if !ok {
		badRequest(w, "Optional query parameters, each at most once and not empty: q (1 to 100 characters), permission (author, publisher, administrator, reviewer or none), status (active or disabled) and after (a user ID).")
		return
	}
	list, next, err := users.List(r.Context(), h.DB, f)
	if err != nil {
		h.internal(w, "list users", err)
		return
	}
	out := struct {
		Users     []adminUserJSON `json:"users"`
		NextAfter *int64          `json:"nextAfter"`
	}{Users: []adminUserJSON{}}
	for _, a := range list {
		out.Users = append(out.Users, toAdminUserJSON(a))
	}
	if next != 0 {
		out.NextAfter = &next
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) getUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "userId")
	if !ok {
		return
	}
	if len(r.URL.Query()) != 0 {
		badRequest(w, "This endpoint takes no query parameters.")
		return
	}
	a, err := users.Get(r.Context(), h.DB, id)
	if errors.Is(err, users.ErrNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		h.internal(w, "get user", err)
		return
	}
	events, err := users.History(r.Context(), h.DB, id)
	if err != nil {
		h.internal(w, "user history", err)
		return
	}
	history := make([]historyEventJSON, 0, len(events))
	for _, e := range events {
		history = append(history, historyEventJSON(e))
	}
	writeJSON(w, http.StatusOK, struct {
		User    adminUserJSON      `json:"user"`
		History []historyEventJSON `json:"history"`
	}{toAdminUserJSON(a), history})
}

func (h *Handlers) grantPermission(w http.ResponseWriter, r *http.Request) {
	h.changePermission(w, r, true)
}

func (h *Handlers) revokePermission(w http.ResponseWriter, r *http.Request) {
	h.changePermission(w, r, false)
}

func (h *Handlers) changePermission(w http.ResponseWriter, r *http.Request, grant bool) {
	target, ok := pathID(w, r, "userId")
	if !ok {
		return
	}
	var req grantRequest
	if !decodeJSON(w, r, &req, grantKeys) {
		return
	}
	if req.Permission == nil {
		badRequest(w, "permission is required.")
		return
	}
	p, err := permissions.Parse(*req.Permission)
	if err != nil {
		badRequest(w, "permission must be author, publisher, administrator or reviewer.")
		return
	}
	if (p == permissions.Reviewer) != (req.SubjectID != nil) {
		badRequest(w, "subjectId is required for reviewer and refused for the global permissions.")
		return
	}
	if grant && target == actor(r) {
		writeSelfChange(w)
		return
	}
	var changed bool
	switch {
	case p == permissions.Reviewer && *req.SubjectID <= 0:
		err = permissions.ErrUnknownSubject
	case p == permissions.Reviewer && grant:
		changed, err = h.Permissions.GrantReviewer(r.Context(), permissions.UserActor(actor(r)), target, *req.SubjectID)
	case p == permissions.Reviewer:
		changed, err = h.Permissions.RevokeReviewer(r.Context(), permissions.UserActor(actor(r)), target, *req.SubjectID)
	case grant:
		changed, err = h.Permissions.Grant(r.Context(), permissions.UserActor(actor(r)), target, p)
	default:
		changed, err = h.Permissions.Revoke(r.Context(), permissions.UserActor(actor(r)), target, p)
	}
	h.writeUserChange(w, r, "change permission", target, changed, err)
}

func (h *Handlers) disableUser(w http.ResponseWriter, r *http.Request) {
	h.changeStatus(w, r, true)
}

func (h *Handlers) enableUser(w http.ResponseWriter, r *http.Request) {
	h.changeStatus(w, r, false)
}

func (h *Handlers) changeStatus(w http.ResponseWriter, r *http.Request, disable bool) {
	target, ok := pathID(w, r, "userId")
	if !ok {
		return
	}
	var req emptyRequest
	if !decodeJSON(w, r, &req, nil) {
		return
	}
	var changed bool
	var err error
	switch {
	case disable && target == actor(r):
		writeSelfChange(w)
		return
	case disable:
		changed, err = h.Accounts.Disable(r.Context(), permissions.UserActor(actor(r)), target)
	default:
		changed, err = h.Accounts.Enable(r.Context(), permissions.UserActor(actor(r)), target)
	}
	h.writeUserChange(w, r, "change account status", target, changed, err)
}

func writeSelfChange(w http.ResponseWriter) {
	auth.WriteError(w, http.StatusForbidden, "self_change",
		"You cannot grant permissions to your own account or disable it. Ask another administrator.")
}

// writeUserChange answers a grant, revocation or status change: the
// account as it is after the change, or the mapped error.
func (h *Handlers) writeUserChange(w http.ResponseWriter, r *http.Request, op string, target int64, changed bool, err error) {
	if err != nil {
		h.writeAdminError(w, op, err)
		return
	}
	a, err := users.Get(r.Context(), h.DB, target)
	if err != nil {
		h.writeAdminError(w, op, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		User    adminUserJSON `json:"user"`
		Changed bool          `json:"changed"`
	}{toAdminUserJSON(a), changed})
}

// ---- invitations ----

func (h *Handlers) issueInvitation(w http.ResponseWriter, r *http.Request) {
	var req invitationRequest
	if !decodeJSON(w, r, &req, invitationKeys) {
		return
	}
	if req.Email == nil {
		badRequest(w, "email is required.")
		return
	}
	ttl := invitations.DefaultTTL
	if req.TTLHours != nil {
		if *req.TTLHours < 1 || *req.TTLHours > int(invitations.MaxTTL/time.Hour) {
			invalidInput(w, "ttlHours must be a whole number of hours from 1 to 720.")
			return
		}
		ttl = time.Duration(*req.TTLHours) * time.Hour
	}
	token, inv, err := h.Invitations.IssueBy(r.Context(), actor(r), *req.Email, ttl)
	switch {
	case errors.Is(err, users.ErrInvalidEmail):
		invalidInput(w, "email is not a valid email address.")
		return
	case errors.Is(err, invitations.ErrEmailTaken):
		auth.WriteError(w, http.StatusConflict, "account_exists", "An account already exists for this email.")
		return
	case err != nil:
		h.writeAdminError(w, "issue invitation", err)
		return
	}
	type invitationJSON struct {
		ID        int64     `json:"id"`
		Email     string    `json:"email"`
		CreatedAt time.Time `json:"createdAt"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	writeJSON(w, http.StatusCreated, struct {
		Invitation invitationJSON `json:"invitation"`
		Path       string         `json:"path"`
	}{invitationJSON{inv.ID, inv.Email, inv.CreatedAt, inv.ExpiresAt}, "/invite/" + url.PathEscape(token)})
}

// ---- catalogue ----

type adminSubjectJSON struct {
	entryJSON
	Chapters []entryJSON `json:"chapters"`
}

func (h *Handlers) adminCatalogue(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) != 0 {
		badRequest(w, "This endpoint takes no query parameters.")
		return
	}
	tree, err := h.Catalogue.Tree(r.Context())
	if err != nil {
		h.internal(w, "admin catalogue", err)
		return
	}
	out := struct {
		Subjects []adminSubjectJSON `json:"subjects"`
	}{Subjects: []adminSubjectJSON{}}
	for _, s := range tree {
		sj := adminSubjectJSON{entryJSON: subjectEntry(s.Subject), Chapters: []entryJSON{}}
		for _, c := range s.Chapters {
			sj.Chapters = append(sj.Chapters, chapterEntry(c))
		}
		out.Subjects = append(out.Subjects, sj)
	}
	writeJSON(w, http.StatusOK, out)
}

// catalogueCodes reads the path codes; a malformed code is 404 like an
// unknown one.
func catalogueCodes(w http.ResponseWriter, r *http.Request, chapter bool) (string, string, bool) {
	subject, ch := r.PathValue("subjectCode"), r.PathValue("chapterCode")
	if !catalogue.ValidCode(subject) || (chapter && !catalogue.ValidCode(ch)) {
		notFound(w)
		return "", "", false
	}
	return subject, ch, true
}

func decodeNames(w http.ResponseWriter, r *http.Request) (*string, *string, bool) {
	var req namesRequest
	if !decodeJSON(w, r, &req, namesKeys) {
		return nil, nil, false
	}
	if req.NameFr == nil && req.NameAr == nil {
		badRequest(w, "nameFr or nameAr is required.")
		return nil, nil, false
	}
	return req.NameFr, req.NameAr, true
}

func decodeOrder(w http.ResponseWriter, r *http.Request) (int, bool) {
	var req orderRequest
	if !decodeJSON(w, r, &req, orderKeys) {
		return 0, false
	}
	if req.SortOrder == nil {
		badRequest(w, "sortOrder is required.")
		return 0, false
	}
	return *req.SortOrder, true
}

func (h *Handlers) setSubjectNames(w http.ResponseWriter, r *http.Request) {
	code, _, ok := catalogueCodes(w, r, false)
	if !ok {
		return
	}
	fr, ar, ok := decodeNames(w, r)
	if !ok {
		return
	}
	s, changed, err := h.Catalogue.SetSubjectNames(r.Context(), permissions.UserActor(actor(r)), code, fr, ar)
	h.writeSubjectChange(w, "rename subject", s, changed, err)
}

func (h *Handlers) setSubjectOrder(w http.ResponseWriter, r *http.Request) {
	code, _, ok := catalogueCodes(w, r, false)
	if !ok {
		return
	}
	order, ok := decodeOrder(w, r)
	if !ok {
		return
	}
	s, changed, err := h.Catalogue.SetSubjectOrder(r.Context(), permissions.UserActor(actor(r)), code, order)
	h.writeSubjectChange(w, "reorder subject", s, changed, err)
}

func (h *Handlers) setChapterNames(w http.ResponseWriter, r *http.Request) {
	subject, code, ok := catalogueCodes(w, r, true)
	if !ok {
		return
	}
	fr, ar, ok := decodeNames(w, r)
	if !ok {
		return
	}
	_, c, changed, err := h.Catalogue.SetChapterNames(r.Context(), permissions.UserActor(actor(r)), subject, code, fr, ar)
	h.writeChapterChange(w, "rename chapter", c, changed, err)
}

func (h *Handlers) setChapterOrder(w http.ResponseWriter, r *http.Request) {
	subject, code, ok := catalogueCodes(w, r, true)
	if !ok {
		return
	}
	order, ok := decodeOrder(w, r)
	if !ok {
		return
	}
	_, c, changed, err := h.Catalogue.SetChapterOrder(r.Context(), permissions.UserActor(actor(r)), subject, code, order)
	h.writeChapterChange(w, "reorder chapter", c, changed, err)
}

func (h *Handlers) writeSubjectChange(w http.ResponseWriter, op string, s catalogue.Subject, changed bool, err error) {
	if err != nil {
		h.writeAdminError(w, op, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Subject entryJSON `json:"subject"`
		Changed bool      `json:"changed"`
	}{subjectEntry(s), changed})
}

func (h *Handlers) writeChapterChange(w http.ResponseWriter, op string, c catalogue.Chapter, changed bool, err error) {
	if err != nil {
		h.writeAdminError(w, op, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Chapter entryJSON `json:"chapter"`
		Changed bool      `json:"changed"`
	}{chapterEntry(c), changed})
}

func invalidInput(w http.ResponseWriter, msg string) {
	auth.WriteError(w, http.StatusUnprocessableEntity, "invalid_input", msg)
}

// writeAdminError maps store errors to the codes in docs/API.md. A denial
// here comes from the recheck inside the store's transaction (the
// administrator permission was revoked, or the account disabled, after the
// middleware's check), so it is a plain 403.
func (h *Handlers) writeAdminError(w http.ResponseWriter, op string, err error) {
	switch {
	case isRetryable(err):
		writeRetry(w)
	case deniedWithCause(err):
		h.internal(w, op, err)
	case errors.Is(err, permissions.ErrDenied):
		auth.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.Is(err, permissions.ErrUnknownUser), errors.Is(err, users.ErrNotFound),
		errors.Is(err, catalogue.ErrNotFound), errors.Is(err, catalogue.ErrChapterNotFound), errors.Is(err, catalogue.ErrInvalidCode):
		notFound(w)
	case errors.Is(err, permissions.ErrLastAdministrator):
		auth.WriteError(w, http.StatusConflict, "last_administrator",
			"This is the last active administrator. Grant administrator to another active account first. Nothing was changed.")
	case errors.Is(err, permissions.ErrRetiredSubject):
		auth.WriteError(w, http.StatusUnprocessableEntity, "retired_subject", "The subject is retired; no reviewer can be newly assigned to it.")
	case errors.Is(err, permissions.ErrUnknownSubject):
		auth.WriteError(w, http.StatusUnprocessableEntity, "unknown_subject", "The subject does not exist.")
	case errors.Is(err, catalogue.ErrInvalidName):
		invalidInput(w, "Names are 1 to 200 characters, without control, invisible or bidirectional formatting characters.")
	case errors.Is(err, catalogue.ErrInvalidSortOrder):
		invalidInput(w, "sortOrder must be a whole number from 0 to 9999.")
	default:
		h.internal(w, op, err)
	}
}
