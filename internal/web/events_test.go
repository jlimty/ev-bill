package web

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/live"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
)

// streamEnv is an env whose Pages has a running hub and whose mux, served by
// a real httptest.Server, also has GET /events.
type streamEnv struct {
	*env
	hub *live.Hub
	srv *httptest.Server
}

// newStreamEnv starts the hub and the server. configure, if not nil, may
// change the server's settings before it starts. Cleanups run in reverse, so
// the hub stops (ending every stream) before the server closes.
func newStreamEnv(t *testing.T, maxSubs int, configure func(*http.Server)) *streamEnv {
	t.Helper()
	e := newEnv(t)
	hub := live.NewHub(maxSubs)
	e.p.Hub = hub
	e.p.RegisterEvents(e.mux)
	srv := httptest.NewUnstartedServer(e.mux)
	if configure != nil {
		configure(srv.Config)
	}
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("hub.Run = %v", err)
		}
	})
	return &streamEnv{env: e, hub: hub, srv: srv}
}

// sseClient reads server-sent events from one open stream.
type sseClient struct {
	t    *testing.T
	resp *http.Response
	r    *bufio.Reader
}

// openStream GETs path and fails the test unless the response is a 200 event
// stream. The body is closed when the test ends.
func openStream(t *testing.T, srv *httptest.Server, path string) *sseClient {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close stream body: %v", err)
		}
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
	}
	return &sseClient{t: t, resp: resp, r: bufio.NewReader(resp.Body)}
}

// next returns the next event's name and data, skipping comments such as
// heartbeats. Data lines are joined with "\n", as EventSource does.
func (c *sseClient) next() (event, data string, err error) {
	var lines []string
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if event != "" || lines != nil {
				return event, strings.Join(lines, "\n"), nil
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			lines = append(lines, strings.TrimPrefix(line, "data: "))
		default:
			return "", "", fmt.Errorf("unexpected stream line %q", line)
		}
	}
}

// mustNext is next, failing the test on a read error or a different event name.
func (c *sseClient) mustNext(wantEvent string) string {
	c.t.Helper()
	event, data, err := c.next()
	if err != nil {
		c.t.Fatalf("read event: %v", err)
	}
	if event != wantEvent {
		c.t.Fatalf("event = %q, want %q", event, wantEvent)
	}
	return data
}

// LIVE-1: the stream sends the current list straight away, with the stream
// and security headers, then a re-rendered list after each Publish.
func TestEvents_LIVE1_FirstEventAndUpdates(t *testing.T) {
	e := newStreamEnv(t, 100, nil)
	e.mockup("CP-OLD")
	c := openStream(t, e.srv, "/events?view=list")

	for name, want := range map[string]string{
		"Content-Type":            "text/event-stream",
		"Cache-Control":           "no-store",
		"X-Accel-Buffering":       "no",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": contentSecurityPolicy,
	} {
		if got := c.resp.Header.Get(name); got != want {
			t.Errorf("LIVE-1: %s = %q, want %q", name, got, want)
		}
	}

	first := c.mustNext("list")
	wantContains(t, first, "CP-OLD", badge("completed"), "$9.98")
	if strings.Contains(first, "CP-LIVE") {
		t.Fatal("LIVE-1: first event already has the session created later")
	}

	id := e.start("CP-LIVE", sgt(0, 15, 7))
	e.hub.Publish()
	second := c.mustNext("list")
	wantContains(t, second, `<a href="/sessions/`+id+`">CP-LIVE</a>`, badge("charging"), "CP-OLD")
}

// LIVE-1: the page and the stream render the same fragment, so the first
// event's HTML appears verbatim in the page.
func TestEvents_LIVE1_StreamMatchesPage(t *testing.T) {
	e := newStreamEnv(t, 100, nil)
	id := e.mockup("CP-01")
	e.start("CP-02", sgt(0, 15, 7))

	list := openStream(t, e.srv, "/events?view=list").mustNext("list")
	if page := e.get("/sessions").Body.String(); !strings.Contains(page, list) {
		t.Errorf("LIVE-1: list event is not part of the list page:\nevent: %s\npage: %s", list, page)
	}
	detail := openStream(t, e.srv, "/events?view=session&id="+id).mustNext("session")
	if page := e.get("/sessions/" + id).Body.String(); !strings.Contains(page, detail) {
		t.Errorf("LIVE-1: session event is not part of the detail page:\nevent: %s\npage: %s", detail, page)
	}
	wantContains(t, detail, badge("completed"), `style="width: 51.6%"`, "$9.98")
}

