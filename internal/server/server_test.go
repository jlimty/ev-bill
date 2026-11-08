package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/live"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/testdb"
)

type pingFunc func(context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

// assertContract validates a response against api/openapi.yaml (API-3).
func assertContract(t *testing.T, path string, rec *httptest.ResponseRecorder) {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatalf("contract router: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	route, params, err := router.FindRoute(req)
	if err != nil {
		t.Fatalf("%s is not in the contract: %v", path, err)
	}
	resp := rec.Result()
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	in.SetBodyBytes(rec.Body.Bytes())
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		t.Errorf("GET %s -> %d violates the contract: %v", path, resp.StatusCode, err)
	}
}

func TestHealth_OPS2(t *testing.T) {
	tests := []struct {
		name       string
		ping       pingFunc
		wantStatus int
		wantBody   string
		maxTime    time.Duration
	}{
		{"ping ok", func(context.Context) error { return nil }, 200, `{"status":"ok"}`, time.Second},
		{"ping error", func(context.Context) error { return errors.New("db down") }, 503, `{"status":"unavailable"}`, time.Second},
		{"ping blocks past timeout", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, 503, `{"status":"unavailable"}`, time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := Health(tc.ping, 50*time.Millisecond, slog.New(slog.NewJSONHandler(&logs, nil)))
			rec := httptest.NewRecorder()
			start := time.Now()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if d := time.Since(start); d > tc.maxTime {
				t.Errorf("OPS-2: took %v, want under %v", d, tc.maxTime)
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("OPS-2: status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != tc.wantBody {
				t.Errorf("OPS-2: body = %s, want %s", got, tc.wantBody)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("OPS-2: Content-Type = %q", ct)
			}
			assertContract(t, "/healthz", rec)
			if tc.wantStatus == 503 && strings.Contains(rec.Body.String(), "db down") {
				t.Error("OPS-2: ping error leaked into the response")
			}
			if tc.wantStatus == 503 && !strings.Contains(logs.String(), "health") {
				t.Errorf("OPS-2: ping error was not logged: %q", logs.String())
			}
		})
	}
}

func TestServe_OPS3_GracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	entered := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, h, 5*time.Second, slog.New(slog.NewJSONHandler(io.Discard, nil))) }()

	type result struct {
		code int
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			got <- result{err: err}
			return
		}
		cerr := resp.Body.Close()
		got <- result{code: resp.StatusCode, err: cerr}
	}()

	<-entered
	cancel()

	if r := <-got; r.err != nil || r.code != http.StatusOK {
		t.Errorf("OPS-3: in-flight request = %d, %v; want 200, nil", r.code, r.err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("OPS-3: Serve = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OPS-3: Serve did not return after shutdown")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		if cerr := conn.Close(); cerr != nil {
			t.Errorf("close: %v", cerr)
		}
		t.Error("OPS-3: dial succeeded after shutdown")
	}
}

func TestServe_OPS3_ShutdownTimeoutReturnsError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	released := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(released)
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, ln, h, 50*time.Millisecond, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close() //nolint:errcheck // best-effort in a test goroutine
		}
	}()
	<-entered
	cancel()
	select {
	case err := <-served:
		if err == nil {
			t.Error("OPS-3: Serve = nil, want a shutdown error when a request outlives the timeout")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OPS-3: Serve did not return")
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Error("OPS-3: stuck handler's request context was not cancelled after Serve returned")
	}
}

func logLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("OPS-5: want exactly one log line, got %d: %q", len(lines), buf.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("OPS-5: log line is not JSON: %v: %q", err, lines[0])
	}
	return m
}

