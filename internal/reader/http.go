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

	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/invitations"
	"github.com/hadden-me/learnoo/internal/rendering"
	"github.com/hadden-me/learnoo/internal/users"
)

//go:embed templates/*.html static/*
var files embed.FS

// CacheControl is sent on every reader response: pages depend on the
// session and must never be stored by a shared cache (or by the browser).
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

// Config wires the reader to the application.
type Config struct {
	Auth        *auth.Handlers
	Service     *auth.Service
	Store       *Store
	Invitations *invitations.Store
	Log         *slog.Logger
}

// Handlers serves the reader pages.
type Handlers struct {
	Config
	pages map[string]*template.Template
}

// New parses the templates; it panics on a template error (a build defect).
func New(cfg Config) *Handlers {
	base := template.Must(template.New("").Funcs(template.FuncMap{"dir": Dir}).ParseFS(files, "templates/layout.html"))
	pages := map[string]*template.Template{}
	for _, name := range []string{"login", "invite", "dashboard", "subject", "lesson", "search", "error"} {
		pages[name] = template.Must(template.Must(base.Clone()).ParseFS(files, "templates/"+name+".html"))
	}
	return &Handlers{Config: cfg, pages: pages}
}

// Register mounts the reader routes documented in docs/API.md.
func (h *Handlers) Register(mux *http.ServeMux) {
	page := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, headers(h.Auth.Middleware(fn)))
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
	mux.Handle("GET /static/{path...}", headers(h.Auth.Middleware(http.HandlerFunc(h.static))))
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

// nextPattern limits post-login redirects to reader paths made of plain
// segments: no scheme, host, query or "//".
var nextPattern = regexp.MustCompile(`^/learn(/[A-Za-z0-9._~-]+)*$`)

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
		h.errorPage(w, r, http.StatusUnauthorized, "Connexion requise", "Connectez-vous pour continuer.")
	}
}

// parseForm reads a bounded application/x-www-form-urlencoded body.
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	return r.ParseForm() == nil
}

// requireCSRF checks the session's CSRF token in the form body. Go's
// cross-origin protection (internal/server) has already rejected
// cross-origin form posts; this is the second, session-bound layer.
func (h *Handlers) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := auth.SessionFrom(r.Context())
		if !ok || !parseForm(w, r) || !validCSRF(r.PostForm.Get(csrfField), sess.CSRFToken) {
			h.errorPage(w, r, http.StatusForbidden, "Requête refusée", "La requête n’a pas pu être vérifiée. Rechargez la page et réessayez.")
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
	Title    string
	User     *users.User
	CSRF     string
	Lesson   bool // loads KaTeX
	Data     any
	Status   int
	Heading  string
	Message  string
	BackLink string
}

func (h *Handlers) render(w http.ResponseWriter, r *http.Request, status int, name string, v view) {
	if sess, ok := auth.SessionFrom(r.Context()); ok {
		u := sess.User
		v.User = &u
		v.CSRF = sess.CSRFToken
	}
	var buf bytes.Buffer
	if err := h.pages[name].ExecuteTemplate(&buf, "layout", v); err != nil {
		h.Log.Error("reader template", "page", name, "err", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Erreur interne.\n"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = buf.WriteTo(w)
	}
}

func (h *Handlers) errorPage(w http.ResponseWriter, r *http.Request, status int, heading, message string) {
	h.render(w, r, status, "error", view{Title: heading, Heading: heading, Message: message, Status: status})
}

func (h *Handlers) notFound(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusNotFound, "Leçon indisponible",
		"Cette page n’existe pas ou cette leçon n’est pas publiée pour le moment.")
}

func (h *Handlers) internal(w http.ResponseWriter, r *http.Request, op string, err error) {
	// Never log tokens, passwords, form bodies or queries.
	h.Log.Error("reader failure", "op", op, "err", err)
	h.errorPage(w, r, http.StatusInternalServerError, "Erreur interne", "Un problème est survenu. Réessayez plus tard.")
}

func (h *Handlers) rateLimited(w http.ResponseWriter, r *http.Request, err *auth.RateLimitedError) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(err.RetryAfter.Seconds()))))
	h.errorPage(w, r, http.StatusTooManyRequests, "Trop de tentatives", "Trop de tentatives. Réessayez dans quelques minutes.")
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
	h.render(w, r, http.StatusOK, "login", view{Title: "Se connecter", Data: loginData{Next: next}})
}

