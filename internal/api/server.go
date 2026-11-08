// Package api serves the JSON API for charging sessions (SES-1 to SES-5,
// API-1, API-2). The wiring into the HTTP server lives elsewhere.
package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/store"
)

// Server holds what the handlers need.
type Server struct {
	Store  *store.Store
	Tariff billing.Tariff
	APIKey string
	Now    func() time.Time
	Logger *slog.Logger
}

// Register adds the session routes to mux. The three POSTs need the API key
// (API-1); the GETs are public.
func (s *Server) Register(mux *http.ServeMux) {
	auth := func(h http.HandlerFunc) http.Handler { return RequireAPIKey(s.APIKey, h) }
	mux.Handle("POST /api/sessions", auth(s.startSession))
	mux.Handle("POST /api/sessions/{id}/meter-values", auth(s.addMeterValue))
	mux.Handle("POST /api/sessions/{id}/stop", auth(s.stopSession))
	mux.HandleFunc("GET /api/sessions/{id}", s.getSession)
	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("/api/", s.notFound)
}

// writeJSON writes v as the JSON body with the given status. It encodes into a
// buffer first, so an encoding failure is logged and answered with a generic
// 500 instead of a success status with an empty body.
func writeJSON(logger *slog.Logger, w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		logger.Error("encode response", "error", err)
		buf.Reset()
		status = http.StatusInternalServerError
		if err := json.NewEncoder(&buf).Encode(errorBody("internal", "internal error")); err != nil {
			logger.Error("encode internal error body", "error", err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		logger.Error("write response", "error", err)
	}
}
