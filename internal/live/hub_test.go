package live

import (
	"context"
	"testing"
	"testing/synctest"
)

// pending reports whether ch holds a signal, without blocking. It fails the
// test if ch is closed.
func pending(t *testing.T, ch <-chan struct{}) bool {
	t.Helper()
	select {
	case _, open := <-ch:
		if !open {
			t.Fatal("channel is closed, want open")
		}
		return true
	default:
		return false
	}
}

// startHub runs h in the bubble and returns a stop function that cancels it
// and returns Run's result.
func startHub(t *testing.T, h *Hub) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	return func() error {
		cancel()
		return <-done
	}
}

// LIVE-2: one Publish reaches every subscriber once.
func TestHub_LIVE2_FanOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub(100)
		var subs []<-chan struct{}
		for range 3 {
			ch, cancel, ok := h.Subscribe()
			if !ok {
				t.Fatal("LIVE-2: Subscribe refused below the cap")
			}
			defer cancel()
			subs = append(subs, ch)
		}
		stop := startHub(t, h)

		h.Publish()
		synctest.Wait()
		for i, ch := range subs {
			if !pending(t, ch) {
				t.Errorf("LIVE-2: subscriber %d got no signal", i)
			}
			if pending(t, ch) {
				t.Errorf("LIVE-2: subscriber %d got more than one signal", i)
			}
		}
		if err := stop(); err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	})
}

// LIVE-2: a subscriber that never reads cannot block Publish or the other
// subscribers; its buffer of 1 coalesces every signal into exactly one
// pending signal. synctest.Wait after each Publish lets the broadcaster
// handle every signal, so 1,000 broadcasts really happen. If Publish or the
// broadcaster blocked, the bubble would deadlock and the test would fail.
func TestHub_LIVE2_SlowSubscriberNeverBlocksPublish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub(100)
		slow, cancelSlow, ok := h.Subscribe()
		if !ok {
			t.Fatal("LIVE-2: Subscribe refused")
		}
		defer cancelSlow()
		fast, cancelFast, ok := h.Subscribe()
		if !ok {
			t.Fatal("LIVE-2: Subscribe refused")
		}
		defer cancelFast()
		stop := startHub(t, h)

		for i := range 1000 {
			h.Publish()
			synctest.Wait()
			if !pending(t, fast) {
				t.Fatalf("LIVE-2: publish %d did not reach the reading subscriber", i+1)
			}
		}
		if n := len(slow); n != 1 {
			t.Errorf("LIVE-2: slow subscriber has %d pending signals, want exactly 1", n)
		}
		if err := stop(); err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	})
}

// LIVE-2: Publish never blocks, even before Run starts or after it stopped.
func TestHub_LIVE2_PublishWithoutRunNeverBlocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub(1)
		for range 10 {
			h.Publish()
		}
		stop := startHub(t, h)
		if err := stop(); err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
		for range 10 {
			h.Publish()
		}
	})
}

// LIVE-1: at most 100 subscribers; cancelling one frees its slot, and
// cancelling twice frees only one.
func TestHub_LIVE1_CapAt100(t *testing.T) {
	h := NewHub(100)
	cancels := make([]func(), 0, 100)
	for i := range 100 {
		_, cancel, ok := h.Subscribe()
		if !ok {
			t.Fatalf("LIVE-1: Subscribe %d refused below the cap", i+1)
		}
		cancels = append(cancels, cancel)
	}
	if _, _, ok := h.Subscribe(); ok {
		t.Fatal("LIVE-1: 101st Subscribe succeeded, want ok=false")
	}

	cancels[0]()
	cancels[0]() // a second cancel must not free another slot
	if _, _, ok := h.Subscribe(); !ok {
		t.Fatal("LIVE-1: Subscribe refused after a cancel freed a slot")
	}
	if _, _, ok := h.Subscribe(); ok {
		t.Fatal("LIVE-1: a double cancel freed two slots")
	}
}

// MIR-11: when Run's context is cancelled, every subscriber channel is closed,
// Run returns nil, and the hub refuses new subscribers. Cancelling after stop
// is safe. synctest fails the test if any goroutine is left behind.
func TestHub_MIR11_RunClosesSubscribersOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub(100)
		var subs []<-chan struct{}
		var cancels []func()
		for range 3 {
			ch, cancel, ok := h.Subscribe()
			if !ok {
				t.Fatal("Subscribe refused")
			}
			subs = append(subs, ch)
			cancels = append(cancels, cancel)
		}
		stop := startHub(t, h)
		h.Publish() // a pending signal must not hide the close
		synctest.Wait()

		if err := stop(); err != nil {
			t.Errorf("MIR-11: Run = %v, want nil", err)
		}
		for i, ch := range subs {
			for range ch { // drain any pending signal; ends only when closed
			}
			if _, open := <-ch; open {
				t.Errorf("MIR-11: subscriber %d channel still open", i)
			}
		}
		if _, _, ok := h.Subscribe(); ok {
			t.Error("MIR-11: Subscribe after stop returned ok=true")
		}
		for _, cancel := range cancels {
			cancel()
			cancel()
		}
		h.Publish()
	})
}

// MIR-11: Run returns at once when its context is already cancelled.
func TestHub_MIR11_RunWithCancelledContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHub(1)
		ch, cancel, ok := h.Subscribe()
		if !ok {
			t.Fatal("Subscribe refused")
		}
		defer cancel()
		ctx, cancelCtx := context.WithCancel(t.Context())
		cancelCtx()
		if err := h.Run(ctx); err != nil {
			t.Errorf("MIR-11: Run = %v, want nil", err)
		}
		if _, open := <-ch; open {
			t.Error("MIR-11: subscriber channel still open after Run returned")
		}
	})
}
