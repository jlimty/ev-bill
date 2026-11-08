package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/routers"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/testdb"
)

const testKey = "test-key"

// fixedNow is 2026-11-07 00:20 SGT.
func fixedNow() time.Time {
	return time.Date(2026, 11, 7, 0, 20, 0, 0, billing.SingaporeTariff().Location)
}

type env struct {
	t      *testing.T
	mux    *http.ServeMux
	router routers.Router
	pool   *pgxpool.Pool
	logs   *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t)
	logs := &bytes.Buffer{}
	srv := &Server{
		Store:  store.New(pool),
		Tariff: billing.SingaporeTariff(),
		APIKey: testKey,
		Now:    fixedNow,
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	return &env{t: t, mux: mux, router: contractRouter(t), pool: pool, logs: logs}
}

// do sends a request (with the right API key unless key is empty), checks the
// response against the contract and returns it.
func (e *env) do(method, path, body, key string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	assertContract(e.t, e.router, method, path, rec)
	return rec
}

func (e *env) post(path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodPost, path, body, testKey)
}

func (e *env) get(path string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodGet, path, "", "")
}

type segmentBody struct {
	Window          string `json:"window"`
	EnergyWh        int64  `json:"energy_wh"`
	RateCentsPerKWh int64  `json:"rate_cents_per_kwh"`
	CostCents       int64  `json:"cost_cents"`
}

type sessionBody struct {
	ID         string        `json:"id"`
	ChargerID  string        `json:"charger_id"`
	Status     string        `json:"status"`
	StartedAt  string        `json:"started_at"`
	StoppedAt  *string       `json:"stopped_at"`
	EnergyWh   int64         `json:"energy_wh"`
	TotalCents int64         `json:"total_cents"`
	Segments   []segmentBody `json:"segments"`
}

func decodeSession(t *testing.T, rec *httptest.ResponseRecorder) sessionBody {
	t.Helper()
	var s sessionBody
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode session %q: %v", rec.Body.String(), err)
	}
	return s
}

type errBody struct {
	Error struct{ Code, Message string }
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if e.Error.Code != code {
		t.Errorf("error code = %q, want %q", e.Error.Code, code)
	}
}

func startBody(charger, at string, wh int64) string {
	return fmt.Sprintf(`{"charger_id":%q,"started_at":%q,"meter_start_wh":%d}`, charger, at, wh)
}

func meterBody(at string, wh int64) string {
	return fmt.Sprintf(`{"timestamp":%q,"meter_wh":%d}`, at, wh)
}

func stopBody(at string, wh int64) string {
	return fmt.Sprintf(`{"stopped_at":%q,"meter_stop_wh":%d}`, at, wh)
}

// startSession starts a session and returns its id.
func (e *env) startSession(charger, at string, wh int64) string {
	e.t.Helper()
	rec := e.post("/api/sessions", startBody(charger, at, wh))
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("start: status %d, body %s", rec.Code, rec.Body.String())
	}
	return decodeSession(e.t, rec).ID
}

const (
	t2140 = "2026-11-06T21:40:00+08:00"
	t2220 = "2026-11-06T22:20:00+08:00"
	t2300 = "2026-11-06T23:00:00+08:00"
	t0015 = "2026-11-07T00:15:00+08:00"
)

