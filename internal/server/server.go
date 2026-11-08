package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jlimty/ev-bill/internal/api"
	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/live"
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

// Deps is what the handler needs. Hub may be nil, which turns GET /events
// into a 503; Feed is passed through to the pages.
type Deps struct {
	Store  *store.Store
	Tariff billing.Tariff
	APIKey string
	Now    func() time.Time
	Logger *slog.Logger
	Hub    *live.Hub
	Feed   *live.Feed
}

// New builds the full handler: the JSON API, the status pages, GET /healthz
// and the GET /events stream, with RequestLogger outermost.
func New(d Deps) http.Handler {
	return build(d, requestTimeout)
}

// build is New with the per-request deadline as a parameter, so tests can use
// a short one. Every route except GET /events sits behind the deadline: the
// inner mux holds them and is wrapped by withDeadline, while the outer mux
// sends GET /events straight to the stream, which must outlive any deadline
// (LIVE-1) and sets its own per-write deadlines instead. RequestLogger wraps
// both, so every request, the stream included, gets one log line.
func build(d Deps, timeout time.Duration) http.Handler {
	pages := &web.Pages{Store: d.Store, Tariff: d.Tariff, Now: d.Now, Logger: d.Logger, Hub: d.Hub, Feed: d.Feed}

	inner := http.NewServeMux()
	(&api.Server{Store: d.Store, Tariff: d.Tariff, APIKey: d.APIKey, Now: d.Now, Logger: d.Logger}).Register(inner)
	pages.Register(inner)
	inner.Handle("GET /healthz", Health(d.Store, healthTimeout, d.Logger))

	outer := http.NewServeMux()
	pages.RegisterEvents(outer)
	outer.Handle("/", withDeadline(timeout, inner))
	return RequestLogger(d.Logger, outer)
}
