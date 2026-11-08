package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/sessions"
)

var sgt = time.FixedZone("SGT", 8*3600)

func basePlan() Plan {
	return Plan{
		ChargerID: "CP-01",
		Start:     time.Date(2026, 11, 6, 21, 40, 0, 0, sgt),
		Duration:  155 * time.Minute,
		Interval:  5 * time.Minute,
		PowerW:    7200,
	}
}

// Brief item: TestPoints.
func TestPoints(t *testing.T) {
	p := basePlan()
	pts := Points(p)
	if len(pts) != 32 {
		t.Fatalf("len = %d, want 32", len(pts))
	}
	if !pts[0].At.Equal(p.Start) || pts[0].MeterWh != 0 {
		t.Errorf("first = %v/%d, want %v/0", pts[0].At, pts[0].MeterWh, p.Start)
	}
	last := pts[len(pts)-1]
	wantEnd := time.Date(2026, 11, 7, 0, 15, 0, 0, sgt)
	if !last.At.Equal(wantEnd) || last.MeterWh != 18600 {
		t.Errorf("last = %v/%d, want %v/18600", last.At, last.MeterWh, wantEnd)
	}
	for i := 1; i < len(pts); i++ {
		if !pts[i].At.After(pts[i-1].At) || pts[i].MeterWh <= pts[i-1].MeterWh {
			t.Fatalf("point %d not strictly increasing: %v/%d after %v/%d",
				i, pts[i].At, pts[i].MeterWh, pts[i-1].At, pts[i-1].MeterWh)
		}
	}
}

// Brief item: TestPoints_UnevenDuration.
func TestPoints_UnevenDuration(t *testing.T) {
	p := basePlan()
	p.Duration = 12 * time.Minute
	var got []time.Duration
	for _, r := range Points(p) {
		got = append(got, r.At.Sub(p.Start))
	}
	want := []time.Duration{0, 5 * time.Minute, 10 * time.Minute, 12 * time.Minute}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("offsets = %v, want %v", got, want)
	}
}

// Brief item: Points uses integer math with a meter start offset and truncation.
func TestPoints_MeterStartAndTruncation(t *testing.T) {
	p := basePlan()
	p.MeterStartWh = 1000
	p.PowerW = 1000
	p.Interval = time.Second
	p.Duration = 2 * time.Second
	pts := Points(p)
	// 1000 W for 1 s = 0.277 Wh, truncated to 0; 2 s = 0.555 Wh, truncated to 0.
	for i, r := range pts {
		if r.MeterWh != 1000 {
			t.Errorf("point %d meter = %d, want 1000", i, r.MeterWh)
		}
	}
	if len(Points(Plan{})) != 0 {
		t.Error("Points of a zero plan should be empty, not loop forever")
	}
}

