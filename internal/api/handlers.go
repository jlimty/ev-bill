package api

import (
	"net/http"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/sessions"
)

type segmentResponse struct {
	Window          string    `json:"window"`
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	EnergyWh        int64     `json:"energy_wh"`
	RateCentsPerKWh int64     `json:"rate_cents_per_kwh"`
	CostCents       int64     `json:"cost_cents"`
}

type sessionResponse struct {
	ID         string            `json:"id"`
	ChargerID  string            `json:"charger_id"`
	Source     string            `json:"source"`
	Status     string            `json:"status"`
	StartedAt  time.Time         `json:"started_at"`
	StoppedAt  *time.Time        `json:"stopped_at"`
	EnergyWh   int64             `json:"energy_wh"`
	TotalCents int64             `json:"total_cents"`
	Segments   []segmentResponse `json:"segments"`
}

type listResponse struct {
	Sessions []sessionResponse `json:"sessions"`
}

// toResponse prices a session and shapes it as the contract's Session. Segments
// is always a non-nil slice so it encodes as [] when empty.
func (s *Server) toResponse(sess sessions.Session, readings []billing.Reading, now time.Time) (sessionResponse, error) {
	v, err := sessions.BuildView(sess, readings, s.Tariff, now)
	if err != nil {
		return sessionResponse{}, err
	}
	segments := make([]segmentResponse, 0, len(v.Bill.Segments))
	for _, seg := range v.Bill.Segments {
		segments = append(segments, segmentResponse{
			Window:          string(seg.Window),
			Start:           seg.Start.UTC(),
			End:             seg.End.UTC(),
			EnergyWh:        seg.EnergyWh,
			RateCentsPerKWh: seg.RateCentsPerKWh,
			CostCents:       seg.CostCents,
		})
	}
	var stoppedAt *time.Time
	if sess.StoppedAt != nil {
		u := sess.StoppedAt.UTC()
		stoppedAt = &u
	}
	return sessionResponse{
		ID:         sess.ID,
		ChargerID:  sess.ChargerID,
		Source:     string(sess.Source),
		Status:     v.Status,
		StartedAt:  sess.StartedAt.UTC(),
		StoppedAt:  stoppedAt,
		EnergyWh:   v.Bill.EnergyWh,
		TotalCents: v.Bill.TotalCents,
		Segments:   segments,
	}, nil
}

// respond writes the session view with status, or the error if pricing fails.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, status int, sess sessions.Session, readings []billing.Reading, now time.Time) {
	body, err := s.toResponse(sess, readings, now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(s.Logger, w, status, body)
}

// startSession handles POST /api/sessions (SES-1).
func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := decodeBody(w, r, &req); err != nil || !req.complete() {
		s.badJSON(w, "request body must be a JSON object with charger_id, started_at and meter_start_wh")
		return
	}
	now := s.Now()
	sess, err := s.Store.CreateSession(r.Context(), sessions.SourceAPI, *req.ChargerID, utcMicro(*req.StartedAt), *req.MeterStartWh, now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusCreated, sess, nil, now)
}

// addMeterValue handles POST /api/sessions/{id}/meter-values (SES-2). A new
// reading is 201; an identical repeat is 200.
func (s *Server) addMeterValue(w http.ResponseWriter, r *http.Request) {
	var req meterValueRequest
	if err := decodeBody(w, r, &req); err != nil || !req.complete() {
		s.badJSON(w, "request body must be a JSON object with timestamp and meter_wh")
		return
	}
	now := s.Now()
	reading := billing.Reading{At: utcMicro(*req.Timestamp), MeterWh: *req.MeterWh}
	sess, readings, duplicate, err := s.Store.AddReading(r.Context(), sessions.SourceAPI, r.PathValue("id"), reading, now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if duplicate {
		status = http.StatusOK
	}
	s.respond(w, r, status, sess, readings, now)
}

// stopSession handles POST /api/sessions/{id}/stop (SES-3).
func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	var req stopRequest
	if err := decodeBody(w, r, &req); err != nil || !req.complete() {
		s.badJSON(w, "request body must be a JSON object with stopped_at and meter_stop_wh")
		return
	}
	stop := billing.Reading{At: utcMicro(*req.StoppedAt), MeterWh: *req.MeterStopWh}
	now := s.Now()
	sess, readings, err := s.Store.StopSession(r.Context(), sessions.SourceAPI, r.PathValue("id"), stop, now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, sess, readings, now)
}

// getSession handles GET /api/sessions/{id}. A malformed id is a 404 from the store.
func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sess, readings, err := s.Store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, sess, readings, s.Now())
}

// listSessions handles GET /api/sessions: the newest sessions.ListLimit sessions.
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	now := s.Now()
	list, err := s.Store.ListRecent(r.Context(), sessions.ListLimit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := listResponse{Sessions: make([]sessionResponse, 0, len(list))}
	for _, item := range list {
		body, err := s.toResponse(item.Session, item.Readings, now)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out.Sessions = append(out.Sessions, body)
	}
	writeJSON(s.Logger, w, http.StatusOK, out)
}
