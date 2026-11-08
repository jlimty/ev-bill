package mirror

import (
	"context"
	"log/slog"
	"time"
)

// Deleter is the store method the retention job uses.
type Deleter interface {
	DeleteCompletedMirrorBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// RunRetention deletes completed mirror sessions older than keep, once at
// startup and then every interval (MIR-9). A failed run is logged and retried
// on the next tick. It returns nil when ctx is done (MIR-11).
func RunRetention(ctx context.Context, st Deleter, every, keep time.Duration, now func() time.Time, l *slog.Logger) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := st.DeleteCompletedMirrorBefore(ctx, now().Add(-keep))
		switch {
		case err != nil && ctx.Err() == nil:
			l.Warn("mirror: retention failed", "error", err)
		case err == nil && n > 0:
			l.Info("mirror: retention deleted completed sessions", "count", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