// Brief item: TestValidate; SES-2 (future readings rejected) is why speed > 1 needs a past end.
func TestValidate(t *testing.T) {
	p := basePlan()
	end := p.Start.Add(p.Duration)
	tests := []struct {
		name    string
		mod     func(*Plan)
		speed   int
		now     time.Time
		wantErr bool
	}{
		{"speed 60 ending after now", nil, 60, end.Add(-time.Minute), true},
		{"speed 60 ending exactly now", nil, 60, end, false},
		{"speed 1 ending in the future", nil, 1, end.Add(-time.Hour), false},
		{"speed 0", nil, 0, end, true},
		{"negative speed", nil, -1, end, true},
		{"zero interval", func(p *Plan) { p.Interval = 0 }, 1, end, true},
		{"zero duration", func(p *Plan) { p.Duration = 0 }, 1, end, true},
		{"negative power", func(p *Plan) { p.PowerW = -1 }, 1, end, true},
		{"negative meter start", func(p *Plan) { p.MeterStartWh = -1 }, 1, end, true},
		{"too many points", func(p *Plan) { p.Duration = 48 * time.Hour; p.Interval = 17 * time.Second }, 1, end.Add(72 * time.Hour), true},
		{"exactly max points", func(p *Plan) { p.Duration = 48 * time.Hour; p.Interval = 17280 * time.Millisecond }, 1, end.Add(72 * time.Hour), false},
		{"final meter at the cap", func(p *Plan) { p.Duration = time.Hour; p.PowerW = 1_000_000_000_000_000 }, 1, end, false},
		{"final meter over the cap", func(p *Plan) { p.Duration = time.Hour; p.PowerW = 1_000_000_000_000_001 }, 1, end, true},
		{"meter start plus energy over the cap", func(p *Plan) { p.Duration = time.Hour; p.MeterStartWh = 1; p.PowerW = 1_000_000_000_000_000 }, 1, end, true},
		{"meter start over the cap", func(p *Plan) { p.MeterStartWh = 1_000_000_000_000_001; p.PowerW = 0 }, 1, end, true},
		{"power that would overflow int64", func(p *Plan) { p.PowerW = 1 << 62 }, 1, end, true},
		{"lowercase charger", func(p *Plan) { p.ChargerID = "cp-01" }, 1, end, true},
		{"empty charger", func(p *Plan) { p.ChargerID = "" }, 1, end, true},
		{"33-char charger", func(p *Plan) { p.ChargerID = strings.Repeat("A", 33) }, 1, end, true},
		{"32-char charger", func(p *Plan) { p.ChargerID = strings.Repeat("A", 32) }, 1, end, false},
		{"SES-5 duration over 48h", func(p *Plan) { p.Duration = 48*time.Hour + time.Second; p.Interval = time.Hour }, 1, end.Add(72 * time.Hour), true},
		{"SES-5 duration exactly 48h", func(p *Plan) { p.Duration = 48 * time.Hour; p.Interval = time.Hour }, 1, end.Add(72 * time.Hour), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := p
			if tc.mod != nil {
				tc.mod(&q)
			}
			err := Validate(q, tc.speed, tc.now)
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

type recorded struct {
	Method, Path, Key, ContentType string
	Body                           map[string]any
}

// reply writes a JSON answer, failing the test if the write fails.
func reply(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write response: %v", err)
	}
}

type fakeAPI struct {
	t        *testing.T
	mu       sync.Mutex
	reqs     []recorded
	failPath string // when a request path ends with this, answer 422
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read request: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.Header.Get("X-API-Key"), r.Header.Get("Content-Type"), body})
	f.mu.Unlock()

	if f.failPath != "" && strings.HasSuffix(r.URL.Path, f.failPath) {
		reply(f.t, w, http.StatusUnprocessableEntity, `{"error":{"code":"invalid_reading","message":"nope"}}`)
		return
	}
	if r.URL.Path == "/api/sessions" {
		reply(f.t, w, http.StatusCreated, `{"id":"id-`+body["charger_id"].(string)+`"}`)
		return
	}
	reply(f.t, w, http.StatusOK, `{"id":"x"}`)
}

func (f *fakeAPI) snapshot() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func newClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, APIKey: "secret-key", HTTP: srv.Client()}
}

func plansFor(ids ...string) []Plan {
	var out []Plan
	for _, id := range ids {
		p := basePlan()
		p.ChargerID = id
		p.Start = time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
		out = append(out, p)
	}
	return out
}

