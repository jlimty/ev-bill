package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jlimty/ev-bill/internal/live"
)

// countingRender renders "<tr>v=N</tr>" for the current value of version and
// counts its calls: each call stands for one store render.
type countingRender struct {
	calls   atomic.Int64
	version atomic.Int64
}

func (c *countingRender) render(context.Context) (fragment, error) {
	c.calls.Add(1)
	return fragment{html: fmt.Appendf(nil, "<tr>v=%d</tr>", c.version.Load())}, nil
}

// fakeViewer is one stream opened the way events opens it: hold the view,
// subscribe, render the first event through the cache, then stream.
type fakeViewer struct {
	w    *fakeStream
	done chan struct{}
}

// openViewer opens a fakeViewer on key in the current synctest bubble. It
// ends when ctx is done.
func openViewer(t *testing.T, ctx context.Context, p *Pages, key string, render fragmentFunc) *fakeViewer {
	t.Helper()
	sub, cancel, ok := p.Hub.Subscribe()
	if !ok {
		t.Fatal("Subscribe refused")
	}
	v := &fakeViewer{w: newFakeStream(), done: make(chan struct{})}
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/events?view=list", nil)
	go func() {
		defer close(v.done)
		defer cancel()
		release := p.renders.hold(key)
		defer release()
		first, err := p.cachedRender(key, renderGap, render)(ctx)
		if err != nil {
			t.Errorf("first render: %v", err)
			return
		}
		p.stream(v.w, r, "list", sub, first, p.cachedRender(key, 0, render))
	}()
	return v
}

// events returns the data of each list event the viewer received so far.
func (v *fakeViewer) events() []string {
	body, _ := v.w.snapshot()
	var out []string
	for _, frame := range strings.Split(body, "\n\n") {
		if data, ok := strings.CutPrefix(frame, "event: list\ndata: "); ok {
			out = append(out, data)
		}
	}
	return out
}

// startTestHub runs a hub in the current bubble until the test ends.
func startTestHub(t *testing.T) *live.Hub {
	t.Helper()
	hub := live.NewHub(100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := hub.Run(ctx); err != nil {
			t.Errorf("hub.Run = %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return hub
}

// LIVE-1, LIVE-2: N streams on one view cost one store render per hub
// generation, not one per stream. Viewers that connect together share the
// first render, and after a Publish they all get the bytes of one re-render.
func TestEvents_LIVE2_StreamsOnOneViewShareOneRender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := streamPages()
		p.Hub = startTestHub(t)
		ctx, cancel := context.WithCancel(t.Context())
		var cr countingRender
		const n = 20
		viewers := make([]*fakeViewer, n)
		for i := range viewers {
			viewers[i] = openViewer(t, ctx, p, "list", cr.render)
		}
		synctest.Wait()
		if got := cr.calls.Load(); got != 1 {
			t.Fatalf("LIVE-2: %d viewers connecting together made %d store renders, want 1", n, got)
		}

		time.Sleep(renderGap)
		cr.version.Store(1)
		p.Hub.Publish()
		synctest.Wait()
		if got := cr.calls.Load(); got != 2 {
			t.Errorf("LIVE-2: one Publish to %d streams made %d store renders, want 1 more (2 in all)", n, got)
		}
		for i, v := range viewers {
			if got := v.events(); len(got) != 2 || got[1] != "<tr>v=1</tr>" {
				t.Errorf("LIVE-1: viewer %d events = %q, want the first and the v=1 update", i, got)
			}
		}

		// A different view renders on its own.
		other := openViewer(t, ctx, p, "session:x", cr.render)
		synctest.Wait()
		if got := cr.calls.Load(); got != 3 {
			t.Errorf("LIVE-2: a second view's first render: %d store renders, want 3", got)
		}
		cancel()
		for _, v := range append(viewers, other) {
			<-v.done
		}
	})
}