// LIVE-1: a session stream re-renders the session's live section on Publish.
func TestEvents_LIVE1_SessionUpdates(t *testing.T) {
	e := newStreamEnv(t, 100, nil)
	id := e.start("CP-01", sgt(0, 15, 7))
	c := openStream(t, e.srv, "/events?view=session&id="+id)
	wantContains(t, c.mustNext("session"), "No readings yet", badge("charging"))

	e.reading(id, sgt(0, 18, 7), 1500)
	e.hub.Publish()
	wantContains(t, c.mustNext("session"), "1.5 kWh", `class="bar"`)
}

// LIVE-1: each write sets its own deadline, so the server's WriteTimeout (10 s
// in Serve, 100 ms here) does not cut the stream off. Its ReadTimeout does not
// either: net/http clears the read deadline once the request is read.
func TestEvents_LIVE1_OutlivesServerTimeouts(t *testing.T) {
	e := newStreamEnv(t, 100, func(s *http.Server) {
		s.ReadTimeout = 100 * time.Millisecond
		s.WriteTimeout = 100 * time.Millisecond
	})
	c := openStream(t, e.srv, "/events?view=list")
	c.mustNext("list")

	time.Sleep(300 * time.Millisecond)
	e.start("CP-LATE", sgt(0, 15, 7))
	e.hub.Publish()
	wantContains(t, c.mustNext("list"), "CP-LATE")
}

// MIR-11: when the hub stops, every open stream ends, so http.Server.Shutdown
// (which does not cancel request contexts) can finish.
func TestEvents_MIR11_StreamEndsWhenHubStops(t *testing.T) {
	e := newEnv(t)
	hub := live.NewHub(100)
	e.p.Hub = hub
	e.p.RegisterEvents(e.mux)
	srv := httptest.NewServer(e.mux)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan error, 1)
	go func() { ran <- hub.Run(ctx) }()

	c := openStream(t, srv, "/events?view=list")
	c.mustNext("list")
	cancel()
	if err := <-ran; err != nil {
		t.Fatalf("hub.Run = %v", err)
	}
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, c.r)
		ended <- err
	}()
	select {
	case err := <-ended:
		if err != nil {
			t.Errorf("MIR-11: stream ended with %v, want a clean end", err)
		}
	case <-time.After(time.Second):
		t.Fatal("MIR-11: stream still open 1 s after the hub stopped")
	}
}

// LIVE-1: above 100 open streams the answer is 503, and a closed stream frees
// its slot. Without a hub, /events is 503 too.
func TestEvents_LIVE1_Over100Is503(t *testing.T) {
	e := newStreamEnv(t, 100, nil)
	var cancels []func()
	for range 99 {
		_, cancel, ok := e.hub.Subscribe()
		if !ok {
			t.Fatal("Subscribe refused below the cap")
		}
		cancels = append(cancels, cancel)
	}
	c := openStream(t, e.srv, "/events?view=list") // the 100th
	c.mustNext("list")

	rec := e.get("/events?view=list")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("LIVE-1: 101st stream = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") != contentSecurityPolicy {
		t.Error("LIVE-1: 503 lacks the security headers")
	}

	cancels[0]()
	openStream(t, e.srv, "/events?view=list").mustNext("list")

	noHub := newEnv(t)
	noHub.p.RegisterEvents(noHub.mux)
	if rec := noHub.get("/events?view=list"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("LIVE-1: /events without a hub = %d, want 503", rec.Code)
	}
}

