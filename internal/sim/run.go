package sim

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Run plays every plan against the API, one goroutine per plan. Each plan
// starts a session with its first point, posts a reading for each middle
// point and stops with the last, sleeping the gap between points divided by
// speed. Every plan is validated first, so nothing is sent for a bad plan.
// A plan's loop stops at its first error; errors are joined. When ctx is
// cancelled, every loop returns promptly.
func Run(ctx context.Context, c *Client, plans []Plan, speed int) error {
	now := time.Now()
	for _, p := range plans {
		if err := Validate(p, speed, now); err != nil {
			return err
		}
	}

	errs := make([]error, len(plans))
	var wg sync.WaitGroup
	for i, p := range plans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runPlan(ctx, c, p, speed); err != nil {
				errs[i] = fmt.Errorf("%s: %w", p.ChargerID, err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// stopTimeout bounds the best-effort stop after a failed plan.
const stopTimeout = 10 * time.Second

// runPlan plays one plan. If it fails after the session started, it tries to
// stop the session at the last point the server accepted, so the charger is
// not left with an active session (SES-3). A failure of that attempt is logged
// and the plan's original error is returned.
func runPlan(ctx context.Context, c *Client, p Plan, speed int) error {
	pts := Points(p)
	last := len(pts) - 1
	var id string
	accepted := -1 // index of the last point the server accepted
	err := func() error {
		for i, pt := range pts {
			if i > 0 {
				if err := sleep(ctx, pt.At.Sub(pts[i-1].At)/time.Duration(speed)); err != nil {
					return err
				}
			}
			var err error
			switch i {
			case 0:
				id, err = c.Start(ctx, p.ChargerID, pt.At, pt.MeterWh)
			case last:
				err = c.Stop(ctx, id, pt.At, pt.MeterWh)
			default:
				err = c.MeterValue(ctx, id, pt.At, pt.MeterWh)
			}
			if err != nil {
				return err
			}
			accepted = i
		}
		return nil
	}()
	if err == nil || accepted < 0 {
		return err
	}
	// ctx may be the reason for the failure, so the stop gets its own deadline.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	pt := pts[accepted]
	if stopErr := c.Stop(stopCtx, id, pt.At, pt.MeterWh); stopErr != nil {
		c.logger().Error("stop after failed plan", "charger_id", p.ChargerID, "session_id", id, "error", stopErr)
	}
	return err
}

// sleep waits d, returning ctx's error early if ctx is done first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