// Brief item: TestRun_PostsFullLifecycle; API-1 (X-API-Key on every write), API contract field names.
func TestRun_PostsFullLifecycle(t *testing.T) {
	f := &fakeAPI{t: t}
	c := newClient(t, f)
	plans := plansFor("CP-01", "CP-02", "CP-03")
	n := len(Points(plans[0]))

	if err := Run(context.Background(), c, plans, 1_000_000); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := f.snapshot()
	counts := map[string]int{}
	byCharger := map[string][]recorded{}
	for _, r := range reqs {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.ContentType != "application/json" {
			t.Errorf("%s: Content-Type = %q", r.Path, r.ContentType)
		}
		if r.Key != "secret-key" {
			t.Errorf("%s: X-API-Key = %q", r.Path, r.Key)
		}
		switch {
		case r.Path == "/api/sessions":
			counts["start"]++
			for _, k := range []string{"charger_id", "started_at", "meter_start_wh"} {
				if _, ok := r.Body[k]; !ok {
					t.Errorf("start body missing %s: %v", k, r.Body)
				}
			}
			id := r.Body["charger_id"].(string)
			byCharger[id] = append(byCharger[id], r)
		case strings.HasSuffix(r.Path, "/meter-values"):
			counts["meter"]++
			for _, k := range []string{"timestamp", "meter_wh"} {
				if _, ok := r.Body[k]; !ok {
					t.Errorf("meter body missing %s: %v", k, r.Body)
				}
			}
			byCharger[strings.TrimSuffix(strings.TrimPrefix(r.Path, "/api/sessions/id-"), "/meter-values")] =
				append(byCharger[strings.TrimSuffix(strings.TrimPrefix(r.Path, "/api/sessions/id-"), "/meter-values")], r)
		case strings.HasSuffix(r.Path, "/stop"):
			counts["stop"]++
			for _, k := range []string{"stopped_at", "meter_stop_wh"} {
				if _, ok := r.Body[k]; !ok {
					t.Errorf("stop body missing %s: %v", k, r.Body)
				}
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.Path, "/api/sessions/id-"), "/stop")
			byCharger[id] = append(byCharger[id], r)
		default:
			t.Errorf("unexpected path %s", r.Path)
		}
	}
	if counts["start"] != 3 || counts["meter"] != 3*(n-2) || counts["stop"] != 3 {
		t.Errorf("counts = %v, want 3 / %d / 3", counts, 3*(n-2))
	}

	for id, rs := range byCharger {
		if len(rs) != n {
			t.Fatalf("%s: %d requests, want %d", id, len(rs), n)
		}
		if rs[0].Path != "/api/sessions" || !strings.HasSuffix(rs[n-1].Path, "/stop") {
			t.Errorf("%s: order wrong: first %s last %s", id, rs[0].Path, rs[n-1].Path)
		}
		var plan Plan
		for _, p := range plans {
			if p.ChargerID == id {
				plan = p
			}
		}
		pts := Points(plan)
		prev := time.Time{}
		for i, r := range rs {
			mkey := "meter_wh"
			switch i {
			case 0:
				mkey = "meter_start_wh"
			case n - 1:
				mkey = "meter_stop_wh"
			}
			if got := int64(r.Body[mkey].(float64)); got != pts[i].MeterWh {
				t.Errorf("%s: request %d %s = %d, want %d", id, i, mkey, got, pts[i].MeterWh)
			}
			key := map[bool]string{true: "started_at", false: "timestamp"}[i == 0]
			if i == n-1 {
				key = "stopped_at"
			}
			ts, err := time.Parse(time.RFC3339, r.Body[key].(string))
			if err != nil {
				t.Fatalf("%s: %s not RFC 3339: %v", id, key, err)
			}
			if !strings.HasSuffix(r.Body[key].(string), "Z") {
				t.Errorf("%s: %s not UTC: %v", id, key, r.Body[key])
			}
			if !ts.After(prev) {
				t.Errorf("%s: request %d out of time order", id, i)
			}
			prev = ts
		}
	}
}

// Brief item: a non-2xx response surfaces status and the API's error code (API-2), never the key.
func TestRun_ErrorCodeSurfaces(t *testing.T) {
	f := &fakeAPI{t: t, failPath: "/meter-values"}
	c := newClient(t, f)
	err := Run(context.Background(), c, plansFor("CP-01"), 1_000_000)
	if err == nil {
		t.Fatal("Run succeeded, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "422") || !strings.Contains(msg, "invalid_reading") {
		t.Errorf("error %q lacks status or code", msg)
	}
	if strings.Contains(msg, "secret-key") {
		t.Errorf("error leaks API key: %q", msg)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 422 || ae.Code != "invalid_reading" {
		t.Errorf("errors.As APIError = %+v", ae)
	}
	// The plan's loop stops at its first error: start plus one failed reading,
	// then the best-effort stop (see TestRun_FailedPlanStopsSessionAtLastAcceptedPoint).
	if got := len(f.snapshot()); got != 3 {
		t.Errorf("requests = %d, want 3", got)
	}
}

// Brief item: a non-JSON error body still yields the status.
func TestClient_NonJSONError(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	_, err := c.Start(context.Background(), "CP-01", time.Now(), 0)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v, want mention of 502", err)
	}
}

// API contract (SES-2): a 200 from meter-values (duplicate) is success.
func TestClient_MeterValueDuplicateIsSuccess(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reply(t, w, http.StatusOK, `{"id":"x"}`)
	}))
	if err := c.MeterValue(context.Background(), "x", time.Now(), 5); err != nil {
		t.Errorf("MeterValue: %v", err)
	}
}