func loggedRequest(t *testing.T, target, incomingID string, status int) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	h := RequestLogger(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if incomingID != "" {
		req.Header.Set("X-Request-ID", incomingID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, logLine(t, &buf)
}

func TestRequestLogger_OPS5(t *testing.T) {
	t.Run("generates an id, logs the fields, echoes the id", func(t *testing.T) {
		rec, line := loggedRequest(t, "/api/sessions", "", http.StatusTeapot)
		id := rec.Header().Get("X-Request-ID")
		if len(id) != 32 {
			t.Errorf("OPS-5: generated id %q, want 32 hex chars", id)
		}
		if line["request_id"] != id {
			t.Errorf("OPS-5: logged request_id = %v, header = %q", line["request_id"], id)
		}
		if line["method"] != "GET" || line["path"] != "/api/sessions" {
			t.Errorf("OPS-5: method/path = %v %v", line["method"], line["path"])
		}
		if line["status"] != float64(http.StatusTeapot) {
			t.Errorf("OPS-5: status = %v", line["status"])
		}
		if _, ok := line["duration_ms"]; !ok {
			t.Error("OPS-5: duration_ms missing")
		}
	})
	t.Run("status defaults to 200", func(t *testing.T) {
		_, line := loggedRequest(t, "/", "", 0)
		if line["status"] != float64(200) {
			t.Errorf("OPS-5: status = %v, want 200", line["status"])
		}
	})
	t.Run("reuses a valid incoming id", func(t *testing.T) {
		rec, line := loggedRequest(t, "/", "abc-123_X.y", 0)
		if rec.Header().Get("X-Request-ID") != "abc-123_X.y" || line["request_id"] != "abc-123_X.y" {
			t.Errorf("OPS-5: id not reused: header %q, log %v", rec.Header().Get("X-Request-ID"), line["request_id"])
		}
	})
	t.Run("replaces an invalid incoming id", func(t *testing.T) {
		for _, bad := range []string{"has space", "semi;colon", strings.Repeat("a", 65), "new\\nline", "abc\n", "ünï"} {
			rec, line := loggedRequest(t, "/", bad, 0)
			got := rec.Header().Get("X-Request-ID")
			if got == bad || len(got) != 32 || line["request_id"] != got {
				t.Errorf("OPS-5: bad id %q: header %q, log %v", bad, got, line["request_id"])
			}
		}
	})
	t.Run("accepts a 64-char id", func(t *testing.T) {
		id := strings.Repeat("a", 64)
		rec, _ := loggedRequest(t, "/", id, 0)
		if rec.Header().Get("X-Request-ID") != id {
			t.Error("OPS-5: 64-char id should be reused")
		}
	})
	t.Run("does not log the query string", func(t *testing.T) {
		_, line := loggedRequest(t, "/api/sessions?api_key=secret&x=1", "", 0)
		if line["path"] != "/api/sessions" {
			t.Errorf("OPS-5: path = %v", line["path"])
		}
	})
}

func TestNew_WiresRoutes_OPS2_OPS5(t *testing.T) {
	pool := testdb.New(t)
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	now := func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }
	h := New(Deps{Store: store.New(pool), Tariff: billing.SingaporeTariff(), APIKey: "k", Now: now, Logger: l})

	for _, tc := range []struct {
		path     string
		wantType string
	}{
		{"/healthz", "application/json"},
		{"/", "text/html"},
		{"/api/sessions", "application/json"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: %s", tc.path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.wantType) {
			t.Errorf("GET %s Content-Type = %q, want %s", tc.path, ct, tc.wantType)
		}
		if rec.Header().Get("X-Request-ID") == "" {
			t.Errorf("OPS-5: GET %s has no X-Request-ID (logger not wrapping)", tc.path)
		}
	}
	if n := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1; n != 3 {
		t.Errorf("OPS-5: %d log lines for 3 requests: %q", n, buf.String())
	}
}

func TestServe_OPS3_ListenerFailureReturnsPromptly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- Serve(context.Background(), ln, http.NotFoundHandler(), time.Second, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}()
	select {
	case err := <-served:
		if err == nil {
			t.Error("OPS-3: Serve = nil on a closed listener, want an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OPS-3: Serve hung instead of returning the listener error")
	}
}

func TestRequestLogger_OPS5_FlushReachesRealWriter(t *testing.T) {
	var buf bytes.Buffer
	h := RequestLogger(slog.New(slog.NewJSONHandler(&buf, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("OPS-5: Flush through the logger wrapper failed: %v", err)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if !rec.Flushed {
		t.Error("OPS-5: flush did not reach the underlying writer")
	}
}

func TestRequestLogger_OPS5_FirstStatusWins(t *testing.T) {
	var buf bytes.Buffer
	h := RequestLogger(slog.New(slog.NewJSONHandler(&buf, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if line := logLine(t, &buf); line["status"] != float64(http.StatusCreated) {
		t.Errorf("OPS-5: status = %v, want the first (201)", line["status"])
	}
}

// failingWriter makes every body write fail, like a client that went away.
type failingWriter struct{ http.ResponseWriter }

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

func TestHealth_OPS2_ResponseWriteFailureIsLogged(t *testing.T) {
	var logs bytes.Buffer
	h := Health(pingFunc(func(context.Context) error { return nil }), time.Second, slog.New(slog.NewJSONHandler(&logs, nil)))
	h.ServeHTTP(failingWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !strings.Contains(logs.String(), "client gone") {
		t.Errorf("OPS-2: write failure not logged: %q", logs.String())
	}
}

// logLines parses every JSON log line in buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %q", err, l)
		}
		out = append(out, m)
	}
	return out
}

// OPS-5: the error logs written while handling a request carry the same
// request_id as the request line, for the API, the pages and /healthz.
func TestErrorLogs_OPS5_CarryRequestID(t *testing.T) {
	pool := testdb.New(t)
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	now := func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }
	h := New(Deps{Store: store.New(pool), Tariff: billing.SingaporeTariff(), APIKey: "k", Now: now, Logger: l})
	pool.Close() // every store call now fails with a 500

	for _, path := range []string{"/api/sessions", "/", "/healthz"} {
		buf.Reset()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Request-ID", "req-42")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code < 500 {
			t.Fatalf("GET %s = %d, want a 5xx", path, rec.Code)
		}
		var errLines int
		for _, line := range logLines(t, &buf) {
			if line["request_id"] != "req-42" {
				t.Errorf("OPS-5: GET %s: log line %v lacks request_id req-42", path, line["msg"])
			}
			if line["level"] == "ERROR" {
				errLines++
			}
		}
		if errLines == 0 {
			t.Errorf("OPS-5: GET %s logged no error line: %q", path, buf.String())
		}
	}
}

// OPS-5: a successful GET /healthz is logged at Debug so platform probes stay
// out of the Info stream; a failing one and every other request stay at Info.
func TestRequestLogger_OPS5_HealthzLevel(t *testing.T) {
	cases := []struct {
		name, method, path string
		status             int
		want               string
	}{
		{"healthz ok", http.MethodGet, "/healthz", http.StatusOK, "DEBUG"},
		{"healthz unavailable", http.MethodGet, "/healthz", http.StatusServiceUnavailable, "INFO"},
		{"other path ok", http.MethodGet, "/api/sessions", http.StatusOK, "INFO"},
		{"healthz wrong method", http.MethodPost, "/healthz", http.StatusMethodNotAllowed, "INFO"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		h := RequestLogger(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(c.status) }))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		if got := logLine(t, &buf)["level"]; got != c.want {
			t.Errorf("OPS-5: %s: level = %v, want %s", c.name, got, c.want)
		}
	}

	// At the production level (Info) a healthy probe writes nothing.
	var buf bytes.Buffer
	h := RequestLogger(slog.New(slog.NewJSONHandler(&buf, nil)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if buf.Len() != 0 {
		t.Errorf("OPS-5: healthy probe logged at Info: %q", buf.String())
	}
}

// OPS-2: every request context carries a deadline, so a handler waiting on it
// sees context.DeadlineExceeded (not Canceled) once the deadline passes.
func TestWithDeadline_OPS2(t *testing.T) {
	var got error
	h := withDeadline(50*time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			got = r.Context().Err()
		case <-time.After(2 * time.Second):
		}
	}))
	start := time.Now()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("ctx err = %v, want context.DeadlineExceeded", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("handler waited %s, want about 50ms", d)
	}
}

// OPS-2: the production deadline is 8 s, below Serve's 10 s WriteTimeout.
func TestRequestTimeout_OPS2(t *testing.T) {
	if requestTimeout != 8*time.Second {
		t.Errorf("requestTimeout = %s, want 8s", requestTimeout)
	}
}

// testDeps returns Deps on a fresh test database, with a hub that runs until
// the test ends, and the pool behind the store.
func testDeps(t *testing.T, logs io.Writer) (Deps, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	hub := live.NewHub(100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("hub.Run = %v", err)
		}
	})
	now := func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }
	return Deps{
		Store:  store.New(pool),
		Tariff: billing.SingaporeTariff(),
		APIKey: "k",
		Now:    now,
		Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		Hub:    hub,
		Feed:   &live.Feed{},
	}, pool
}