// LIVE-1: a burst of 50 Publishes within 2 s gives each stream at most two
// renders: one straight away, and one when the 2 s gap ends, which carries
// the burst's last change, so nothing is lost. The first event on connect is
// not delayed.
func TestEvents_LIVE1_BurstRendersAtMostTwicePerStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := streamPages()
		p.Hub = startTestHub(t)
		ctx, cancel := context.WithCancel(t.Context())
		var cr countingRender
		viewers := []*fakeViewer{
			openViewer(t, ctx, p, "list", cr.render),
			openViewer(t, ctx, p, "list", cr.render),
			openViewer(t, ctx, p, "list", cr.render),
		}
		synctest.Wait()
		for i, v := range viewers {
			if got := v.events(); len(got) != 1 {
				t.Fatalf("LIVE-1: viewer %d has %d events at connect, want the first straight away", i, len(got))
			}
		}

		time.Sleep(5 * time.Second) // well past the first event's gap
		start := cr.calls.Load()
		for i := range 50 {
			cr.version.Store(int64(i + 1))
			p.Hub.Publish()
			time.Sleep(39 * time.Millisecond) // 50 × 39 ms = 1.95 s
		}
		time.Sleep(renderGap)
		synctest.Wait()
		for i, v := range viewers {
			got := v.events()[1:]
			if len(got) == 0 || len(got) > 2 {
				t.Errorf("LIVE-1: viewer %d got %d updates for the burst, want 1 or 2: %q", i, len(got), got)
				continue
			}
			if last := got[len(got)-1]; last != "<tr>v=50</tr>" {
				t.Errorf("LIVE-1: viewer %d last update = %q, want the burst's last change v=50", i, last)
			}
		}
		if renders := cr.calls.Load() - start; renders > 2 {
			t.Errorf("LIVE-1: the burst made %d store renders for 3 viewers, want at most 2", renders)
		}
		cancel()
		for _, v := range viewers {
			<-v.done
		}
	})
}

// LIVE-1: at most two renders run at once across all views, so the 10
// connection pool always keeps connections for /healthz and the reconciler.
// A render that cannot get a slot within renderTimeout fails rather than
// queueing for ever.
func TestRenderCache_LIVE1_AtMostTwoRendersAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c renderCache
		var mu sync.Mutex
		active, peak := 0, 0
		release := make(chan struct{})
		render := func(context.Context) (fragment, error) {
			mu.Lock()
			active++
			peak = max(peak, active)
			mu.Unlock()
			<-release
			mu.Lock()
			active--
			mu.Unlock()
			return fragment{html: []byte("x")}, nil
		}
		errs := make(chan error, 6)
		for i := range 6 {
			go func() {
				_, err := c.get(t.Context(), fmt.Sprintf("session:%d", i), 1, 0, render)
				errs <- err
			}()
		}
		synctest.Wait()
		mu.Lock()
		if active != 2 {
			t.Errorf("LIVE-1: %d renders running with 6 views waiting, want 2", active)
		}
		mu.Unlock()
		close(release)
		for range 6 {
			if err := <-errs; err != nil {
				t.Errorf("render: %v", err)
			}
		}
		if peak != 2 {
			t.Errorf("LIVE-1: peak concurrent renders = %d, want 2", peak)
		}

		// Two renders that hang hold both slots: a third view gives up after
		// renderTimeout.
		hang := make(chan struct{})
		stuck := func(context.Context) (fragment, error) { <-hang; return fragment{}, nil }
		for i := range 2 {
			go func() {
				if _, err := c.get(t.Context(), fmt.Sprintf("stuck:%d", i), 1, 0, stuck); err != nil {
					t.Errorf("stuck render: %v", err)
				}
			}()
		}
		synctest.Wait()
		begin := time.Now()
		_, err := c.get(t.Context(), "list", 1, 0, render)
		if err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(begin) != renderTimeout {
			t.Errorf("LIVE-1: render with no free slot = %v after %v, want a deadline error after %v", err, time.Since(begin), renderTimeout)
		}
		close(hang)
		synctest.Wait()
	})
}

