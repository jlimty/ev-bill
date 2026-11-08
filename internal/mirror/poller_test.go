package mirror

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

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/live"
)

const testAccountKey = "SECRET-ACCOUNT-KEY-123"

// fakeFetcher returns a snapshot stamped with the (bubble) clock, or the next
// queued error.
type fakeFetcher struct {
	mu    sync.Mutex
	calls int
	errs  []error
	conns []datamall.Connector
	block bool // wait for ctx to end, then return its error
}

func (f *fakeFetcher) Fetch(ctx context.Context, _ map[string]bool) (datamall.Snapshot, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	conns := f.conns
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return datamall.Snapshot{}, ctx.Err()
	}
	if err != nil {
		return datamall.Snapshot{}, err
	}
	return datamall.Snapshot{UpdatedAt: time.Now(), Connectors: conns}, nil
}

func (f *fakeFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newPoller(f Fetcher) (*Poller, *syncBuffer) {
	log, logs := newLogger()
	return &Poller{
		Fetcher:     f,
		Reconciler:  &Reconciler{Store: newFakeStore(), Now: time.Now, Logger: log, GapAfter: GapAfter},
		PostalCodes: map[string]bool{"100001": true},
		Interval:    PollInterval,
		Feed:        &live.Feed{},
		Logger:      log,
	}, logs
}

// runPoller runs p in the background and returns a stop function that cancels it
// and returns Run's result.
func runPoller(p *Poller) func() error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	return func() error {
		cancel()
		return <-done
	}
}

// MIR-1: one poll at startup, then one every interval.
func TestPoller_MIR1_PollsAtStartAndEveryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeFetcher{}
		p, _ := newPoller(f)
		stop := runPoller(p)

		synctest.Wait()
		if n := f.count(); n != 1 {
			t.Fatalf("at t=0: %d fetches, want 1", n)
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if n := f.count(); n != 3 {
			t.Fatalf("at t=10m: %d fetches, want 3", n)
		}
		if err := stop(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	})
}

// MIR-1: a failed cycle is logged and recorded in the feed state, the poller
// keeps going, and the next success clears the error and records LastGood as
// the feed's own timestamp.
func TestPoller_MIR1_FailureLoggedAndRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeFetcher{errs: []error{errors.New("datamall: GET https://example.test/file: status 403")}}
		p, logs := newPoller(f)
		p.Feed.Store(live.FeedState{Enabled: true})
		stop := runPoller(p)

		synctest.Wait()
		st := p.Feed.Load()
		if !st.Enabled || !strings.Contains(st.LastError, "403") || !st.LastGood.IsZero() {
			t.Fatalf("after a failure: feed = %+v, want Enabled with LastError set and no LastGood", st)
		}
		if !strings.Contains(logs.String(), msgPollFailed) {
			t.Errorf("failure not logged; logs:\n%s", logs)
		}

		time.Sleep(PollInterval)
		synctest.Wait()
		st = p.Feed.Load()
		if st.LastError != "" || !st.LastGood.Equal(time.Now()) || !st.Enabled {
			t.Fatalf("after a success: feed = %+v, want LastError cleared and LastGood = %v", st, time.Now())
		}
		if err := stop(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	})
}

// MIR-1: a reconcile failure is a failed cycle too.
func TestPoller_MIR1_ReconcileFailureRecorded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, logs := newPoller(&fakeFetcher{})
		p.Reconciler.Store.(*fakeStore).errs["ChargerStates"] = errors.New("db down")
		stop := runPoller(p)
		synctest.Wait()
		if st := p.Feed.Load(); !strings.Contains(st.LastError, "db down") {
			t.Errorf("feed = %+v, want the reconcile error recorded", st)
		}
		if !strings.Contains(logs.String(), msgPollFailed) {
			t.Errorf("reconcile failure not logged; logs:\n%s", logs)
		}
		if err := stop(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	})
}

// MIR-11: cancelling the context ends Run with nil, including in the middle of
// a fetch, and that is not recorded as a feed failure. synctest.Test fails if
// any goroutine is left behind.
func TestPoller_MIR11_StopsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, _ := newPoller(&fakeFetcher{})
		stop := runPoller(p)
		synctest.Wait()
		if err := stop(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		p, logs := newPoller(&fakeFetcher{block: true})
		stop := runPoller(p)
		synctest.Wait()
		if err := stop(); err != nil {
			t.Fatalf("Run (cancelled mid-fetch) = %v, want nil", err)
		}
		if st := p.Feed.Load(); st.LastError != "" {
			t.Errorf("shutdown recorded as a feed error: %+v", st)
		}
		if strings.Contains(logs.String(), msgPollFailed) {
			t.Errorf("shutdown logged as a failed poll:\n%s", logs)
		}
	})
}

// LIVE-2: OnChange runs only after a cycle that changed something.
func TestPoller_LIVE2_OnChangeOnlyWhenChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeFetcher{conns: []datamall.Connector{ac("SG-1", datamall.Available)}}
		p, _ := newPoller(f)
		var changes atomic.Int32
		p.OnChange = func() { changes.Add(1) }
		stop := runPoller(p)

		synctest.Wait()
		if n := changes.Load(); n != 1 {
			t.Fatalf("after the first poll: OnChange ran %d times, want 1 (new charger)", n)
		}
		time.Sleep(PollInterval)
		synctest.Wait()
		if n := changes.Load(); n != 1 {
			t.Fatalf("after an identical poll: OnChange ran %d times, want still 1", n)
		}
		f.mu.Lock()
		f.conns = []datamall.Connector{ac("SG-1", datamall.Occupied)}
		f.mu.Unlock()
		time.Sleep(PollInterval)
		synctest.Wait()
		if n := changes.Load(); n != 2 {
			t.Fatalf("after a status change: OnChange ran %d times, want 2", n)
		}
		if err := stop(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	})
}

// MIR-1: with the real client against a fake DataMall, a failed download is
// recorded, and neither the AccountKey nor the presigned link's query string
// reaches the log or the feed state. Real network I/O, so no synctest bubble.
func TestPoller_MIR1_RealClientFailureNeverLeaksKey(t *testing.T) {
	t.Parallel()
	const presignedQuery = "X-Amz-Security-Token=TOPSECRET"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/file" {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		if _, err := fmt.Fprintf(w, `{"value":[{"Link":"%s/file?%s"}]}`, srv.URL, presignedQuery); err != nil {
			t.Errorf("write link: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := &datamall.Client{BaseURL: srv.URL, AccountKey: testAccountKey, HTTP: srv.Client(), Loc: billing.SingaporeTariff().Location}
	p, logs := newPoller(client)
	p.Interval = time.Hour
	stop := runPoller(p)
	deadline := time.Now().Add(10 * time.Second)
	for p.Feed.Load().LastError == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	st := p.Feed.Load()
	if !strings.Contains(st.LastError, "403") {
		t.Fatalf("feed = %+v, want the 403 recorded", st)
	}
	for _, secret := range []string{testAccountKey, "TOPSECRET"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(st.LastError, secret) {
			t.Errorf("%q leaked; logs:\n%s\nfeed error: %s", secret, logs, st.LastError)
		}
	}
}