func (h *Handlers) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		h.render(w, r, http.StatusBadRequest, "login", view{Title: "Se connecter",
			Data: loginData{Next: "/learn", Error: "Saisissez votre adresse e-mail et votre mot de passe."}})
		return
	}
	email, password := r.PostForm.Get("email"), r.PostForm.Get("password")
	data := loginData{Email: email, Next: safeNext(r.PostForm.Get("next"))}
	if email == "" || password == "" {
		data.Error = "Saisissez votre adresse e-mail et votre mot de passe."
		h.render(w, r, http.StatusBadRequest, "login", view{Title: "Se connecter", Data: data})
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
		data.Error = "Trop de tentatives de connexion. Réessayez dans quelques minutes."
		h.render(w, r, http.StatusTooManyRequests, "login", view{Title: "Se connecter", Data: data})
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		data.Error = "Adresse e-mail ou mot de passe incorrect."
		h.render(w, r, http.StatusUnauthorized, "login", view{Title: "Se connecter", Data: data})
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
		h.errorPage(w, r, http.StatusForbidden, "Requête refusée", "La requête n’a pas pu être vérifiée. Rechargez la page et réessayez.")
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
	h.errorPage(w, r, http.StatusNotFound, "Ce lien d’invitation ne fonctionne plus",
		"Il a peut-être expiré ou a déjà été utilisé. Demandez un nouveau lien à la personne qui vous a invité·e.")
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
	h.render(w, r, http.StatusOK, "invite", view{Title: "Activer votre compte",
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
		data.Errors["display_name"] = "Saisissez un nom de 1 à 100 caractères, sans caractères de contrôle."
	}
	password := f.Get("password")
	if invitations.CheckPassword(password) != nil {
		data.Errors["password"] = "Le mot de passe doit contenir au moins " + strconv.Itoa(invitations.MinPasswordLen) + " caractères."
	} else if f.Get("password_confirm") != password {
		data.Errors["password_confirm"] = "Les deux mots de passe ne correspondent pas."
	}
	if len(data.Errors) > 0 {
		h.render(w, r, http.StatusUnprocessableEntity, "invite", view{Title: "Activer votre compte", Data: data})
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
}

func (h *Handlers) dashboard(w http.ResponseWriter, r *http.Request) {
	bms, err := h.Store.Bookmarks(r.Context(), userID(r.Context()))
	if err != nil {
		h.internal(w, r, "bookmarks", err)
		return
	}
	subjects, err := h.Store.Subjects(r.Context())
	if err != nil {
		h.internal(w, r, "subjects", err)
		return
	}
	h.render(w, r, http.StatusOK, "dashboard", view{Title: "Mes leçons", Data: dashboardData{Bookmarks: bms, Subjects: subjects}})
}

type subjectData struct {
	Code     string
	Chapters []Chapter
}

func (h *Handlers) subject(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	chapters, err := h.Store.SubjectChapters(r.Context(), code)
	if errors.Is(err, ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.internal(w, r, "subject", err)
		return
	}
	h.render(w, r, http.StatusOK, "subject", view{Title: code, Data: subjectData{Code: code, Chapters: chapters}})
}

type lessonData struct {
	Lesson
	Dir  string
	Body template.HTML
}

// Dir returns the base direction of a lesson language.
func Dir(lang string) string {
	if lang == "ar" {
		return "rtl"
	}
	return "ltr"
}

func (h *Handlers) lesson(w http.ResponseWriter, r *http.Request) {
	l, err := h.Store.Lesson(r.Context(), userID(r.Context()), r.PathValue("publicId"))
	if errors.Is(err, ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.internal(w, r, "lesson", err)
		return
	}
	body, err := rendering.RenderBlocks(l.Document)
	if err != nil {
		h.internal(w, r, "render lesson", err)
		return
	}
	w.Header().Set("Content-Security-Policy", cspLesson)
	h.render(w, r, http.StatusOK, "lesson", view{Title: l.Title, Lesson: true,
		Data: lessonData{Lesson: l, Dir: Dir(l.Language), Body: body}})
}

type searchData struct {
	Query    string
	Searched bool
	Invalid  bool
	Results  []LessonSummary
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
		}
	}
	h.render(w, r, http.StatusOK, "search", view{Title: "Rechercher une leçon", Data: data})
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
		h.errorPage(w, r, http.StatusConflict, "Trop de signets",
			"Vous avez atteint la limite de "+strconv.Itoa(MaxBookmarks)+" signets. Retirez-en avant d’en ajouter.")
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

// Public assets are generic styles and scripts with no lesson content.
// Everything else under /static is lesson media (today only the renderer's
// fixture image) and needs a session; anonymous requests get 404, as do
// unknown paths and directories.
var (
	publicRendering = map[string]bool{"tokens.css": true, "site.css": true, "math.js": true}
	protectedMedia  = map[string]bool{"complex-plane.svg": true}
)

func (h *Handlers) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("path")
	var fsys fs.FS
	switch {
	case !fs.ValidPath(name):
	case name == "reader.css":
		fsys, _ = fs.Sub(files, "static")
	case publicRendering[name], strings.HasPrefix(name, "vendor/katex/"):
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
