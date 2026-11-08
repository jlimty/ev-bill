package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jlimty/ev-bill/internal/api"
	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/web"
)

// healthTimeout bounds the database ping behind /healthz (OPS-2).
const healthTimeout = 2 * time.Second

// requestTimeout is the deadline on every request's context. It is below the
// 10 s WriteTimeout in Serve, so database work cannot outlive the response.
const requestTimeout = 8 * time.Second

// withDeadline gives each request's context a deadline of d. A database call
// still running at the deadline fails, and the handlers' existing error paths
// return the generic 500 JSON and log the cause. Unlike http.TimeoutHandler it
// writes no response itself, so the JSON contract holds.
func withDeadline(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// New builds the full handler: the JSON API, the status pages and
// GET /healthz, all wrapped by a per-request deadline and, outermost, RequestLogger.
func New(st *store.Store, t billing.Tariff, apiKey string, now func() time.Time, l *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	(&api.Server{Store: st, Tariff: t, APIKey: apiKey, Now: now, Logger: l}).Register(mux)
	(&web.Pages{Store: st, Tariff: t, Now: now, Logger: l}).Register(mux)
	mux.Handle("GET /healthz", Health(st, healthTimeout, l))
	return newHandler(l, mux, requestTimeout)
}

// newHandler wraps h in the per-request deadline, then RequestLogger so the
// log line still has the real status. New passes requestTimeout; tests pass a
// short one.
func newHandler(l *slog.Logger, h http.Handler, timeout time.Duration) http.Handler {
	return RequestLogger(l, withDeadline(timeout, h))
}
