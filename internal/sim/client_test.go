package sim

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// dropFirst answers every request with 200 (201 for a create), except that
// the first request whose path ends with dropSuffix is read and then has its
// connection closed with no response, as a server does when its IdleTimeout
// fires just as the client reuses the connection.
type dropFirst struct {
	t          *testing.T
	dropSuffix string

	mu      sync.Mutex
	dropped bool
	hits    map[string]int // requests seen per path
}

func (d *dropFirst) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		d.t.Errorf("read request: %v", err)
	}
	d.mu.Lock()
	if d.hits == nil {
		d.hits = map[string]int{}
	}
	d.hits[r.URL.Path]++
	drop := !d.dropped && strings.HasSuffix(r.URL.Path, d.dropSuffix)
	if drop {
		d.dropped = true
	}
	d.mu.Unlock()

	if drop {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			d.t.Errorf("hijack: %v", err)
			return
		}
		if err := conn.Close(); err != nil {
			d.t.Errorf("close hijacked conn: %v", err)
		}
		return
	}
	if r.URL.Path == "/api/sessions" {
		reply(d.t, w, http.StatusCreated, `{"id":"x"}`)
		return
	}
	reply(d.t, w, http.StatusOK, `{"id":"x"}`)
}

func (d *dropFirst) count(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hits[path]
}

// SES-2 (identical duplicate reading is a 200 no-op) and SES-3 (identical stop
// replay returns the original result): a reading or stop that got no response
// at all is safe to resend, so it is retried once and gets through.
func TestClient_SES2_SES3_RetriedOnceWhenConnectionDropped(t *testing.T) {
	tests := []struct {
		name string
		path string
		call func(*Client) error
	}{
		{"SES-2 meter value", "/api/sessions/x/meter-values", func(c *Client) error {
			return c.MeterValue(context.Background(), "x", time.Now(), 5)
		}},
		{"SES-3 stop", "/api/sessions/x/stop", func(c *Client) error {
			return c.Stop(context.Background(), "x", time.Now(), 5)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &dropFirst{t: t, dropSuffix: tt.path}
			c := newClient(t, d)
			if err := tt.call(c); err != nil {
				t.Fatalf("call after one dropped connection: %v", err)
			}
			if got := d.count(tt.path); got != 2 {
				t.Errorf("requests to %s = %d, want 2 (dropped, then retried)", tt.path, got)
			}
		})
	}
}

// SES-2/SES-3: a reading whose connection the server closed mid-plan no longer
// ends the session early; the plan runs to its own stop.
func TestRun_SES2_DroppedConnectionDoesNotStopSession(t *testing.T) {
	const meterPath, stopPath = "/api/sessions/x/meter-values", "/api/sessions/x/stop"
	d := &dropFirst{t: t, dropSuffix: meterPath}
	c := newClient(t, d)
	plans := plansFor("CP-01")
	if err := Run(context.Background(), c, plans, 1_000_000); err != nil {
		t.Fatalf("Run: %v", err)
	}
	readings := len(Points(plans[0])) - 2 // minus the start point and the stop
	if got := d.count(meterPath); got != readings+1 {
		t.Errorf("meter requests = %d, want %d (every reading, one of them twice)", got, readings+1)
	}
	if got := d.count(stopPath); got != 1 {
		t.Errorf("stop requests = %d, want 1 (the plan's own stop)", got)
	}
}

// SES-1: creating a session is not idempotent (a resend could open a second
// session), so a create that got no response is not retried.
func TestClient_SES1_StartNotRetriedWhenConnectionDropped(t *testing.T) {
	d := &dropFirst{t: t, dropSuffix: "/api/sessions"}
	c := newClient(t, d)
	_, err := c.Start(context.Background(), "CP-01", time.Now(), 0)
	if err == nil {
		t.Fatal("Start succeeded after a dropped connection, want the error")
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Errorf("error leaks the API key: %v", err)
	}
	if got := d.count("/api/sessions"); got != 1 {
		t.Errorf("create requests = %d, want 1 (no retry)", got)
	}
}

// SES-2/SES-3: when the server answered, its answer stands; a 4xx or 5xx on a
// reading or stop is returned, not retried.
func TestClient_SES2_SES3_ResponseIsNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusInternalServerError} {
		for _, path := range []string{"/api/sessions/x/meter-values", "/api/sessions/x/stop"} {
			t.Run(http.StatusText(status)+" "+path, func(t *testing.T) {
				var mu sync.Mutex
				hits := 0
				c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					mu.Lock()
					hits++
					mu.Unlock()
					reply(t, w, status, `{"error":{"code":"nope","message":"no"}}`)
				}))
				var err error
				if strings.HasSuffix(path, "/stop") {
					err = c.Stop(context.Background(), "x", time.Now(), 5)
				} else {
					err = c.MeterValue(context.Background(), "x", time.Now(), 5)
				}
				var ae *APIError
				if !errors.As(err, &ae) || ae.Status != status {
					t.Fatalf("err = %v, want APIError with status %d", err, status)
				}
				mu.Lock()
				defer mu.Unlock()
				if hits != 1 {
					t.Errorf("requests = %d, want 1 (no retry)", hits)
				}
			})
		}
	}
}

// failAndCancel fails every round trip without a response and cancels the
// caller's context, counting the attempts.
type failAndCancel struct {
	cancel func()
	mu     sync.Mutex
	calls  int
}

func (f *failAndCancel) RoundTrip(*http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	f.cancel()
	return nil, io.EOF
}

// A transport failure while the caller's context is done is not retried: the
// caller has given up, so Run can return promptly (and stop the session).
func TestClient_NoRetryAfterContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &failAndCancel{cancel: cancel}
	c := &Client{BaseURL: "http://sim.invalid", APIKey: "secret-key", HTTP: &http.Client{Transport: rt}}
	if err := c.MeterValue(ctx, "x", time.Now(), 5); err == nil {
		t.Fatal("MeterValue succeeded, want the transport error")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.calls != 1 {
		t.Errorf("round trips = %d, want 1 (no retry once ctx is done)", rt.calls)
	}
}

// The client drops an idle kept-alive connection before the server's 60 s
// IdleTimeout (internal/server/serve.go) can close it under a new request.
func TestNewClient_IdleConnTimeoutBelowServerIdleTimeout(t *testing.T) {
	c := NewClient("http://x", "k")
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", c.HTTP.Transport)
	}
	if tr.IdleConnTimeout != 30*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 30s", tr.IdleConnTimeout)
	}
	if tr == http.DefaultTransport {
		t.Error("NewClient shares http.DefaultTransport, want its own clone")
	}
}