// LIVE-1, LOC-7: only the list, session and locations views exist; a session
// must exist.
func TestEvents_BadView(t *testing.T) {
	e := newStreamEnv(t, 1, nil)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/events", http.StatusBadRequest},
		{"/events?view=nope", http.StatusBadRequest},
		{"/events?view=session", http.StatusBadRequest},
		{"/events?view=session&id=0b9a1f6e-6a39-4d3c-8f0e-1c2d3e4f5a6b", http.StatusNotFound},
		{"/events?view=session&id=not-a-uuid", http.StatusNotFound},
	} {
		rec := e.get(tc.path)
		if rec.Code != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
	// The refused requests released their subscriptions: the cap is 1.
	openStream(t, e.srv, "/events?view=list").mustNext("list")
}

// LIVE-1: a store failure before the first event is a 500 with the cause only
// in the log, and the subscription is released.
func TestEvents_LIVE1_StoreFailureIs500(t *testing.T) {
	e := newStreamEnv(t, 1, nil)
	e.pool.Close()
	rec := e.get("/events?view=list")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /events = %d, want 500", rec.Code)
	}
	if body := strings.ToLower(rec.Body.String()); strings.Contains(body, "pool") || strings.Contains(body, "list sessions") {
		t.Errorf("500 body leaks the cause: %q", body)
	}
	if !strings.Contains(e.logs.String(), "level=ERROR") {
		t.Errorf("error not logged: %q", e.logs.String())
	}
	_, cancel, ok := e.hub.Subscribe()
	if !ok {
		t.Fatal("LIVE-1: failed request kept its subscription")
	}
	cancel()
}

// fakeStream is an in-memory ResponseWriter for streams under synctest. It
// supports Flush and write deadlines, and can be told to fail each of them.
type fakeStream struct {
	mu            sync.Mutex
	header        http.Header
	status        int
	body          bytes.Buffer
	writeErr      error
	flushErr      error
	deadlineErr   error
	writeDeadline time.Time
	deadlines     []time.Time // every deadline set, in order
}

func newFakeStream() *fakeStream { return &fakeStream{header: http.Header{}} }

func (f *fakeStream) Header() http.Header { return f.header }

func (f *fakeStream) WriteHeader(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = code
}

func (f *fakeStream) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.body.Write(b)
}

func (f *fakeStream) FlushError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushErr
}

func (f *fakeStream) SetWriteDeadline(t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeDeadline = t
	f.deadlines = append(f.deadlines, t)
	return f.deadlineErr
}

func (f *fakeStream) snapshot() (body string, writeDeadline time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body.String(), f.writeDeadline
}

// deadlineLog returns every write deadline set so far, in order.
func (f *fakeStream) deadlineLog() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.deadlines...)
}