// Brief item: Start needs an id in the response.
func TestClient_StartWithoutID(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reply(t, w, http.StatusCreated, `{}`)
	}))
	if _, err := c.Start(context.Background(), "CP-01", time.Now(), 0); err == nil {
		t.Error("Start accepted a response without an id")
	}
}

// Brief item: unreachable server is an error, and the key is not in it.
func TestClient_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &Client{BaseURL: url, APIKey: "secret-key", HTTP: &http.Client{Timeout: time.Second}}
	err := c.Stop(context.Background(), "x", time.Now(), 1)
	if err == nil || strings.Contains(err.Error(), "secret-key") {
		t.Errorf("err = %v", err)
	}
}

func TestNewClient_DefaultTimeout(t *testing.T) {
	c := NewClient("http://x", "k")
	if c.HTTP.Timeout != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", c.HTTP.Timeout)
	}
}

// Brief item: cancelling mid-run returns promptly with ctx's error and leaves no goroutines.
func TestRun_CancelMidRun(t *testing.T) {
	started := make(chan struct{}, 3)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sessions" {
			started <- struct{}{}
			reply(t, w, http.StatusCreated, `{"id":"x"}`)
			return
		}
		reply(t, w, http.StatusOK, `{"id":"x"}`)
	})
	c := newClient(t, h)
	plans := plansFor("CP-01", "CP-02")
	for i := range plans {
		plans[i].Interval = time.Hour
		plans[i].Duration = 5 * time.Hour
		plans[i].Start = time.Now().Add(-10 * time.Hour)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, plans, 1) }()
	<-started
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// SES-2: speed > 1 with a future end is rejected before any request is sent.
func TestRun_RejectsFutureReplayBeforeAnyRequest(t *testing.T) {
	f := &fakeAPI{t: t}
	c := newClient(t, f)
	good := plansFor("CP-01")[0]
	bad := plansFor("CP-02")[0]
	bad.Start = time.Now().Add(time.Hour)
	if err := Run(context.Background(), c, []Plan{good, bad}, 60); err == nil {
		t.Fatal("Run accepted a future replay at speed 60")
	}
	if got := len(f.snapshot()); got != 0 {
		t.Errorf("%d requests sent, want 0", got)
	}
}

// Timestamps keep sub-second precision (in UTC) so short intervals do not collide.
func TestClient_TimestampsKeepSubSecondPrecision(t *testing.T) {
	f := &fakeAPI{t: t}
	c := newClient(t, f)
	at := time.Date(2026, 11, 6, 21, 40, 0, 500_000_000, sgt)
	id, err := c.Start(context.Background(), "CP-01", at, 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.MeterValue(context.Background(), id, at.Add(250*time.Millisecond), 1); err != nil {
		t.Fatalf("MeterValue: %v", err)
	}
	if err := c.Stop(context.Background(), id, at.Add(750*time.Millisecond), 2); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	reqs := f.snapshot()
	want := []struct{ key, val string }{
		{"started_at", "2026-11-06T13:40:00.5Z"},
		{"timestamp", "2026-11-06T13:40:00.75Z"},
		{"stopped_at", "2026-11-06T13:40:01.25Z"},
	}
	for i, w := range want {
		if got := reqs[i].Body[w.key]; got != w.val {
			t.Errorf("request %d %s = %v, want %s", i, w.key, got, w.val)
		}
	}
}

// failOnMeter answers the nth meter-values call with 422 and delegates the rest.
func failOnMeter(t *testing.T, f *fakeAPI, nth int, stopCode string) http.Handler {
	var mu sync.Mutex
	meters := 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/meter-values") {
			mu.Lock()
			meters++
			n := meters
			mu.Unlock()
			if n == nth {
				reply(t, w, http.StatusUnprocessableEntity, `{"error":{"code":"invalid_reading","message":"nope"}}`)
				return
			}
		}
		if stopCode != "" && strings.HasSuffix(r.URL.Path, "/stop") {
			reply(t, w, http.StatusInternalServerError, `{"error":{"code":"`+stopCode+`","message":"no"}}`)
			return
		}
		f.ServeHTTP(w, r)
	})
}

