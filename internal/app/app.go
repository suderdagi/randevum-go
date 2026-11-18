// Package app wires the handlers into one http.Handler. Tests use it too,
// so they exercise exactly the routes and middleware that run in production.
package app

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/suderdagi/randevum-go/internal/auth"
	"github.com/suderdagi/randevum-go/internal/booking"
	"github.com/suderdagi/randevum-go/internal/business"
	"github.com/suderdagi/randevum-go/internal/config"
	"github.com/suderdagi/randevum-go/internal/httpx"
)

func New(cfg config.Config, db *pgxpool.Pool, now func() time.Time) http.Handler {
	authService := &auth.Service{DB: db, Secret: cfg.JWTSecret, AccessTTL: cfg.AccessTokenTTL, RefreshTTL: cfg.RefreshTokenTTL}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unavailable"})
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	authService.Routes(mux)
	(&business.Handler{DB: db, Auth: authService}).Routes(mux)
	(&booking.Handler{DB: db, Auth: authService, Now: now}).Routes(mux)
	mux.Handle("/", httpx.Handler(func(http.ResponseWriter, *http.Request) error {
		return &httpx.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "Route not found"}
	}))

	return logRequests(recoverPanics(mux))
}

func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.ErrorContext(r.Context(), "panic", "value", v, "path", r.URL.Path)
				httpx.JSON(w, http.StatusInternalServerError, map[string]any{
					"error": httpx.Error{Code: "INTERNAL_ERROR", Message: "Something went wrong"},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
	})
}
