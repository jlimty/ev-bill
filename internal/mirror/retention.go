package mirror

import (
	"context"
	"log/slog"
	"time"
)

// Log messages, named so tests can find them.
const (
	msgSessionsDeleted = "mirror: retention deleted completed sessions"
	msgSamplesDeleted  = "mirror: retention deleted occupancy samples"
)

// Deleter is the store methods the retention job uses.
type Deleter interface {
	DeleteCompletedMirrorBefore(ctx context.Context, cutoff time.Time) (int64, error)
	DeleteSamplesBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// RunRetention deletes completed mirror sessions (MIR-9) and occupancy samples
// (LOC-1) older than keep, once at startup and then every interval. Both use
// the same cutoff. A failed delete is logged, does not stop the other one, and
// is retried on the next tick. It returns nil when ctx is done (MIR-11).
func RunRetention(ctx context.Context, st Deleter, every, keep time.Duration, now func() time.Time, l *slog.Logger) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		cutoff := now().Add(-keep)
		n, err := st.DeleteCompletedMirrorBefore(ctx, cutoff)
		logRetention(ctx, l, "mirror: retention failed", msgSessionsDeleted, n, err)
		n, err = st.DeleteSamplesBefore(ctx, cutoff)
		logRetention(ctx, l, "mirror: sample retention failed", msgSamplesDeleted, n, err)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// logRetention logs a failed delete as a warning, unless ctx is done, and a
// successful one that deleted rows at Info.
func logRetention(ctx context.Context, l *slog.Logger, failMsg, okMsg string, n int64, err error) {
	switch {
	case err != nil && ctx.Err() == nil:
		l.Warn(failMsg, "error", err)
	case err == nil && n > 0:
		l.Info(okMsg, "count", n)
	}
}
