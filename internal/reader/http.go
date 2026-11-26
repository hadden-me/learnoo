package reader

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/i18n"
	"github.com/hadden-me/learnoo/internal/invitations"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/rendering"
	"github.com/hadden-me/learnoo/internal/reports"
	"github.com/hadden-me/learnoo/internal/users"
	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed templates/*.html static/*
var files embed.FS

// CacheControl is sent on every reader response but fingerprinted assets
// (AssetCacheControl): pages depend on the session and must never be stored
// by a shared cache (or by the browser).
const CacheControl = "private, no-store"

// Content security policies. Forms (sign-in, invitation, navigation) load
// only same-origin styles. Lesson pages add the same-origin KaTeX script and
// fonts, inline style attributes produced by KaTeX, and the youtube-nocookie
// frame that internal/rendering's click-to-load button creates.
const (
	cspPage   = "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	cspLesson = "default-src 'none'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self'; font-src 'self'; " +
		"frame-src https://www.youtube-nocookie.com; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
)

// csrfField is the form field carrying the session's CSRF token.
const csrfField = "csrf_token"

const maxFormBytes = 8 << 10

// maxReportFormBytes bounds the error report form: 2000 characters of up to
// 4 UTF-8 bytes each, percent-encoded (3 bytes per byte), plus the other
// fields.
const maxReportFormBytes = 32 << 10

// Config wires the reader to the application.
type Config struct {
	Auth        *auth.Handlers
	Service     *auth.Service
	Store       *Store
	Invitations *invitations.Store
	Reports     *reports.Store
	// Media holds uploaded images embedded in published lessons.
	Media *media.Store
	Log   *slog.Logger
}

// Handlers serves the reader pages.
type Handlers struct {
	Config
	pages  map[string]map[i18n.Locale]*template.Template
	assets manifest
}

// New parses the templates and fingerprints the public assets; it panics on
// an error in either (a build defect).
func New(cfg Config) *Handlers {
	assets, err := embeddedAssets()
	if err != nil {
		panic(err)
	}
	funcs := template.FuncMap{"dir": Dir, "asset": assets.url}
	pages := map[string]map[i18n.Locale]*template.Template{}
	for _, name := range append([]string{"login", "invite", "dashboard", "subject", "lesson", "reported", "search", "error"}, coursePages...) {
		set, err := localized(funcs, false, []string{"templates/layout.html"}, name)
		if err != nil {
			panic(err)
		}
		pages[name] = set
	}
	return &Handlers{Config: cfg, pages: pages, assets: assets}
}

// Register mounts the reader routes documented in docs/API.md.
func (h *Handlers) Register(mux *http.ServeMux) {
	page := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, headers(i18n.Middleware(h.Auth.Middleware(fn))))
	}
	page("GET /login", h.loginPage)
	page("POST /login", h.loginSubmit)
	page("POST /logout", h.logout)
	page("GET /invite/{token}", h.invitePage)
	page("POST /invite", h.inviteSubmit)
	page("GET /learn", h.requireUser(h.dashboard))
	page("GET /learn/search", h.requireUser(h.search))
	page("GET /learn/subjects/{code}", h.requireUser(h.subject))
	page("GET /learn/lessons/{publicId}", h.requireUser(h.lesson))
	page("POST /learn/lessons/{publicId}/bookmark", h.requireUser(h.requireCSRF(h.addBookmark)))
	page("POST /learn/lessons/{publicId}/unbookmark", h.requireUser(h.requireCSRF(h.removeBookmark)))
	page("POST /learn/lessons/{publicId}/report", h.requireUser(h.requireCSRFBody(maxReportFormBytes, h.report, h.reportTooLarge)))
	page("GET /learn/lessons/{publicId}/reported", h.requireUser(h.reported))
	page("GET /learn/lessons/{publicId}/media/{mediaId}", h.lessonMedia)
	h.registerCourses(page)
	mux.Handle("GET /static/{path...}", headers(h.Auth.Middleware(http.HandlerFunc(h.static))))
	mux.Handle("GET "+AssetPrefix+"{version}/{path...}", http.HandlerFunc(h.serveAsset))
	h.registerPublic(mux) // public.go (LMS-B07)
}

// headers applies the reader's cache rule and default CSP; handlers may
// replace the CSP.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", CacheControl)
		w.Header().Set("Content-Security-Policy", cspPage)
		next.ServeHTTP(w, r)
	})
}

// nextPattern limits post-login redirects to reader and Studio paths made
// of plain segments: no scheme, host, query or "//".
var nextPattern = regexp.MustCompile(`^/(?:learn|studio)(/[A-Za-z0-9._~-]+)*$`)

// SafeNext returns next if it is a reader or Studio path the sign-in page
// may redirect to, and /learn otherwise.
func SafeNext(next string) string { return safeNext(next) }

func safeNext(next string) string {
	if nextPattern.MatchString(next) && !strings.Contains(next, "/..") && !strings.Contains(next, "/./") {
		return next
	}
	return "/learn"
}

// requireUser sends anonymous readers to the sign-in page (safe methods)
// or answers 401 (mutations). It reveals nothing about the requested lesson.
func (h *Handlers) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.SessionFrom(r.Context()); ok {
			next(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(safeNext(r.URL.Path)), http.StatusSeeOther)
			return
		}
		h.errorPage(w, r, http.StatusUnauthorized, "error.signInRequired", "error.signInRequiredText")
	}
}

// parseForm reads a bounded application/x-www-form-urlencoded body.
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	return parseFormLimit(w, r, maxFormBytes) == nil
}

// errNotForm means the body is not application/x-www-form-urlencoded.
var errNotForm = errors.New("not a form body")

// parseFormLimit reads a form body of at most limit bytes. A larger body
// returns an *http.MaxBytesError.
func parseFormLimit(w http.ResponseWriter, r *http.Request, limit int64) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		return errNotForm
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return r.ParseForm()
}

// requireCSRF checks the session's CSRF token in the form body. Go's
// cross-origin protection (internal/server) has already rejected
// cross-origin form posts; this is the second, session-bound layer.
func (h *Handlers) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return h.requireCSRFBody(maxFormBytes, next, nil)
}

// requireCSRFBody is requireCSRF for a form body of at most limit bytes.
// A larger body is never accepted: its CSRF token cannot be checked and
// next does not run. If tooLarge is set, it answers instead of the generic
// 403 page; it must not change state.
func (h *Handlers) requireCSRFBody(limit int64, next, tooLarge http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := auth.SessionFrom(r.Context())
		var err error
		if ok {
			err = parseFormLimit(w, r, limit)
		}
		var big *http.MaxBytesError
		if ok && tooLarge != nil && errors.As(err, &big) {
			tooLarge(w, r)
			return
		}
		if !ok || err != nil || !validCSRF(r.PostForm.Get(csrfField), sess.CSRFToken) {
			h.errorPage(w, r, http.StatusForbidden, "error.refused", "error.refusedText")
			return
		}
		next(w, r)
	}
}

func validCSRF(got, want string) bool {
	return got != "" && want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// view is the data every template receives.
type view struct {
	Title string
	// TitleLang is set when Title is not French (an Arabic catalogue name).
	TitleLang string
	User      *users.User
	CSRF      string
	Lesson    bool // loads KaTeX
	Data      any
	Status    int
	Heading   string
	Message   string
	BackLink  string
	// SwitchURL is the language switch's target (render sets it).
	SwitchURL string
	// Self is the page the language switch returns to when the response
	// answers a form (POST); GET pages switch in place.
	Self string
}

func (h *Handlers) render(w http.ResponseWriter, r *http.Request, status int, name string, v view) {
	if sess, ok := auth.SessionFrom(r.Context()); ok {
		u := sess.User
		v.User = &u
		v.CSRF = sess.CSRFToken
	}
	loc := locale(r)
	fallback := v.Self
	if fallback == "" {
		fallback = "/login"
		if v.User != nil {
			fallback = "/learn"
		}
	}
	v.SwitchURL = i18n.SwitchURL(r, loc, fallback)
	var buf bytes.Buffer
	if err := h.pages[name][loc].ExecuteTemplate(&buf, "layout", v); err != nil {
		h.Log.Error("reader template", "page", name, "err", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(i18n.T(loc, "error.internalPlain") + "\n"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = buf.WriteTo(w)
	}
}

// ErrorPage renders the reader's error page (used by the Studio shell for
// its 403 page) in the request's interface locale; heading and message are
// message keys (internal/i18n). The caller sets the response's cache and
// CSP headers.
func (h *Handlers) ErrorPage(w http.ResponseWriter, r *http.Request, status int, heading, message string) {
	h.errorPage(w, r, status, heading, message)
}

// errorPage renders the error page; heading and message are message keys,
// message formatted with args.
func (h *Handlers) errorPage(w http.ResponseWriter, r *http.Request, status int, heading, message string, args ...any) {
	title := msg(r, heading)
	h.render(w, r, status, "error", view{Title: title, Heading: title, Message: msg(r, message, args...), Status: status})
}

func (h *Handlers) notFound(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusNotFound, "error.lessonUnavailable", "error.lessonUnavailableText")
}

func (h *Handlers) internal(w http.ResponseWriter, r *http.Request, op string, err error) {
	// Never log tokens, passwords, form bodies or queries.
	h.Log.Error("reader failure", "op", op, "err", err)
	h.errorPage(w, r, http.StatusInternalServerError, "error.internal", "error.internalText")
}

func (h *Handlers) rateLimited(w http.ResponseWriter, r *http.Request, err *auth.RateLimitedError) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(err.RetryAfter.Seconds()))))
	h.errorPage(w, r, http.StatusTooManyRequests, "error.tooMany", "error.tooManyText")
}

// ---- sign-in and sign-out ----

type loginData struct {
	Email string
	Next  string
	Error string
}

func (h *Handlers) loginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if _, ok := auth.SessionFrom(r.Context()); ok {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	h.render(w, r, http.StatusOK, "login", view{Title: msg(r, "login.title"), Data: loginData{Next: next}})
}

func (h *Handlers) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		h.render(w, r, http.StatusBadRequest, "login", view{Title: msg(r, "login.title"),
			Data: loginData{Next: "/learn", Error: msg(r, "login.missing")}})
		return
	}
	email, password := r.PostForm.Get("email"), r.PostForm.Get("password")
	data := loginData{Email: email, Next: safeNext(r.PostForm.Get("next"))}
	if email == "" || password == "" {
		data.Error = msg(r, "login.missing")
		h.render(w, r, http.StatusBadRequest, "login", view{Title: msg(r, "login.title"), Data: data})
		return
	}
	var previous string
	if c, err := r.Cookie(auth.CookieName); err == nil {
		previous = c.Value
	}
	token, sess, err := h.Service.Login(r.Context(), email, password, auth.ClientAddr(r), previous)
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(limited.RetryAfter.Seconds()))))
		data.Error = msg(r, "login.tooMany")
		h.render(w, r, http.StatusTooManyRequests, "login", view{Title: msg(r, "login.title"), Data: data})
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		data.Error = msg(r, "login.invalid")
		h.render(w, r, http.StatusUnauthorized, "login", view{Title: msg(r, "login.title"), Data: data})
		return
	case err != nil:
		h.internal(w, r, "login", err)
		return
	}
	auth.SetSessionCookie(w, token, sess)
	http.Redirect(w, r, data.Next, http.StatusSeeOther)
}

func (h *Handlers) logout(w http.ResponseWriter, r *http.Request) {
	sess, ok := auth.SessionFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !parseForm(w, r) || !validCSRF(r.PostForm.Get(csrfField), sess.CSRFToken) {
		h.errorPage(w, r, http.StatusForbidden, "error.refused", "error.refusedText")
		return
	}
	token, _ := auth.SessionToken(r.Context())
	if err := h.Service.Logout(r.Context(), token); err != nil {
		h.internal(w, r, "logout", err)
		return
	}
	auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- invitations ----

type inviteData struct {
	Token       string
	Email       string
	DisplayName string
	Errors      map[string]string
	MinPassword int
}

func (h *Handlers) throttleInvitation(w http.ResponseWriter, r *http.Request) bool {
	err := h.Service.ThrottleInvitation(r.Context(), auth.ClientAddr(r))
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		h.rateLimited(w, r, limited)
		return false
	case err != nil:
		h.internal(w, r, "throttle invitation", err)
		return false
	}
	return true
}

func (h *Handlers) invalidInvitation(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusNotFound, "error.invitation", "error.invitationText")
}

func (h *Handlers) invitePage(w http.ResponseWriter, r *http.Request) {
	// The token is in this URL only: keep it out of Referer headers.
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !h.throttleInvitation(w, r) {
		return
	}
	token := r.PathValue("token")
	email, err := h.Invitations.Pending(r.Context(), token)
	if errors.Is(err, invitations.ErrInvalid) {
		h.invalidInvitation(w, r)
		return
	}
	if err != nil {
		h.internal(w, r, "load invitation", err)
		return
	}
	h.render(w, r, http.StatusOK, "invite", view{Title: msg(r, "invite.title"),
		Data: inviteData{Token: token, Email: email, MinPassword: invitations.MinPasswordLen}})
}

func (h *Handlers) inviteSubmit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !h.throttleInvitation(w, r) {
		return
	}
	if !parseForm(w, r) {
		h.invalidInvitation(w, r)
		return
	}
	f := r.PostForm
	token := f.Get("token")
	email, err := h.Invitations.Pending(r.Context(), token)
	if errors.Is(err, invitations.ErrInvalid) {
		h.invalidInvitation(w, r)
		return
	}
	if err != nil {
		h.internal(w, r, "load invitation", err)
		return
	}
	data := inviteData{Token: token, Email: email, DisplayName: f.Get("display_name"), Errors: map[string]string{}, MinPassword: invitations.MinPasswordLen}
	name, err := invitations.NormalizeDisplayName(data.DisplayName)
	if err != nil {
		data.Errors["display_name"] = msg(r, "invite.nameInvalid")
	}
	password := f.Get("password")
	if invitations.CheckPassword(password) != nil {
		data.Errors["password"] = msg(r, "invite.passwordShort", invitations.MinPasswordLen)
	} else if f.Get("password_confirm") != password {
		data.Errors["password_confirm"] = msg(r, "invite.passwordMismatch")
	}
	if len(data.Errors) > 0 {
		h.render(w, r, http.StatusUnprocessableEntity, "invite", view{Title: msg(r, "invite.title"), Data: data, Self: "/invite/" + token})
		return
	}
	hash, err := h.Service.HashPassword(password)
	if err != nil {
		h.internal(w, r, "hash password", err)
		return
	}
	u, err := h.Invitations.Accept(r.Context(), token, name, hash)
	if errors.Is(err, invitations.ErrInvalid) || errors.Is(err, invitations.ErrEmailTaken) {
		h.invalidInvitation(w, r)
		return
	}
	if err != nil {
		h.internal(w, r, "accept invitation", err)
		return
	}
	// Sign the new student in through the normal login path (throttling,
	// session rotation). If that fails, the account still exists.
	var previous string
	if c, err := r.Cookie(auth.CookieName); err == nil {
		previous = c.Value
	}
	sessToken, sess, err := h.Service.Login(r.Context(), u.Email, password, auth.ClientAddr(r), previous)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	auth.SetSessionCookie(w, sessToken, sess)
	http.Redirect(w, r, "/learn", http.StatusSeeOther)
}

// ---- reader pages ----

func userID(ctx context.Context) int64 {
	s, _ := auth.SessionFrom(ctx)
	return s.User.ID
}

type dashboardData struct {
	Bookmarks []Bookmark
	Subjects  []Subject
	Home      homeData // LMS-B08: preference, courses, recent material
}

func (h *Handlers) dashboard(w http.ResponseWriter, r *http.Request) {
	bms, err := h.Store.Bookmarks(r.Context(), userID(r.Context()))
	if err != nil {
		h.internal(w, r, "bookmarks", err)
		return
	}
	home, subjects, err := h.home(r)
	if err != nil {
		h.internal(w, r, "home", err)
		return
	}
	h.render(w, r, http.StatusOK, "dashboard", view{Title: msg(r, "nav.home"), Data: dashboardData{Bookmarks: bms, Subjects: subjects, Home: home}})
}

type subjectData struct {
	Subject  Subject
	Chapters []Chapter
}

func (h *Handlers) subject(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	subject, chapters, err := h.Store.SubjectChapters(r.Context(), code)
	if errors.Is(err, ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.internal(w, r, "subject", err)
		return
	}
	label := subject.Label(string(locale(r)))
	v := view{Title: label.Text, TitleLang: label.Lang, Data: subjectData{Subject: subject, Chapters: chapters}}
	h.render(w, r, http.StatusOK, "subject", v)
}

type lessonData struct {
	Lesson
	Parts  rendering.LessonParts
	Report reportForm
}

// reportForm is the state of the "Signaler une erreur" form on the lesson
// page: empty and closed, or reopened with the student's text and an error.
type reportForm struct {
	Open    bool
	Message string
	Error   string
	Max     int
}

// Dir returns the base direction of a lesson language.
func Dir(lang string) string { return rendering.Dir(lang) }

func (h *Handlers) lesson(w http.ResponseWriter, r *http.Request) {
	h.showLesson(w, r, http.StatusOK, reportForm{})
}

// showLesson renders the published lesson page with the report form in the
// given state.
func (h *Handlers) showLesson(w http.ResponseWriter, r *http.Request, status int, form reportForm) {
	l, err := h.Store.Lesson(r.Context(), userID(r.Context()), r.PathValue("publicId"))
	if errors.Is(err, ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.internal(w, r, "lesson", err)
		return
	}
	// The same parts as the Studio preview (POST /api/preview): heading,
	// objective and body come from the published document.
	parts, err := rendering.RenderLesson(l.Document, LessonMediaURL(l.PublicID))
	if err != nil {
		h.internal(w, r, "render lesson", err)
		return
	}
	form.Max = reports.MaxMessage
	w.Header().Set("Content-Security-Policy", cspLesson)
	h.render(w, r, status, "lesson", view{Title: l.Title, TitleLang: l.Language, Lesson: true, Self: lessonPath(l.PublicID),
		Data: lessonData{Lesson: l, Parts: parts, Report: form}})
}

// LessonMediaURL is the reader's MediaURL for a published lesson: embedded
// uploaded images are fetched through lessonMedia, scoped to the lesson.
func LessonMediaURL(publicID string) rendering.MediaURL {
	return rendering.MediaPrefix(lessonPath(publicID) + "/media/")
}

func lessonPath(publicID string) string { return "/learn/lessons/" + publicID }

// mediaIDPattern accepts the server's media IDs: positive decimal, no sign
// or leading zeros, at most 18 digits (fits in int64).
var mediaIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// lessonMedia serves an uploaded image embedded in the lesson's currently
// published revision (Store.LessonMedia) to any signed-in account. Every
// other request (anonymous, malformed or unknown IDs, an image of another
// lesson, of a draft or of an unpublished, withdrawn or archived lesson, or
// attached to the revision but not embedded in it) gets the same JSON 404,
// as protected /static media does: no redirect, so nothing about the lesson
// is revealed.
func (h *Handlers) lessonMedia(w http.ResponseWriter, r *http.Request) {
	publicID, raw := r.PathValue("publicId"), r.PathValue("mediaId")
	if _, ok := auth.SessionFrom(r.Context()); !ok || !ValidPublicID(publicID) || !mediaIDPattern.MatchString(raw) {
		staticNotFound(w)
		return
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		staticNotFound(w)
		return
	}
	key, err := h.Store.LessonMedia(r.Context(), publicID, id)
	if errors.Is(err, ErrNotFound) {
		staticNotFound(w)
		return
	}
	if err != nil {
		h.Log.Error("reader failure", "op", "lesson media", "err", err)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "Something went wrong. Try again later.")
		return
	}
	if err := h.Media.Serve(w, r, key); err != nil {
		h.Log.Error("reader media unavailable", "id", id, "err", err)
		staticNotFound(w)
	}
}

// revisionField accepts a positive decimal revision number.
var revisionField = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

// report files an error report on the published revision the student was
// reading (LRN-015). Every accepted submission counts against the
// per-account and per-address throttle, valid or not.
func (h *Handlers) report(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("publicId")
	if !ValidPublicID(id) {
		h.notFound(w, r)
		return
	}
	err := h.Service.ThrottleErrorReport(r.Context(), userID(r.Context()), auth.ClientAddr(r))
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		h.rateLimited(w, r, limited)
		return
	case err != nil:
		h.internal(w, r, "throttle error report", err)
		return
	}
	raw := r.PostForm.Get("revision")
	if len(r.PostForm["revision"]) != 1 || len(r.PostForm["message"]) != 1 || !revisionField.MatchString(raw) {
		h.errorPage(w, r, http.StatusBadRequest, "error.invalid", "error.invalidText")
		return
	}
	number, _ := strconv.Atoi(raw)
	message := r.PostForm.Get("message")
	// Keep at most a little more than the limit when re-displaying text.
	form := reportForm{Open: true, Message: truncateRunes(message, reports.MaxMessage+100)}
	_, err = h.Reports.Create(r.Context(), userID(r.Context()), id, number, message)
	switch {
	case err == nil:
		http.Redirect(w, r, "/learn/lessons/"+id+"/reported", http.StatusSeeOther)
	case errors.Is(err, reports.ErrTextRequired):
		form.Error = msg(r, "report.required")
		h.showLesson(w, r, http.StatusUnprocessableEntity, form)
	case errors.Is(err, reports.ErrInvalidText):
		form.Error = msg(r, "report.invalidText", reports.MaxMessage)
		h.showLesson(w, r, http.StatusUnprocessableEntity, form)
	case errors.Is(err, reports.ErrNotFound):
		h.notFound(w, r)
	case errors.Is(err, reports.ErrRevisionChanged):
		// The published revision changed since it was displayed. Show the
		// current version with the student's text kept; nothing was
		// stored. A lesson that is no longer published gets 404.
		form.Error = msg(r, "report.lessonChanged")
		h.showLesson(w, r, http.StatusConflict, form)
	case errors.Is(err, reports.ErrRejected), isRetryable(err):
		// A concurrent change or a database guard refused the report;
		// nothing was stored and the request can be sent again.
		form.Error = msg(r, "report.retry")
		h.showLesson(w, r, http.StatusConflict, form)
	default:
		h.internal(w, r, "error report", err)
	}
}

// reportTooLarge answers a report form body over maxReportFormBytes (text
// pasted far beyond the limit). Nothing is stored or throttled, since the
// CSRF token was not checked; it only shows the lesson again, read-only.
// The truncated text cannot be kept.
func (h *Handlers) reportTooLarge(w http.ResponseWriter, r *http.Request) {
	if !ValidPublicID(r.PathValue("publicId")) {
		h.notFound(w, r)
		return
	}
	h.showLesson(w, r, http.StatusRequestEntityTooLarge, reportForm{Open: true,
		Error: msg(r, "report.tooLong", reports.MaxMessage)})
}

// reported confirms a filed report. It shows no lesson data.
func (h *Handlers) reported(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("publicId")
	if !ValidPublicID(id) {
		h.notFound(w, r)
		return
	}
	h.render(w, r, http.StatusOK, "reported", view{Title: msg(r, "reported.title"), Data: id})
}

// isRetryable reports a deadlock (40P01) or serialization failure (40001):
// the transaction rolled back and stored nothing.
func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

type searchData struct {
	Query    string
	Searched bool
	Invalid  bool
	Results  []LessonSummary
	Courses  []CourseSummary // LMS-B08: published courses matching the query
	Max      int
	Min      int
}

func (h *Handlers) search(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("q")
	data := searchData{Query: raw, Max: MaxSearchResults, Min: MinSearchRunes}
	if raw != "" {
		q, ok := NormalizeQuery(raw)
		data.Query = q
		data.Searched = ok
		data.Invalid = !ok
		if ok {
			res, err := h.Store.Search(r.Context(), q)
			switch {
			case errors.Is(err, ErrInvalidQuery):
				data.Searched, data.Invalid = false, true
			case err != nil:
				h.internal(w, r, "search", err)
				return
			}
			data.Results = res
			if data.Searched {
				if data.Courses, err = h.Store.SearchCourses(r.Context(), q); err != nil && !errors.Is(err, ErrInvalidQuery) {
					h.internal(w, r, "search courses", err)
					return
				}
			}
		}
	}
	h.render(w, r, http.StatusOK, "search", view{Title: msg(r, "search.title"), Data: data})
}

func bookmarkReturn(r *http.Request, publicID string) string {
	if r.PostForm.Get("return") == "dashboard" {
		return "/learn"
	}
	return "/learn/lessons/" + publicID
}

func (h *Handlers) addBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("publicId")
	err := h.Store.AddBookmark(r.Context(), userID(r.Context()), id)
	switch {
	case errors.Is(err, ErrNotFound):
		h.notFound(w, r)
	case errors.Is(err, ErrTooManyBookmarks):
		h.errorPage(w, r, http.StatusConflict, "error.tooManyBookmarks", "error.tooManyBookmarksText", MaxBookmarks)
	case err != nil:
		h.internal(w, r, "add bookmark", err)
	default:
		http.Redirect(w, r, bookmarkReturn(r, id), http.StatusSeeOther)
	}
}

func (h *Handlers) removeBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("publicId")
	err := h.Store.RemoveBookmark(r.Context(), userID(r.Context()), id)
	switch {
	case errors.Is(err, ErrNotFound):
		h.notFound(w, r)
	case err != nil:
		h.internal(w, r, "remove bookmark", err)
	default:
		http.Redirect(w, r, bookmarkReturn(r, id), http.StatusSeeOther)
	}
}

// ---- static assets ----

// Unversioned static files. Public assets (isPublicAsset: generic styles and
// scripts with no lesson content, and KaTeX) are also served fingerprinted
// under /assets/ (assets.go), which pages use; these legacy URLs stay
// private, no-store. Everything else under /static is lesson media (today
// only the renderer's fixture image) and needs a session; anonymous requests
// get 404, as do unknown paths and directories.
var protectedMedia = map[string]bool{"complex-plane.svg": true}

func (h *Handlers) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("path")
	var fsys fs.FS
	switch {
	case !fs.ValidPath(name):
	case name == "reader.css":
		fsys, _ = fs.Sub(files, "static")
	case isPublicAsset(name):
		fsys, _ = fs.Sub(rendering.Files, "static")
	case protectedMedia[name]:
		if _, ok := auth.SessionFrom(r.Context()); ok {
			fsys, _ = fs.Sub(rendering.Files, "static")
		}
	}
	if fsys == nil {
		staticNotFound(w)
		return
	}
	info, err := fs.Stat(fsys, name)
	if err != nil || !info.Mode().IsRegular() {
		staticNotFound(w)
		return
	}
	http.ServeFileFS(w, r, fsys, name)
}

func staticNotFound(w http.ResponseWriter) {
	auth.WriteError(w, http.StatusNotFound, "not_found", "Not found.")
}
