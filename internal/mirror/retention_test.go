package mirror

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type fakeDeleter struct {
	mu            sync.Mutex
	cutoffs       []time.Time
	sampleCutoffs []time.Time
	err           error // returned by DeleteCompletedMirrorBefore
	sampleErr     error // returned by DeleteSamplesBefore
}

func (d *fakeDeleter) DeleteCompletedMirrorBefore(_ context.Context, cutoff time.Time) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cutoffs = append(d.cutoffs, cutoff)
	if d.err != nil {
		return 0, d.err
	}
	return 3, nil
}

func (d *fakeDeleter) DeleteSamplesBefore(_ context.Context, cutoff time.Time) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sampleCutoffs = append(d.sampleCutoffs, cutoff)
	if d.sampleErr != nil {
		return 0, d.sampleErr
	}
	return 5, nil
}

func (d *fakeDeleter) calls() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.cutoffs...)
}

func (d *fakeDeleter) sampleCalls() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.sampleCutoffs...)
}

// MIR-9: retention runs at startup and then hourly, deleting completed mirror
// sessions older than 14 days. MIR-11: it returns nil when the context ends,
// leaving no goroutine behind.
func TestRetention_MIR9_HourlyFourteenDays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDeleter{}
		log, _ := newLogger()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		begin := time.Now()
		go func() { done <- RunRetention(ctx, d, RetentionEvery, RetentionKeep, time.Now, log) }()

		synctest.Wait()
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		got := d.calls()
		if len(got) != 3 {
			t.Fatalf("%d calls by t=2h, want 3 (t=0, 1h, 2h)", len(got))
		}
		for i, c := range got {
			want := begin.Add(time.Duration(i)*time.Hour - 14*24*time.Hour)
			if !c.Equal(want) {
				t.Errorf("call %d cutoff = %v, want %v", i, c, want)
			}
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("RunRetention = %v, want nil", err)
		}
	})
}

// MIR-9: a failed delete is logged and retried on the next tick.
func TestRetention_MIR9_FailureLoggedAndRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDeleter{err: errors.New("db down")}
		log, logs := newLogger()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- RunRetention(ctx, d, time.Hour, RetentionKeep, time.Now, log) }()
		synctest.Wait()
		time.Sleep(time.Hour)
		synctest.Wait()
		if n := len(d.calls()); n != 2 {
			t.Fatalf("%d calls, want 2 (retried after a failure)", n)
		}
		if !strings.Contains(logs.String(), "db down") {
			t.Errorf("failure not logged; logs:\n%s", logs)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("RunRetention = %v, want nil", err)
		}
	})
}

// LOC-1, MIR-9: the same job deletes occupancy samples older than 14 days, at
// startup and then hourly, with the same cutoff as the sessions, and logs how
// many it deleted.
func TestRetention_LOC1_DeletesOldSamples(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDeleter{}
		log, logs := newLogger()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel() // a failed check still stops RunRetention
		done := make(chan error, 1)
		begin := time.Now()
		go func() { done <- RunRetention(ctx, d, RetentionEvery, RetentionKeep, time.Now, log) }()

		synctest.Wait()
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		got := d.sampleCalls()
		if len(got) != 3 {
			t.Fatalf("%d sample deletes by t=2h, want 3 (t=0, 1h, 2h)", len(got))
		}
		sessionCutoffs := d.calls()
		for i, c := range got {
			want := begin.Add(time.Duration(i)*time.Hour - 14*24*time.Hour)
			if !c.Equal(want) || !c.Equal(sessionCutoffs[i]) {
				t.Errorf("sample delete %d cutoff = %v, want %v (the sessions' cutoff)", i, c, want)
			}
		}
		if !strings.Contains(logs.String(), msgSamplesDeleted) || !strings.Contains(logs.String(), `"count":5`) {
			t.Errorf("sample count not logged; logs:\n%s", logs)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("RunRetention = %v, want nil", err)
		}
	})
}

// LOC-1, MIR-9: a failure of one delete is logged and does not stop the other.
func TestRetention_LOC1_OneFailureDoesNotStopTheOther(t *testing.T) {
	for _, tc := range []struct {
		name               string
		sessErr, sampleErr error
	}{
		{"sessions fail", errors.New("sessions down"), nil},
		{"samples fail", nil, errors.New("samples down")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := &fakeDeleter{err: tc.sessErr, sampleErr: tc.sampleErr}
				log, logs := newLogger()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel() // a failed check still stops RunRetention
				done := make(chan error, 1)
				go func() { done <- RunRetention(ctx, d, time.Hour, RetentionKeep, time.Now, log) }()
				synctest.Wait()
				if len(d.calls()) != 1 || len(d.sampleCalls()) != 1 {
					t.Fatalf("calls = %d sessions, %d samples; want 1 each", len(d.calls()), len(d.sampleCalls()))
				}
				out := logs.String()
				for _, err := range []error{tc.sessErr, tc.sampleErr} {
					if err != nil && !strings.Contains(out, err.Error()) {
						t.Errorf("failure %q not logged; logs:\n%s", err, out)
					}
				}
				// The delete that succeeded still logs its count.
				wantMsg := msgSamplesDeleted
				if tc.sessErr == nil {
					wantMsg = msgSessionsDeleted
				}
				if !strings.Contains(out, wantMsg) {
					t.Errorf("successful delete not logged (%q); logs:\n%s", wantMsg, out)
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatalf("RunRetention = %v, want nil", err)
				}
			})
		})
	}
}