// wantDeadlines checks that each write set a deadline eventWriteTimeout after
// its own time, then cleared it once the event was flushed (LIVE-1).
func wantDeadlines(t *testing.T, f *fakeStream, writesAt ...time.Time) {
	t.Helper()
	var want []time.Time
	for _, at := range writesAt {
		want = append(want, at.Add(eventWriteTimeout), time.Time{})
	}
	got := f.deadlineLog()
	if len(got) != len(want) {
		t.Fatalf("LIVE-1: deadlines = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("LIVE-1: deadline %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// noFlushWriter cannot flush or set deadlines.
type noFlushWriter struct {
	header http.Header
	body   bytes.Buffer
}

func (w *noFlushWriter) Header() http.Header         { return w.header }
func (w *noFlushWriter) WriteHeader(int)             {}
func (w *noFlushWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

func streamPages() *Pages {
	return &Pages{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// fixedRender always renders the same fragment.
func fixedRender(html string) fragmentFunc {
	return func(context.Context) (fragment, error) { return fragment{html: []byte(html)}, nil }
}

// runStream runs p.stream on w in a goroutine and returns the channel it
// signals on and a function that waits for it to return.
func runStream(t *testing.T, p *Pages, w http.ResponseWriter, ctx context.Context, render fragmentFunc) (sub chan struct{}, wait func()) {
	t.Helper()
	sub = make(chan struct{}, 1)
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/events?view=list", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.stream(w, r, "list", sub, fragment{html: []byte("<tr>first</tr>")}, render)
	}()
	return sub, func() { <-done }
}

// LIVE-1: with no change for 25 s the stream writes a ": ping" comment, and
// every write first sets a write deadline 10 s ahead, then clears it once the
// event is flushed, so an idle stream carries no stale deadline.
func TestEvents_LIVE1_Heartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeStream()
		ctx, cancel := context.WithCancel(t.Context())
		start := time.Now()
		sub, wait := runStream(t, streamPages(), f, ctx, fixedRender("<tr>next</tr>"))

		synctest.Wait()
		body, dl := f.snapshot()
		if body != "event: list\ndata: <tr>first</tr>\n\n" {
			t.Fatalf("LIVE-1: first write = %q", body)
		}
		if !dl.IsZero() {
			t.Errorf("LIVE-1: write deadline still set after the flush: %v", dl.Sub(start))
		}
		wantDeadlines(t, f, start)

		time.Sleep(25*time.Second - time.Nanosecond)
		synctest.Wait()
		if body, _ := f.snapshot(); strings.Contains(body, ": ping") {
			t.Fatal("LIVE-1: heartbeat before 25 s")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		body, _ = f.snapshot()
		if !strings.HasSuffix(body, "\n\n: ping\n\n") {
			t.Fatalf("LIVE-1: no heartbeat at 25 s: %q", body)
		}
		wantDeadlines(t, f, start, start.Add(25*time.Second))

		time.Sleep(25 * time.Second)
		synctest.Wait()
		if body, _ := f.snapshot(); strings.Count(body, ": ping\n\n") != 2 {
			t.Errorf("LIVE-1: want 2 heartbeats after 50 s: %q", body)
		}

		sub <- struct{}{}
		synctest.Wait()
		if body, _ := f.snapshot(); !strings.HasSuffix(body, "event: list\ndata: <tr>next</tr>\n\n") {
			t.Errorf("LIVE-1: signal did not send the re-rendered fragment: %q", body)
		}

		cancel()
		wait()
		if f.status != http.StatusOK {
			t.Errorf("status = %d, want 200", f.status)
		}
	})
}

// LIVE-1: any failure to deliver an event ends the stream: a write error, a
// flush error, a write deadline that cannot be set (other than
// http.ErrNotSupported), or a failed re-render.
func TestEvents_LIVE1_WriteFailuresEndStream(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name   string
		setup  func(f *fakeStream)
		render fragmentFunc
		signal bool
	}{
		{"write error", func(f *fakeStream) { f.writeErr = boom }, fixedRender("x"), false},
		{"flush error", func(f *fakeStream) { f.flushErr = boom }, fixedRender("x"), false},
		{"deadline error", func(f *fakeStream) { f.deadlineErr = boom }, fixedRender("x"), false},
		{"render error", func(*fakeStream) {}, func(context.Context) (fragment, error) { return fragment{}, boom }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFakeStream()
				tc.setup(f)
				sub, wait := runStream(t, streamPages(), f, t.Context(), tc.render)
				if tc.signal {
					sub <- struct{}{}
				}
				wait() // returns without the context being cancelled
			})
		})
	}
}

// LIVE-1: a writer that cannot flush ends the stream after the first write.
func TestEvents_LIVE1_NoFlusherEndsStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &noFlushWriter{header: http.Header{}}
		_, wait := runStream(t, streamPages(), w, t.Context(), fixedRender("x"))
		wait()
		if !strings.HasPrefix(w.body.String(), "event: list\n") {
			t.Errorf("body = %q", w.body.String())
		}
	})
}

