package live

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// LIVE-3 (with SES-4): RunTicker calls f once per interval, never before the
// first interval has passed, and returns nil once ctx is done. The server uses
// it to re-render open pages every 30 s, so relative times and the
// charging-to-stale status move without a write.
func TestRunTicker_LIVE3_CallsEveryIntervalUntilDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- RunTicker(ctx, 30*time.Second, func() { calls.Add(1) }) }()

		time.Sleep(30*time.Second - time.Nanosecond)
		synctest.Wait()
		if n := calls.Load(); n != 0 {
			t.Fatalf("LIVE-3: %d calls before 30 s, want 0", n)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if n := calls.Load(); n != 1 {
			t.Fatalf("LIVE-3: %d calls at 30 s, want 1", n)
		}
		time.Sleep(60 * time.Second)
		synctest.Wait()
		if n := calls.Load(); n != 3 {
			t.Fatalf("LIVE-3: %d calls at 90 s, want 3", n)
		}

		cancel()
		if err := <-done; err != nil {
			t.Errorf("RunTicker = %v, want nil", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := calls.Load(); n != 3 {
			t.Errorf("LIVE-3: %d calls after ctx was done, want still 3", n)
		}
	})
}
