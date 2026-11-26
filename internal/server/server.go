// Package server assembles the authenticated application's HTTP handler.
// It is separate from cmd/renderer, which stays an isolated experiment.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/hadden-me/learnoo/internal/accounts"
	"github.com/hadden-me/learnoo/internal/auth"
	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/hadden-me/learnoo/internal/invitations"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/reader"
	"github.com/hadden-me/learnoo/internal/reports"
	"github.com/hadden-me/learnoo/internal/review"
	"github.com/hadden-me/learnoo/internal/studioapi"
	"github.com/hadden-me/learnoo/internal/studioweb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Option configures New.
type Option func(*options)

type options struct {
	studio fs.FS
}

// WithStudio serves the built Studio bundle (web/studio/dist) from files
// under /studio (internal/studioweb). Without it /studio is not routed.
func WithStudio(files fs.FS) Option {
	return func(o *options) { o.studio = files }
}

// New returns the root handler: routes wrapped in origin-based CSRF
// protection and security headers. mediaStore holds uploaded images.
func New(db *pgxpool.Pool, svc *auth.Service, mediaStore *media.Store, log *slog.Logger, opts ...Option) http.Handler {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	mux := http.NewServeMux()
	authH := auth.NewHandlers(svc, log)
	authH.Register(mux)
	studioapi.New(studioapi.Config{
		DB:          db,
		Auth:        authH,
		Perms:       permissions.NewMiddleware(permissions.NewStore(db), log),
		Lessons:     lessons.NewStore(db),
		Media:       mediaStore,
		Review:      review.NewStore(db),
		Reports:     reports.NewStore(db),
		Catalogue:   catalogue.NewStore(db),
		Curriculum:  curriculum.NewStore(db),
		Courses:     courses.NewStore(db),
		Permissions: permissions.NewStore(db),
		Accounts:    accounts.NewStore(db),
		Invitations: invitations.NewStore(db, nil),
		Service:     svc,
		Log:         log,
	}).Register(mux)
	readerH := reader.New(reader.Config{
		Auth:        authH,
		Service:     svc,
		Store:       reader.NewStore(db),
		Invitations: invitations.NewStore(db, nil),
		Reports:     reports.NewStore(db),
		Media:       mediaStore,
		Log:         log,
	})
	readerH.Register(mux)
	if o.studio != nil {
		meta := map[string]string{}
		for name, asset := range map[string]string{"learnoo-katex-js": "vendor/katex/katex.min.js", "learnoo-katex-css": "vendor/katex/katex.min.css"} {
			meta[name], _ = readerH.AssetURL(asset) // public asset names: never an error
		}
		studioweb.New(studioweb.Config{
			Auth:      authH,
			Perms:     permissions.NewStore(db),
			Files:     o.studio,
			ErrorPage: readerH.ErrorPage,
			Meta:      meta,
			Log:       log,
		}).Register(mux)
	}
	// Liveness: the process serves HTTP. No dependency is checked, so a
	// database outage does not make an orchestrator restart the app.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeStatusOK(w)
	})
	// Readiness: the database answers and the media directory is writable
	// (a permission check that writes nothing). Internal use only: the
	// reverse proxy must not expose the probes (docs/DEPLOYMENT.md).
	// Failures are logged; the response names no component or error.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			log.Error("readiness: database", "err", err)
			auth.WriteError(w, http.StatusServiceUnavailable, "unavailable", "Not ready.")
			return
		}
		if err := mediaStore.CheckWritable(); err != nil {
			log.Error("readiness: media directory", "err", err)
			auth.WriteError(w, http.StatusServiceUnavailable, "unavailable", "Not ready.")
			return
		}
		writeStatusOK(w)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		auth.WriteError(w, http.StatusNotFound, "not_found", "Not found.")
	})

	// Rejects cross-origin unsafe requests using Sec-Fetch-Site / Origin
	// (Go 1.25). Session-bound CSRF tokens add a second check on
	// authenticated mutations; this layer also covers login CSRF.
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.WriteError(w, http.StatusForbidden, "cross_origin_rejected", "Cross-origin request rejected.")
	}))
	return securityHeaders(cop.Handler(mux))
}

func writeStatusOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Every response may depend on the session: never store in shared caches.
		h.Set("Cache-Control", "no-store")
		h.Set("Vary", "Cookie")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