// LIVE-1: a writer that cannot set deadlines (http.ErrNotSupported) still
// streams; the stream ends when the request context is done or the
// subscription closes.
func TestEvents_LIVE1_DeadlinesNotSupportedStillStreams(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := httptest.NewRecorder()
		ctx, cancel := context.WithCancel(t.Context())
		sub, wait := runStream(t, streamPages(), rec, ctx, fixedRender("<tr>next</tr>"))
		sub <- struct{}{}
		time.Sleep(renderGap) // the signal came within renderGap of the first event
		synctest.Wait()
		cancel()
		wait()
		want := "event: list\ndata: <tr>first</tr>\n\nevent: list\ndata: <tr>next</tr>\n\n"
		if rec.Body.String() != want || !rec.Flushed {
			t.Errorf("body = %q (flushed %v), want %q", rec.Body.String(), rec.Flushed, want)
		}

		rec = httptest.NewRecorder()
		sub, wait = runStream(t, streamPages(), rec, t.Context(), fixedRender("x"))
		close(sub)
		wait()
	})
}

// LIVE-1: an event carries each line of the fragment as its own data line,
// with CR and CRLF line ends normalised so no line can break the framing.
func TestEventFrame_LIVE1(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"<tr>\n<td>a</td>\n</tr>\n", "event: list\ndata: <tr>\ndata: <td>a</td>\ndata: </tr>\n\n"},
		{"a\r\nb\rc", "event: list\ndata: a\ndata: b\ndata: c\n\n"},
		{"", "event: list\ndata: \n\n"},
	} {
		if got := string(eventFrame("list", []byte(tc.in))); got != tc.want {
			t.Errorf("eventFrame(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// LIVE-1, UI-1: the fragments the stream sends are escaped by html/template.
func TestFragments_EscapeValues(t *testing.T) {
	list, err := executeFragment("list", "list_body", listPage{Rows: []listRow{{ID: `"><x>`, Charger: "<script>", Status: `"><b>`}}})
	if err != nil {
		t.Fatalf("list_body: %v", err)
	}
	detail, err := executeFragment("detail", "detail_live", detailPage{Charger: "<script>", Status: `"><b>`})
	if err != nil {
		t.Fatalf("detail_live: %v", err)
	}
	for name, out := range map[string][]byte{"list_body": list, "detail_live": detail} {
		if bytes.Contains(out, []byte("<script>")) || bytes.Contains(out, []byte("<x>")) || bytes.Contains(out, []byte("<b>")) {
			t.Errorf("%s: unescaped output: %s", name, out)
		}
	}
	if _, err := executeFragment("list", "missing", nil); err == nil {
		t.Error("executeFragment with an unknown template = nil error")
	}
}

// LIVE-1: a stream that ends because the client left (context.Canceled) or
// its session was deleted (sessions.ErrNotFound) is a normal end and is logged
// at Info; any other render failure is logged at Error. Either way the stream
// ends.
func TestEvents_LIVE1_RenderEndLogLevels(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		err       error
		wantLevel string
	}{
		{"client gone", fmt.Errorf("list sessions: %w", context.Canceled), "level=INFO"},
		{"session deleted", fmt.Errorf("get session: %w", sessions.ErrNotFound), "level=INFO"},
		{"other failure", boom, "level=ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var logs bytes.Buffer
				p := &Pages{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
				render := func(context.Context) (fragment, error) { return fragment{}, tc.err }
				sub, wait := runStream(t, p, newFakeStream(), t.Context(), render)
				sub <- struct{}{}
				wait()
				out := logs.String()
				if !strings.Contains(out, tc.wantLevel) {
					t.Errorf("LIVE-1: log = %q, want %s", out, tc.wantLevel)
				}
				if tc.wantLevel == "level=INFO" && strings.Contains(out, "level=ERROR") {
					t.Errorf("LIVE-1: normal end logged as an error: %q", out)
				}
			})
		})
	}
}

// LIVE-1: clearing the deadline after the flush tolerates a writer that
// cannot set deadlines, but any other failure to clear it ends the stream.
func TestEvents_LIVE1_ClearDeadlineFailureEndsStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &clearFailStream{fakeStream: newFakeStream()}
		_, wait := runStream(t, streamPages(), f, t.Context(), fixedRender("x"))
		wait() // ends after the first write, without the context being cancelled
		if body, _ := f.snapshot(); !strings.HasPrefix(body, "event: list\n") {
			t.Errorf("body = %q", body)
		}
	})
}

