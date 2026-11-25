package permissions

import (
	"log/slog"
	"net/http"

	"github.com/hadden-me/learnoo/internal/auth"
)

// Middleware guards routes with server-side permission checks. It must run
// inside auth's session middleware. Anonymous requests get 401; signed-in
// users without the permission, or whose permission cannot be verified, get 403.
type Middleware struct {
	store *Store
	log   *slog.Logger
}

// NewMiddleware builds permission middleware.
func NewMiddleware(store *Store, log *slog.Logger) *Middleware {
	return &Middleware{store: store, log: log}
}

// Require allows only users holding the global permission p.
func (m *Middleware) Require(p Permission, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := auth.SessionFrom(r.Context())
		if !ok {
			auth.WriteError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}
		if err := m.store.Require(r.Context(), sess.User.ID, p); err != nil {
			m.deny(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireReviewer allows only users assigned to review the subject that
// subject extracts from the request. An extraction error denies.
func (m *Middleware) RequireReviewer(subject func(*http.Request) (int64, error), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := auth.SessionFrom(r.Context())
		if !ok {
			auth.WriteError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}
		id, err := subject(r)
		if err != nil {
			m.deny(w, ErrDenied)
			return
		}
		if err := m.store.RequireReviewer(r.Context(), sess.User.ID, id); err != nil {
			m.deny(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAnyReviewer allows only users assigned to review at least one
// subject. Routes whose subject is known only after a lookup use it as the
// early check and then check the subject with RequireReviewerTx.
func (m *Middleware) RequireAnyReviewer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := auth.SessionFrom(r.Context())
		if !ok {
			auth.WriteError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}
		set, err := m.store.Load(r.Context(), sess.User.ID)
		if err != nil {
			m.deny(w, err)
			return
		}
		if len(set.ReviewerSubjects()) == 0 {
			m.deny(w, ErrDenied)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *Middleware) deny(w http.ResponseWriter, err error) {
	if err != ErrDenied {
		// A failed lookup still denies; log the cause without request data.
		m.log.Error("permission check failed", "err", err)
	}
	auth.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
}