// SES-1: a new session is charging with no energy and an empty segments array.
func TestStart_Returns201(t *testing.T) {
	e := newEnv(t)
	rec := e.post("/api/sessions", startBody("CP-01", t0015, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	s := decodeSession(t, rec)
	if s.Status != "charging" || s.EnergyWh != 0 || s.TotalCents != 0 || s.StoppedAt != nil || s.ChargerID != "CP-01" {
		t.Errorf("unexpected session %+v", s)
	}
	if s.Segments == nil || len(s.Segments) != 0 {
		t.Errorf("segments = %v, want empty non-nil", s.Segments)
	}
	if !strings.Contains(rec.Body.String(), `"segments":[]`) {
		t.Errorf("body must contain segments:[], got %s", rec.Body.String())
	}
}

// API-1: POSTs need the right X-API-Key; GETs are public.
func TestAuth_API1(t *testing.T) {
	e := newEnv(t)
	body := startBody("CP-01", t0015, 0)
	wantError(t, e.do(http.MethodPost, "/api/sessions", body, ""), http.StatusUnauthorized, "unauthorized")
	wantError(t, e.do(http.MethodPost, "/api/sessions", body, "wrong"), http.StatusUnauthorized, "unauthorized")
	id := e.startSession("CP-01", t0015, 0)
	wantError(t, e.do(http.MethodPost, "/api/sessions/"+id+"/stop", stopBody(t0015, 0), ""), http.StatusUnauthorized, "unauthorized")
	wantError(t, e.do(http.MethodPost, "/api/sessions/"+id+"/meter-values", meterBody(t0015, 0), "wrong"), http.StatusUnauthorized, "unauthorized")
	if rec := e.get("/api/sessions"); rec.Code != http.StatusOK {
		t.Errorf("GET list without key = %d, want 200", rec.Code)
	}
	if rec := e.get("/api/sessions/" + id); rec.Code != http.StatusOK {
		t.Errorf("GET one without key = %d, want 200", rec.Code)
	}
}

// API-1: the key check compares SHA-256 digests, so a prefix or longer key fails.
func TestAuth_API1_PrefixAndLongerKeysFail(t *testing.T) {
	e := newEnv(t)
	body := startBody("CP-01", t0015, 0)
	for _, key := range []string{"test-ke", "test-key-extra"} {
		wantError(t, e.do(http.MethodPost, "/api/sessions", body, key), http.StatusUnauthorized, "unauthorized")
	}
}

// SES-1: a second active session on one charger is 409 active_session_exists.
func TestStart_SecondActiveIs409(t *testing.T) {
	e := newEnv(t)
	e.startSession("CP-01", t0015, 0)
	wantError(t, e.post("/api/sessions", startBody("CP-01", t0015, 0)), http.StatusConflict, "active_session_exists")
}

// SES-2: a new reading is 201, an identical repeat is 200.
func TestMeterValues_NewIs201DuplicateIs200(t *testing.T) {
	e := newEnv(t)
	id := e.startSession("CP-01", t2140, 0)
	path := "/api/sessions/" + id + "/meter-values"
	first := e.post(path, meterBody(t2220, 4800))
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d, want 201; %s", first.Code, first.Body.String())
	}
	second := e.post(path, meterBody(t2220, 4800))
	if second.Code != http.StatusOK {
		t.Fatalf("repeat = %d, want 200; %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("duplicate returned a different session:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	if s := decodeSession(t, second); s.EnergyWh != 4800 {
		t.Errorf("energy_wh = %d, want 4800", s.EnergyWh)
	}
}

// SES-2: a different value at a stored timestamp is 409 conflicting_reading.
func TestMeterValues_ConflictIs409(t *testing.T) {
	e := newEnv(t)
	id := e.startSession("CP-01", t2140, 0)
	path := "/api/sessions/" + id + "/meter-values"
	e.post(path, meterBody(t2220, 4800))
	wantError(t, e.post(path, meterBody(t2220, 4900)), http.StatusConflict, "conflicting_reading")
}

// SES-2: readings for an unknown session are 404.
func TestMeterValues_UnknownSessionIs404(t *testing.T) {
	e := newEnv(t)
	wantError(t, e.post("/api/sessions/0b9a1f6e-6a39-4d3c-8f0e-1c2d3e4f5a6b/meter-values", meterBody(t2220, 1)), http.StatusNotFound, "not_found")
}

func stopMockup(e *env) (id string, rec *httptest.ResponseRecorder) {
	e.t.Helper()
	id = e.startSession("CP-01", t2140, 0)
	e.post("/api/sessions/"+id+"/meter-values", meterBody(t2220, 4800))
	e.post("/api/sessions/"+id+"/meter-values", meterBody(t2300, 9600))
	return id, e.post("/api/sessions/"+id+"/stop", stopBody(t0015, 18000))
}

// SES-3, BIL-7: the mockup session bills 998 cents in two segments.
func TestStop_MockupSessionBill(t *testing.T) {
	e := newEnv(t)
	_, rec := stopMockup(e)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; %s", rec.Code, rec.Body.String())
	}
	s := decodeSession(t, rec)
	if s.Status != "completed" || s.StoppedAt == nil || s.EnergyWh != 18000 || s.TotalCents != 998 {
		t.Errorf("unexpected session %+v", s)
	}
	want := []segmentBody{
		{Window: "peak", EnergyWh: 9600, RateCentsPerKWh: 62, CostCents: 595},
		{Window: "off-peak", EnergyWh: 8400, RateCentsPerKWh: 48, CostCents: 403},
	}
	if len(s.Segments) != len(want) {
		t.Fatalf("segments = %+v, want %+v", s.Segments, want)
	}
	for i := range want {
		if s.Segments[i] != want[i] {
			t.Errorf("segment %d = %+v, want %+v", i, s.Segments[i], want[i])
		}
	}
}

// SES-3: an identical stop replays with the same 200 body; a different stop is 409.
func TestStop_ReplayIs200DifferentIs409(t *testing.T) {
	e := newEnv(t)
	id, first := stopMockup(e)
	path := "/api/sessions/" + id + "/stop"
	replay := e.post(path, stopBody(t0015, 18000))
	if replay.Code != http.StatusOK {
		t.Fatalf("replay = %d, want 200; %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Errorf("replay body differs:\n%s\n%s", first.Body.String(), replay.Body.String())
	}
	wantError(t, e.post(path, stopBody(t0015, 18001)), http.StatusConflict, "stop_conflict")
}

// SES-3: a reading after the stop is 409 session_stopped.
func TestMeterValues_AfterStopIs409(t *testing.T) {
	e := newEnv(t)
	id, _ := stopMockup(e)
	wantError(t, e.post("/api/sessions/"+id+"/meter-values", meterBody(t0015, 18000)), http.StatusConflict, "session_stopped")
}

// API-2: unknown and malformed ids are both 404 not_found.
func TestGet_UnknownAndMalformedIDsAre404(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"0b9a1f6e-6a39-4d3c-8f0e-1c2d3e4f5a6b", "not-a-uuid", "1"} {
		wantError(t, e.get("/api/sessions/"+id), http.StatusNotFound, "not_found")
	}
}

// SES-4: GET one returns the session with its status.
func TestGet_ReturnsSession(t *testing.T) {
	e := newEnv(t)
	id := e.startSession("CP-01", t0015, 0)
	rec := e.get("/api/sessions/" + id)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if s := decodeSession(t, rec); s.ID != id || s.Status != "charging" {
		t.Errorf("unexpected session %+v", s)
	}
}

// SES-4: an active session with no reading for over 15 minutes is stale.
func TestGet_StaleAfterFifteenMinutes(t *testing.T) {
	e := newEnv(t)
	id := e.startSession("CP-01", t2140, 0)
	if s := decodeSession(t, e.get("/api/sessions/"+id)); s.Status != "stale" {
		t.Errorf("status = %q, want stale", s.Status)
	}
}

type listBody struct {
	Sessions []sessionBody `json:"sessions"`
}

// API table: the list is newest first and never null.
func TestList_NewestFirst(t *testing.T) {
	e := newEnv(t)
	var empty listBody
	rec := e.get("/api/sessions")
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil || empty.Sessions == nil {
		t.Fatalf("empty list must be [] not null: %s (%v)", rec.Body.String(), err)
	}
	older := e.startSession("CP-01", "2026-11-06T10:00:00+08:00", 0)
	newer := e.startSession("CP-02", "2026-11-06T11:00:00+08:00", 0)
	rec = e.get("/api/sessions")
	var l listBody
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(l.Sessions) != 2 || l.Sessions[0].ID != newer || l.Sessions[1].ID != older {
		t.Errorf("order = %+v, want %s then %s", l.Sessions, newer, older)
	}
}

// R14: the list returns at most sessions.ListLimit (50), newest first.
func TestList_CapsAtListLimit(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var newest string
	for i := 0; i < sessions.ListLimit+1; i++ {
		at := base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		newest = e.startSession(fmt.Sprintf("CP-%02d", i), at, 0)
	}
	var l listBody
	rec := e.get("/api/sessions")
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(l.Sessions) != 50 {
		t.Fatalf("len = %d, want 50", len(l.Sessions))
	}
	if l.Sessions[0].ID != newest {
		t.Errorf("first = %s, want newest %s", l.Sessions[0].ID, newest)
	}
}

// Review Focus 2: the same instant written with two offsets is a duplicate.
func TestTimestamps_OffsetsAreSameInstant(t *testing.T) {
	e := newEnv(t)
	id := e.startSession("CP-01", t2140, 0)
	path := "/api/sessions/" + id + "/meter-values"
	first := e.post(path, meterBody("2026-11-06T14:20:00Z", 4800))
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d, want 201; %s", first.Code, first.Body.String())
	}
	second := e.post(path, meterBody("2026-11-06T22:20:00+08:00", 4800))
	if second.Code != http.StatusOK {
		t.Fatalf("second = %d, want 200; %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("bodies differ:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}

// Review Focus 3: sub-microsecond precision is dropped, so a replay is a duplicate.
func TestTimestamps_SubMicrosecondReplayIsDuplicate(t *testing.T) {
	e := newEnv(t)
	id := e.startSession("CP-01", t2140, 0)
	path := "/api/sessions/" + id + "/meter-values"
	body := meterBody("2026-11-06T22:20:00.123456789+08:00", 4800)
	first := e.post(path, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d, want 201; %s", first.Code, first.Body.String())
	}
	second := e.post(path, body)
	if second.Code != http.StatusOK {
		t.Fatalf("second = %d, want 200; %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("bodies differ:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}

// Review Focus 4: bad bodies are 400 bad_json (or 422 for rule failures), never 500.
func TestBadBodies(t *testing.T) {
	e := newEnv(t)
	id := e.startSession("CP-01", t2140, 0)
	meter := "/api/sessions/" + id + "/meter-values"
	stop := "/api/sessions/" + id + "/stop"
	// Valid except for its size: without the limit this is a fresh reading (201).
	big := `{"timestamp":"` + t2220 + `","meter_wh":1` + strings.Repeat(" ", 1100*1024) + `}`
	cases := []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"invalid JSON", meter, `{"timestamp":`, 400, "bad_json"},
		{"unknown field", meter, `{"timestamp":"` + t2220 + `","meter_wh":1,"extra":1}`, 400, "bad_json"},
		{"string meter_wh", meter, `{"timestamp":"` + t2220 + `","meter_wh":"12"}`, 400, "bad_json"},
		{"fractional meter_wh", meter, `{"timestamp":"` + t2220 + `","meter_wh":1.5}`, 400, "bad_json"},
		{"int64 overflow", meter, `{"timestamp":"` + t2220 + `","meter_wh":99999999999999999999}`, 400, "bad_json"},
		{"missing meter_wh", meter, `{"timestamp":"` + t2220 + `"}`, 400, "bad_json"},
		{"missing timestamp", meter, `{"meter_wh":1}`, 400, "bad_json"},
		{"bad timestamp", meter, `{"timestamp":"yesterday","meter_wh":1}`, 400, "bad_json"},
		{"empty body", meter, ``, 400, "bad_json"},
		{"null body", meter, `null`, 400, "bad_json"},
		{"trailing object", meter, meterBody(t2220, 1) + meterBody(t2300, 2), 400, "bad_json"},
		{"trailing junk", meter, meterBody(t2220, 1) + ` x`, 400, "bad_json"},
		{"over 1 MiB", meter, big, 400, "bad_json"},
		{"stop missing field", stop, `{"stopped_at":"` + t0015 + `"}`, 400, "bad_json"},
		{"start missing field", "/api/sessions", `{"charger_id":"CP-09","started_at":"` + t0015 + `"}`, 400, "bad_json"},
		{"negative meter", meter, meterBody(t2220, -1), 422, "invalid_reading"},
		{"lowercase charger", "/api/sessions", startBody("cp-01", t0015, 0), 422, "invalid_charger_id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := e.post(c.path, c.body)
			if rec.Code == http.StatusInternalServerError {
				t.Fatalf("got 500: %s", rec.Body.String())
			}
			wantError(t, rec, c.status, c.code)
		})
	}
}

// API-2: an unexpected failure is a generic 500 internal; the cause is logged, not echoed.
func TestInternalError_IsGenericAndLogged(t *testing.T) {
	e := newEnv(t)
	e.pool.Close()
	rec := e.get("/api/sessions")
	wantError(t, rec, http.StatusInternalServerError, "internal")
	var body errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Message != "internal error" {
		t.Errorf("message = %q, want generic %q", body.Error.Message, "internal error")
	}
	want := `{"error":{"code":"internal","message":"internal error"}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want exactly %q", rec.Body.String(), want)
	}
	logged := e.logs.String()
	if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, "request failed") {
		t.Errorf("underlying error was not logged: %q", logged)
	}
}

// API-1: an empty configured key fails closed, with or without the header.
func TestRequireAPIKey_EmptyKeyFailsClosed(t *testing.T) {
	called := false
	h := RequireAPIKey("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	for _, header := range []*string{nil, new(string)} {
		req := httptest.NewRequest(http.MethodPost, "/api/sessions", nil)
		if header != nil {
			req.Header.Set("X-API-Key", *header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		wantError(t, rec, http.StatusUnauthorized, "unauthorized")
	}
	if called {
		t.Error("API-1: handler ran with an empty configured key")
	}
}

// API-2: unrouted /api paths and wrong methods get the JSON not_found body.
func TestUnroutedAPIPathsAreJSON404(t *testing.T) {
	e := newEnv(t)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/nope"},
		{http.MethodPut, "/api/sessions"},
		{http.MethodDelete, "/api/sessions/0b9a1f6e-6a39-4d3c-8f0e-1c2d3e4f5a6b"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, req)
		wantError(t, rec, http.StatusNotFound, "not_found")
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s Content-Type = %q", c.method, c.path, ct)
		}
	}
}
