package live

import (
	"context"
	"time"
)

// RunTicker calls f every interval until ctx is done, then returns nil. The
// first call comes one interval after the start. The server runs it with
// Hub.Publish so open pages re-render on a schedule as well as on writes,
// which keeps relative times and the charging-to-stale status (SES-4) current.
func RunTicker(ctx context.Context, every time.Duration, f func()) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			f()
		}
	}
}