// LIVE-2: the cache holds a view's render only while a stream holds the view,
// so it stays bounded by the open streams.
func TestRenderCache_LIVE2_ForgetsViewWithNoStreams(t *testing.T) {
	var c renderCache
	var cr countingRender
	r1 := c.hold("session:a")
	r2 := c.hold("session:a")
	if _, err := c.get(context.Background(), "session:a", 1, 0, cr.render); err != nil {
		t.Fatal(err)
	}
	r1()
	if _, err := c.get(context.Background(), "session:a", 1, 0, cr.render); err != nil || cr.calls.Load() != 1 {
		t.Errorf("LIVE-2: with one stream left the render was not reused (%d renders, err %v)", cr.calls.Load(), err)
	}
	r2()
	c.mu.Lock()
	n := len(c.views)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("LIVE-2: %d views cached after the last stream ended, want 0", n)
	}
}

// LIVE-1: a render is reused for its own or an older generation, never for a
// newer one; a failed render is never reused; and with a maxAge (the first
// event on connect) a finished render older than maxAge is not reused.
func TestRenderCache_LIVE1_ReuseRules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c renderCache
		release := c.hold("list")
		defer release()
		var cr countingRender
		get := func(gen uint64, maxAge time.Duration) {
			t.Helper()
			if _, err := c.get(t.Context(), "list", gen, maxAge, cr.render); err != nil {
				t.Fatalf("get: %v", err)
			}
		}
		wantCalls := func(what string, n int64) {
			t.Helper()
			if got := cr.calls.Load(); got != n {
				t.Errorf("LIVE-1: %s: %d store renders, want %d", what, got, n)
			}
		}
		get(1, 0)
		get(1, 0)
		wantCalls("same generation", 1)
		get(0, 0)
		wantCalls("older generation", 1)
		get(2, 0)
		wantCalls("newer generation", 2)

		time.Sleep(time.Second)
		get(2, renderGap)
		wantCalls("first event within renderGap", 2)
		time.Sleep(renderGap)
		get(2, renderGap)
		wantCalls("first event after renderGap", 3)
		time.Sleep(time.Hour)
		get(2, 0)
		wantCalls("signal with no maxAge", 3)

		boom := errors.New("boom")
		fail := func(context.Context) (fragment, error) { cr.calls.Add(1); return fragment{}, boom }
		if _, err := c.get(t.Context(), "list", 3, 0, fail); !errors.Is(err, boom) {
			t.Fatalf("failed render = %v, want boom", err)
		}
		get(3, 0)
		wantCalls("after a failed render", 5)
	})
}

// LIVE-1: streams waiting on one render all get it even when the stream that
// started it leaves, and a waiter that leaves stops waiting at once.
func TestRenderCache_LIVE1_SharedRenderSurvivesLeaver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c renderCache
		release := c.hold("list")
		defer release()
		proceed := make(chan struct{})
		var sawCancel atomic.Bool
		render := func(ctx context.Context) (fragment, error) {
			<-proceed
			sawCancel.Store(ctx.Err() != nil)
			return fragment{html: []byte("shared")}, nil
		}
		leaderCtx, leave := context.WithCancel(t.Context())
		go func() {
			// The leader's result does not matter: it has left.
			if _, err := c.get(leaderCtx, "list", 1, 0, render); err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("leader: %v", err)
			}
		}()
		synctest.Wait()

		quitterCtx, quit := context.WithCancel(t.Context())
		quitErr := make(chan error, 1)
		go func() {
			_, err := c.get(quitterCtx, "list", 1, 0, render)
			quitErr <- err
		}()
		got := make(chan string, 1)
		go func() {
			f, err := c.get(t.Context(), "list", 1, 0, render)
			if err != nil {
				t.Errorf("waiter: %v", err)
			}
			got <- string(f.html)
		}()
		synctest.Wait()
		quit()
		if err := <-quitErr; !errors.Is(err, context.Canceled) {
			t.Errorf("LIVE-1: a waiter that left got %v, want context.Canceled", err)
		}
		leave()
		synctest.Wait()
		close(proceed)
		if html := <-got; html != "shared" {
			t.Errorf("LIVE-1: waiter got %q, want the shared render", html)
		}
		if sawCancel.Load() {
			t.Error("LIVE-1: the render was cancelled when the stream that started it left")
		}
	})
}
