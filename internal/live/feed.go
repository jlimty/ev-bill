// Package live shares the live mirror's state with the web pages.
package live

import (
	"sync/atomic"
	"time"
)

// FeedState is the health of the DataMall feed as the page shows it. LastGood
// is the feed timestamp of the last successful cycle; LastError is the last
// failure's (already redacted) message, cleared by the next success.
type FeedState struct {
	Enabled   bool
	LastGood  time.Time
	LastError string
}

// Feed holds the current FeedState. The poller stores it and page handlers
// load it concurrently, so it sits behind an atomic pointer. The zero Feed is
// ready to use.
type Feed struct {
	state atomic.Pointer[FeedState]
}

// Load returns the current state, or the zero FeedState if none was stored.
func (f *Feed) Load() FeedState {
	if p := f.state.Load(); p != nil {
		return *p
	}
	return FeedState{}
}

// Store replaces the current state.
func (f *Feed) Store(s FeedState) { f.state.Store(&s) }