// clearFailStream accepts a real deadline but fails to clear one.
type clearFailStream struct{ *fakeStream }

func (c *clearFailStream) SetWriteDeadline(t time.Time) error {
	if t.IsZero() {
		return errors.New("cannot clear")
	}
	return c.fakeStream.SetWriteDeadline(t)
}

// LIVE-3: with no sessions the list event still carries the table body and
// its empty-state row. EventSource drops an event with empty data, and the
// element live.js swaps must exist on the empty page too.
func TestEvents_LIVE3_EmptyListFragmentIsNotEmpty(t *testing.T) {
	e := newStreamEnv(t, 100, nil)
	first := openStream(t, e.srv, "/events?view=list").mustNext("list")
	wantContains(t, first, "<tbody>", "No sessions yet")
	if page := e.get("/sessions").Body.String(); !strings.Contains(page, `<div class="card" data-live="list">`+first) {
		t.Errorf("LIVE-3: empty list event is not the content of the swapped element:\nevent: %s\npage: %s", first, page)
	}
}

// LIVE-1, LIVE-2: through the real handler and store, five streams on the
// list view cost one store render per Publish between them, and each still
// gets the new row.
func TestEvents_LIVE2_OneStoreRenderPerPublish(t *testing.T) {
	e := newStreamEnv(t, 100, nil)
	var clients []*sseClient
	for range 5 {
		c := openStream(t, e.srv, "/events?view=list")
		c.mustNext("list")
		clients = append(clients, c)
	}
	before := e.p.renders.renders.Load()
	e.start("CP-NEW", sgt(0, 15, 7))
	e.hub.Publish()
	for i, c := range clients {
		if got := c.mustNext("list"); !strings.Contains(got, "CP-NEW") {
			t.Errorf("LIVE-1: stream %d update lacks the new session: %s", i, got)
		}
	}
	if n := e.p.renders.renders.Load() - before; n != 1 {
		t.Errorf("LIVE-2: one Publish to 5 list streams made %d store renders, want 1", n)
	}
}

// LIVE-1: a session stream sends a completed session's fragment, then a done
// event with data 1, and ends, freeing its slot: on connect for a session
// already completed, and when a charging session completes mid-stream.
func TestEvents_LIVE1_CompletedSessionSendsDone(t *testing.T) {
	e := newStreamEnv(t, 1, nil)
	done := e.mockup("CP-DONE")
	c := openStream(t, e.srv, "/events?view=session&id="+done)
	wantContains(t, c.mustNext("session"), badge("completed"))
	if data := c.mustNext("done"); data != "1" {
		t.Errorf("LIVE-1: done data = %q, want 1", data)
	}
	if _, _, err := c.next(); !errors.Is(err, io.EOF) {
		t.Errorf("LIVE-1: after done: %v, want the stream to end", err)
	}

	id := e.start("CP-01", sgt(0, 15, 7))
	c = openStream(t, e.srv, "/events?view=session&id="+id) // the cap is 1: the first stream freed its slot
	wantContains(t, c.mustNext("session"), badge("charging"))
	if _, _, _, err := e.st.StopSession(context.Background(), sessions.SourceAPI, id, billing.Reading{At: sgt(0, 18, 7), MeterWh: 1500}, fixedNow()); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	e.hub.Publish()
	wantContains(t, c.mustNext("session"), badge("completed"), "1.5 kWh")
	c.mustNext("done")
	if _, _, err := c.next(); !errors.Is(err, io.EOF) {
		t.Errorf("LIVE-1: after done: %v, want the stream to end", err)
	}
}