// SES-3: when a plan fails after its session started, the simulator stops the
// session at the last point the server accepted, so the charger is not left active.
func TestRun_FailedPlanStopsSessionAtLastAcceptedPoint(t *testing.T) {
	f := &fakeAPI{t: t}
	c := newClient(t, failOnMeter(t, f, 2, ""))
	plans := plansFor("CP-01")
	err := Run(context.Background(), c, plans, 1_000_000)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "invalid_reading" {
		t.Fatalf("Run err = %v, want the original invalid_reading error", err)
	}
	reqs := f.snapshot() // start, accepted reading 1, (reading 2 rejected, not recorded), stop
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3: %+v", len(reqs), reqs)
	}
	pts := Points(plans[0])
	stop := reqs[2]
	if !strings.HasSuffix(stop.Path, "/stop") {
		t.Fatalf("last request = %s, want a stop", stop.Path)
	}
	if got := stop.Body["stopped_at"]; got != ts(pts[1].At) {
		t.Errorf("stopped_at = %v, want %s (last accepted point)", got, ts(pts[1].At))
	}
	if got := int64(stop.Body["meter_stop_wh"].(float64)); got != pts[1].MeterWh {
		t.Errorf("meter_stop_wh = %d, want %d", got, pts[1].MeterWh)
	}
}

// When the best-effort stop fails too, the original error is still returned and
// the stop failure is logged, not hidden.
func TestRun_FailedStopAttemptIsLoggedOriginalErrorReturned(t *testing.T) {
	f := &fakeAPI{t: t}
	c := newClient(t, failOnMeter(t, f, 1, "stop_exploded"))
	var logs strings.Builder
	c.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	err := Run(context.Background(), c, plansFor("CP-01"), 1_000_000)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "invalid_reading" {
		t.Fatalf("Run err = %v, want invalid_reading", err)
	}
	if strings.Contains(err.Error(), "stop_exploded") {
		t.Errorf("stop failure replaced the original error: %v", err)
	}
	if !strings.Contains(logs.String(), "stop_exploded") || !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("stop failure not logged: %q", logs.String())
	}
}

// A plan whose start was rejected has no session, so nothing is stopped.
func TestRun_FailedStartSendsNoStop(t *testing.T) {
	f := &fakeAPI{t: t, failPath: "/api/sessions"}
	c := newClient(t, f)
	if err := Run(context.Background(), c, plansFor("CP-01"), 1_000_000); err == nil {
		t.Fatal("Run succeeded, want error")
	}
	if got := len(f.snapshot()); got != 1 {
		t.Errorf("requests = %d, want only the rejected start", got)
	}
}

// cancelAfterStart cancels a context once the start response has been fully
// read, so the cancel lands deterministically after the session exists.
type cancelAfterStart struct {
	base   http.RoundTripper
	cancel func()
}

type cancelOnClose struct {
	io.ReadCloser
	cancel func()
}

func (b cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func (t cancelAfterStart) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err == nil && r.URL.Path == "/api/sessions" {
		resp.Body = cancelOnClose{resp.Body, t.cancel}
	}
	return resp, err
}

// SES-3: cancelling mid-run still stops the started session, using a context
// that is not cancelled.
func TestRun_CancelStopsStartedSession(t *testing.T) {
	f := &fakeAPI{t: t}
	c := newClient(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.HTTP = &http.Client{Transport: cancelAfterStart{base: http.DefaultTransport, cancel: cancel}}
	p := plansFor("CP-01")[0]
	p.Interval, p.Duration, p.Start = time.Hour, 5*time.Hour, time.Now().Add(-10*time.Hour)
	if err := Run(ctx, c, []Plan{p}, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	reqs := f.snapshot()
	if len(reqs) != 2 || !strings.HasSuffix(reqs[1].Path, "/stop") {
		t.Errorf("requests = %+v, want start then stop", reqs)
	}
}

// SES-2: the largest valid plan sends no more readings than a session may hold
// (points minus the start point and the stop).
func TestMaxPoints_SES2_FitsSessionReadingCap(t *testing.T) {
	p := Plan{ChargerID: "CP-01", Start: time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC),
		Duration: 10_000*time.Second + 999*time.Millisecond, Interval: time.Second, PowerW: 1000}
	if err := Validate(p, 1, p.Start.Add(24*time.Hour)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := len(Points(p)) - 2; got > sessions.MaxReadings {
		t.Errorf("readings = %d, want <= %d", got, sessions.MaxReadings)
	}
}
