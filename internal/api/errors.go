package api

import (
	"errors"
	"net/http"

	"github.com/jlimty/ev-bill/internal/sessions"
)

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

func errorBody(code, message string) errorResponse {
	return errorResponse{Error: errorDetail{Code: code, Message: message}}
}

// statusFor maps a rule error kind to its HTTP status (API-2). ok is false for
// a kind it does not know.
func statusFor(k sessions.Kind) (status int, ok bool) {
	switch k {
	case sessions.KindInvalid:
		return http.StatusUnprocessableEntity, true
	case sessions.KindConflict:
		return http.StatusConflict, true
	case sessions.KindNotFound:
		return http.StatusNotFound, true
	}
	return 0, false
}

// fail answers err. Rule errors carry their own code and message; anything else
// is logged and answered with a generic 500 so internals never reach the client.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var re *sessions.RuleError
	if errors.As(err, &re) {
		if status, ok := statusFor(re.Kind); ok {
			writeJSON(s.Logger, w, status, errorBody(re.Code, re.Message))
			return
		}
	}
	s.Logger.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	writeJSON(s.Logger, w, http.StatusInternalServerError, errorBody("internal", "internal error"))
}

// notFound answers any /api path or method that no route matched, so clients
// always get the JSON Error body instead of the mux's text/plain 404.
func (s *Server) notFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(s.Logger, w, http.StatusNotFound, errorBody("not_found", "no such route"))
}

func (s *Server) badJSON(w http.ResponseWriter, message string) {
	writeJSON(s.Logger, w, http.StatusBadRequest, errorBody("bad_json", message))
}