// LIVE-1: "GET /events" also matches HEAD. HEAD gets the stream's headers and
// an empty body at once, and holds no stream slot. A bad view is still 400.
func TestEvents_LIVE1_HeadHoldsNoSlot(t *testing.T) {
	e := newStreamEnv(t, 1, nil)
	for range 3 {
		// A HEAD that streamed would end only with this context, with a body.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodHead, "/events?view=list", nil))
		cancel()
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("LIVE-1: HEAD /events = %d with %d body bytes, want 200 and none", rec.Code, rec.Body.Len())
		}
		for name, want := range map[string]string{
			"Content-Type":            "text/event-stream",
			"Cache-Control":           "no-store",
			"Content-Security-Policy": contentSecurityPolicy,
		} {
			if got := rec.Header().Get(name); got != want {
				t.Errorf("LIVE-1: HEAD %s = %q, want %q", name, got, want)
			}
		}
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/events?view=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("LIVE-1: HEAD with a bad view = %d, want 400", rec.Code)
	}
	if e.p.renders.renders.Load() != 0 {
		t.Error("LIVE-1: HEAD rendered a fragment")
	}
	// The cap is 1, so the HEADs hold no slot if a stream still opens.
	openStream(t, e.srv, "/events?view=list").mustNext("list")
}

// LIVE-1: a final fragment (a completed session) is followed by the done
// event in the same write, and the stream ends without the request ending,
// whether it is the first event or a later one.
func TestEvents_LIVE1_FinalFragmentEndsStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeStream()
		final := func(context.Context) (fragment, error) {
			return fragment{html: []byte("<p>completed</p>"), final: true}, nil
		}
		sub, wait := runStream(t, streamPages(), f, t.Context(), final)
		sub <- struct{}{}
		wait()
		body, _ := f.snapshot()
		if want := "event: list\ndata: <tr>first</tr>\n\nevent: list\ndata: <p>completed</p>\n\n" + doneFrame; body != want {
			t.Errorf("LIVE-1: body = %q, want %q", body, want)
		}

		f = newFakeStream()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/events", nil)
		streamPages().stream(f, r, "session", make(chan struct{}), fragment{html: []byte("x"), final: true}, final)
		if body, _ := f.snapshot(); body != "event: session\ndata: x\n\n"+doneFrame {
			t.Errorf("LIVE-1: final first event: body = %q", body)
		}
	})
}

// LOC-7, LIVE-1, LIVE-2: the locations view streams the same locations_body
// the page renders, first on connect and again after Publish, and its streams
// share one render per hub generation through the render cache.
func TestEvents_LOC7_LocationsView(t *testing.T) {
	e := newStreamEnv(t, 100, nil)
	e.observe([]store.ChargerRow{locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Available)}, nil)

	c := openStream(t, e.srv, "/events?view=locations")
	first := c.mustNext("locations")
	wantContains(t, first, "<h2>Alpha Mall</h2>", `<li class="cell free" title="A-001 · 22 kW AC · Type 2 · free"></li>`)
	if page := e.get("/").Body.String(); !strings.Contains(page, `<div data-live="locations">`+first) {
		t.Errorf("LOC-7: locations event is not the content of the swapped element:\nevent: %s\npage: %s", first, page)
	}
	other := openStream(t, e.srv, "/events?view=locations")
	other.mustNext("locations")

	busy := locCharger("A-001", "100001", "Alpha Mall", "Testco", datamall.Occupied)
	busy.ObservedAt = locObserved.Add(time.Minute)
	e.observe([]store.ChargerRow{busy}, nil)
	before := e.p.renders.renders.Load()
	e.hub.Publish()
	for _, s := range []*sseClient{c, other} {
		wantContains(t, s.mustNext("locations"), `<li class="cell busy" title="A-001 · 22 kW AC · Type 2 · busy"></li>`)
	}
	if n := e.p.renders.renders.Load() - before; n != 1 {
		t.Errorf("LIVE-2: one Publish to 2 locations streams made %d renders, want 1", n)
	}
}

// LOC-7: a store failure on the locations view is a 500 before the first
// event, with the cause only in the log.
func TestEvents_LOC7_LocationsStoreFailureIs500(t *testing.T) {
	e := newStreamEnv(t, 1, nil)
	e.pool.Close()
	rec := e.get("/events?view=locations")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /events?view=locations = %d, want 500", rec.Code)
	}
	if !strings.Contains(e.logs.String(), "location scope") {
		t.Errorf("cause not logged: %q", e.logs.String())
	}
}
