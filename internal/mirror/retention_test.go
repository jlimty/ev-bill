package mirror

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type fakeDeleter struct {
	mu      sync.Mutex
	cutoffs []time.Time
	err     error
}

func (d *fakeDeleter) DeleteCompletedMirrorBefore(_ context.Context, cutoff time.Time) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cutoffs = append(d.cutoffs, cutoff)
	if d.err != nil {
		return 0, d.err
	}
	return 3, nil
}

func (d *fakeDeleter) calls() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.cutoffs...)
}

// MIR-9: retention runs at startup and then hourly, deleting completed mirror
// sessions older than 14 days. MIR-11: it returns nil when the context ends,
// leaving no goroutine behind.
func TestRetention_MIR9_HourlyFourteenDays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDeleter{}
		log, _ := newLogger()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		begin := time.Now()
		go func() { done <- RunRetention(ctx, d, RetentionEvery, RetentionKeep, time.Now, log) }()

		synctest.Wait()
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		got := d.calls()
		if len(got) != 3 {
			t.Fatalf("%d calls by t=2h, want 3 (t=0, 1h, 2h)", len(got))
		}
		for i, c := range got {
			want := begin.Add(time.Duration(i)*time.Hour - 14*24*time.Hour)
			if !c.Equal(want) {
				t.Errorf("call %d cutoff = %v, want %v", i, c, want)
			}
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("RunRetention = %v, want nil", err)
		}
	})
}

// MIR-9: a failed delete is logged and retried on the next tick.
func TestRetention_MIR9_FailureLoggedAndRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDeleter{err: errors.New("db down")}
		log, logs := newLogger()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- RunRetention(ctx, d, time.Hour, RetentionKeep, time.Now, log) }()
		synctest.Wait()
		time.Sleep(time.Hour)
		synctest.Wait()
		if n := len(d.calls()); n != 2 {
			t.Fatalf("%d calls, want 2 (retried after a failure)", n)
		}
		if !strings.Contains(logs.String(), "db down") {
			t.Errorf("failure not logged; logs:\n%s", logs)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("RunRetention = %v, want nil", err)
		}
	})
}