// openEvents opens GET /events?view=list on url, reads the first event's
// "event:" line, and returns a reader for the rest of the stream. The body is
// closed when the test ends.
func openEvents(t *testing.T, url string) (*http.Response, *bufio.Reader) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(url + "/events?view=list")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close stream: %v", err)
		}
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events = %d, want 200", resp.StatusCode)
	}
	r := bufio.NewReader(resp.Body)
	if line := readLine(t, r); line != "event: list" {
		t.Fatalf("first line = %q, want event: list", line)
	}
	return resp, r
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return strings.TrimSuffix(line, "\n")
}

// nextEventData reads to the next "event:" line and returns that event's data.
func nextEventData(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	for readLine(t, r) != "event: list" {
	}
	var data []string
	for line := readLine(t, r); line != ""; line = readLine(t, r) {
		data = append(data, strings.TrimPrefix(line, "data: "))
	}
	return strings.Join(data, "\n")
}

// LIVE-1, OPS-2: GET /events sits outside the per-request deadline, so it
// stays open long past it, while the API and the pages still get it. A table
// lock makes their queries wait, so only the deadline can end them. The stream
// still goes through RequestLogger.
func TestEvents_LIVE1_NotCutByDeadlines(t *testing.T) {
	var logs bytes.Buffer
	d, pool := testDeps(t, &syncWriter{w: &logs})
	srv := httptest.NewServer(build(d, 50*time.Millisecond))
	t.Cleanup(srv.Close)

	resp, stream := openEvents(t, srv.URL)
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("OPS-5: the stream is not wrapped by RequestLogger")
	}

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE sessions IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock sessions: %v", err)
	}
	for _, path := range []string{"/api/sessions", "/sessions"} {
		start := time.Now()
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if err := r.Body.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		if r.StatusCode != http.StatusInternalServerError {
			t.Errorf("OPS-2: GET %s behind a lock = %d, want 500 from the deadline", path, r.StatusCode)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("OPS-2: GET %s took %s, want about 50ms", path, took)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	time.Sleep(100 * time.Millisecond) // the stream is now well past 50ms
	if _, err := d.Store.CreateSession(ctx, sessions.SourceAPI, "CP-LATE", d.Now(), 0, d.Now()); err != nil {
		t.Fatalf("create session: %v", err)
	}
	d.Hub.Publish()
	if data := nextEventData(t, stream); !strings.Contains(data, "CP-LATE") {
		t.Errorf("LIVE-1: event after the deadline lacks the new session: %s", data)
	}
}

// syncWriter makes a writer safe for the server's concurrent log writes.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// MIR-11, LIVE-1, OPS-3: with a stream open, stopping the hub ends it, so
// Serve's graceful shutdown finishes well inside its timeout instead of
// failing on the stream (Shutdown does not cancel request contexts).
func TestServe_MIR11_ShutdownEndsStreams(t *testing.T) {
	d, _ := testDeps(t, io.Discard)
	hub := live.NewHub(100)
	d.Hub = hub
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan error, 1)
	go func() { ran <- hub.Run(ctx) }()
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, New(d), 5*time.Second, d.Logger) }()

	openEvents(t, "http://"+ln.Addr().String())
	start := time.Now()
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("MIR-11: Serve = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("MIR-11: Serve did not return with a stream open")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("MIR-11: shutdown took %s, want well under the 5 s timeout", took)
	}
	if err := <-ran; err != nil {
		t.Errorf("hub.Run = %v", err)
	}
}

// LIVE-1: without a hub, GET /events is a 503 and the other routes still work.
func TestNew_LIVE1_NoHub(t *testing.T) {
	d, _ := testDeps(t, io.Discard)
	d.Hub = nil
	h := New(d)
	for path, want := range map[string]int{"/events?view=list": http.StatusServiceUnavailable, "/": http.StatusOK} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

// LIVE-3: New wires the API's writes to the hub, so a session started through
// the API reaches an open list stream without any other signal.
func TestNew_LIVE3_APIWritesReachStreams(t *testing.T) {
	d, _ := testDeps(t, io.Discard)
	srv := httptest.NewServer(New(d))
	t.Cleanup(srv.Close)
	_, stream := openEvents(t, srv.URL)

	body := `{"charger_id":"CP-API","started_at":"2026-11-08T12:00:00Z","meter_start_wh":0}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/sessions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/sessions: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/sessions = %d, want 201", resp.StatusCode)
	}
	if data := nextEventData(t, stream); !strings.Contains(data, "CP-API") {
		t.Errorf("LIVE-3: event after the API write lacks the new session: %s", data)
	}
}
