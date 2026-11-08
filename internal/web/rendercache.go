package web

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// maxConcurrentRenders bounds the database work behind all open streams
// together. The pool has 10 connections (maxConns in cmd/server), and
// /healthz and the reconciler must always find one free (LIVE-1).
const maxConcurrentRenders = 2

// fragment is one rendered view. final is true for a completed session: the
// stream sends it, then a done event, and ends (LIVE-1).
type fragment struct {
	html  []byte
	final bool
}

// fragmentFunc renders the fragment one stream sends.
type fragmentFunc func(context.Context) (fragment, error)

// renderCache shares renders between the streams on one view (LIVE-1,
// LIVE-2). A view is a key: "list", or "session:<id>". Every stream on a view
// that asks for the same hub generation gets the bytes of one render, so the
// cost of a hub signal grows with the number of views, not of viewers.
//
// It keeps one render per view, the latest, and forgets a view when its last
// stream ends, so it never holds more entries than there are open streams.
// At most maxConcurrentRenders renders run at once. The zero value is ready
// to use.
type renderCache struct {
	mu    sync.Mutex
	views map[string]*cachedView
	slots chan struct{} // a render holds one slot while it runs

	renders atomic.Int64 // renders started; tests and measurements read it
}

// cachedView is one view's streams and its latest render.
type cachedView struct {
	streams int
	last    *renderCall
}

// renderCall is one render, shared by every stream that wants it. done is
// closed when the render ends; at, frag and err are written before that and
// read only after it.
type renderCall struct {
	gen  uint64
	done chan struct{}
	at   time.Time
	frag fragment
	err  error
}

// hold registers a stream on key. The stream calls release when it ends; the
// last release forgets the view and its render.
func (c *renderCache) hold(key string) (release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.views == nil {
		c.views = make(map[string]*cachedView)
	}
	v := c.views[key]
	if v == nil {
		v = &cachedView{}
		c.views[key] = v
	}
	v.streams++
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		v.streams--
		if v.streams == 0 && c.views[key] == v {
			delete(c.views, key)
		}
	}
}

// get returns key's fragment for hub generation gen. It shares a render of
// gen or later that is still running, or that finished without error and,
// when maxAge is above zero, at most maxAge ago. Otherwise it renders, within
// renderTimeout and a render slot. The render does not stop when ctx does, so
// one stream leaving cannot fail the others waiting on its render; ctx ends
// only this caller's wait.
func (c *renderCache) get(ctx context.Context, key string, gen uint64, maxAge time.Duration, render fragmentFunc) (fragment, error) {
	c.mu.Lock()
	if c.slots == nil {
		c.slots = make(chan struct{}, maxConcurrentRenders)
	}
	slots := c.slots
	v := c.views[key]
	if v == nil {
		v = &cachedView{} // not held: render, and share with no one
	}
	call := v.last
	if !reusable(call, gen, maxAge) {
		if call != nil && call.gen > gen {
			gen = call.gen // a fresh render covers every generation so far
		}
		call = &renderCall{gen: gen, done: make(chan struct{})}
		v.last = call
		c.mu.Unlock()
		c.run(ctx, call, slots, render)
	} else {
		c.mu.Unlock()
	}
	select {
	case <-call.done:
		return call.frag, call.err
	case <-ctx.Done():
		return fragment{}, ctx.Err()
	}
}

// reusable reports whether call can serve generation gen: it is for gen or
// later, and is still running or finished without error, within maxAge when
// maxAge is above zero. A failed render is never reused, so the next signal
// retries it.
func reusable(call *renderCall, gen uint64, maxAge time.Duration) bool {
	if call == nil || call.gen < gen {
		return false
	}
	select {
	case <-call.done:
		return call.err == nil && (maxAge <= 0 || time.Since(call.at) <= maxAge)
	default:
		return true // still running, so as fresh as a new one
	}
}

// run renders call and closes call.done. Waiting for a slot and the render
// share one renderTimeout.
func (c *renderCache) run(ctx context.Context, call *renderCall, slots chan struct{}, render fragmentFunc) {
	defer close(call.done)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renderTimeout)
	defer cancel()
	select {
	case slots <- struct{}{}:
	case <-rctx.Done():
		call.err = fmt.Errorf("wait for a render slot: %w", rctx.Err())
		return
	}
	defer func() { <-slots }()
	c.renders.Add(1)
	call.frag, call.err = render(rctx)
	call.at = time.Now()
}
