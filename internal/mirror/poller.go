package mirror

import (
	"context"
	"log/slog"
	"time"

	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/live"
)

// Fetcher fetches one snapshot of the watched postal codes. *datamall.Client
// implements it.
type Fetcher interface {
	Fetch(ctx context.Context, postalCodes map[string]bool) (datamall.Snapshot, error)
}

// Poller fetches the feed at startup and every Interval, reconciles each
// snapshot and records the feed's health in Feed (MIR-1). OnChange, if set,
// runs after a cycle that changed something (LIVE-2).
type Poller struct {
	Fetcher     Fetcher
	Reconciler  *Reconciler
	PostalCodes map[string]bool
	Interval    time.Duration
	Feed        *live.Feed
	OnChange    func()
	Logger      *slog.Logger
}

// Run polls until ctx is done, then returns nil (MIR-11). A failed cycle is
// logged and recorded, and the next tick retries; it never returns an error.
func (p *Poller) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		// select picks at random when a tick and cancellation are both ready,
		// so check first rather than run one more cycle during shutdown.
		if ctx.Err() != nil {
			return nil
		}
		p.cycle(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// cycle runs one fetch and reconcile.
func (p *Poller) cycle(ctx context.Context) {
	snap, err := p.Fetcher.Fetch(ctx, p.PostalCodes)
	if err == nil {
		var changed bool
		changed, err = p.Reconciler.Apply(ctx, snap)
		if err == nil {
			st := p.Feed.Load()
			st.Enabled, st.LastError = true, ""
			if snap.UpdatedAt.After(st.LastGood) {
				st.LastGood = snap.UpdatedAt
			}
			p.Feed.Store(st)
			if changed && p.OnChange != nil {
				p.OnChange()
			}
			return
		}
	}
	if ctx.Err() != nil {
		return // shutting down: not a feed failure
	}
	// A failed fetch, a failed reconcile and a snapshot skipped as
	// ErrFutureSnapshot all land here: LastError is set and LastGood is left
	// alone. Fetch errors are already redacted by package datamall: no
	// AccountKey, no presigned query string.
	p.Logger.Warn(msgPollFailed, "error", err)
	st := p.Feed.Load()
	st.Enabled, st.LastError = true, err.Error()
	p.Feed.Store(st)
}
