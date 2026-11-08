package live

import (
	"context"
	"sync"
	"sync/atomic"
)

// Hub fans a "sessions changed" signal out to subscribers (LIVE-2). One
// broadcaster goroutine, Run, does the fan-out. Every channel send is
// non-blocking, so a slow subscriber can never hold up the publisher.
//
// Concurrency: subs and stopped are guarded by mu. A subscriber channel is
// sent to and closed only while mu is held, and is closed only by stop, which
// also removes it from subs, so no send can reach a closed channel and no
// channel is closed twice.
type Hub struct {
	maxSubs int
	in      chan struct{} // capacity 1: Publish coalesces into one waiting signal
	gen     atomic.Uint64 // broadcasts so far; see Gen

	mu      sync.Mutex
	subs    map[chan struct{}]struct{}
	stopped bool
}

// NewHub returns a hub that allows at most maxSubs subscribers at a time
// (LIVE-1 caps open streams at 100).
func NewHub(maxSubs int) *Hub {
	return &Hub{
		maxSubs: maxSubs,
		in:      make(chan struct{}, 1),
		subs:    make(map[chan struct{}]struct{}),
	}
}

// Run is the broadcaster. It forwards each published signal to every
// subscriber until ctx is done, then closes every subscriber channel, refuses
// new subscribers, and returns nil (MIR-11). Call it once.
func (h *Hub) Run(ctx context.Context) error {
	defer h.stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.in:
			h.broadcast()
		}
	}
}

// Publish signals that sessions changed. It never blocks: if a signal is
// already waiting for the broadcaster, this one is dropped, since the waiting
// one already says "something changed".
func (h *Hub) Publish() {
	select {
	case h.in <- struct{}{}:
	default:
	}
}

// Subscribe registers a subscriber. ch receives a signal after each change;
// it has a buffer of 1 that coalesces signals, so a reader that falls behind
// sees one signal, not a backlog. ch is closed when the hub stops. cancel
// unregisters the subscriber; it is safe to call more than once and after the
// hub stopped. ok is false, with a nil ch and a no-op cancel, when the hub is
// at its cap or has stopped.
func (h *Hub) Subscribe() (ch <-chan struct{}, cancel func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped || len(h.subs) >= h.maxSubs {
		return nil, func() {}, false
	}
	c := make(chan struct{}, 1)
	h.subs[c] = struct{}{}
	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		// Deleting is idempotent, and after stop c is no longer in subs.
		delete(h.subs, c)
	}
	return c, cancel, true
}

// Gen returns the hub's generation: the number of broadcasts so far. It goes
// up before each broadcast's signals are sent, so a subscriber that reads Gen
// after receiving a signal sees that broadcast's generation or a later one.
// Streams key their shared renders by it: one render per view per generation
// serves every stream on that view (LIVE-1, LIVE-2).
func (h *Hub) Gen() uint64 { return h.gen.Load() }

func (h *Hub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen.Add(1)
	for c := range h.subs {
		select {
		case c <- struct{}{}:
		default: // a signal is already waiting for this subscriber
		}
	}
}

func (h *Hub) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	for c := range h.subs {
		close(c)
		delete(h.subs, c)
	}
}
