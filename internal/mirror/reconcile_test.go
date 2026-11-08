package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/testdb"
)

// r0 is the first observation in the reconciler tests: a feed timestamp read
// in Asia/Singapore, as the DataMall client produces it (MIR-3).
var r0 = time.Date(2026, 11, 8, 9, 0, 0, 0, billing.SingaporeTariff().Location)

func mins(n int) time.Duration { return time.Duration(n) * time.Minute }

// syncBuffer is a log sink that is safe to share between goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(buf, nil)), buf
}

// ac is a watched 7.4 kW AC connector with full details.
func ac(id string, st datamall.Status) datamall.Connector {
	return datamall.Connector{
		ChargerID: id, LocationName: "Test Mall", Address: "1 Test Road", PostalCode: "100001",
		Operator: "Testco", Position: "B1", PlugType: "Type 2", Current: "AC", PowerW: 7_400,
		Price: "0.5000", Status: st,
	}
}

func snapAt(at time.Time, conns ...datamall.Connector) datamall.Snapshot {
	return datamall.Snapshot{UpdatedAt: at, Connectors: conns}
}

// harness drives a Reconciler against a real test database with a clock the
// test sets.
type harness struct {
	t     *testing.T
	pool  *pgxpool.Pool
	st    *store.Store
	clock time.Time
	logs  *syncBuffer
	log   *slog.Logger
	rec   *Reconciler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := testdb.New(t)
	h := &harness{t: t, pool: pool, st: store.New(pool)}
	h.log, h.logs = newLogger()
	h.rec = h.reconciler(h.st)
	return h
}

func (h *harness) reconciler(s MirrorStore) *Reconciler {
	return &Reconciler{Store: s, Now: func() time.Time { return h.clock }, Logger: h.log, GapAfter: GapAfter}
}

// apply sets the clock to the observation time and applies the snapshot.
func (h *harness) apply(at time.Time, conns ...datamall.Connector) bool {
	h.t.Helper()
	h.clock = at
	changed, err := h.rec.Apply(context.Background(), snapAt(at, conns...))
	if err != nil {
		h.t.Fatalf("Apply at %v: %v", at, err)
	}
	return changed
}

// sessionsOf returns a charger's sessions, oldest first.
func (h *harness) sessionsOf(charger string) []store.SessionWithReadings {
	h.t.Helper()
	all, err := h.st.ListRecent(context.Background(), sessions.ListLimit)
	if err != nil {
		h.t.Fatalf("ListRecent: %v", err)
	}
	var out []store.SessionWithReadings
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Session.ChargerID == charger {
			out = append(out, all[i])
		}
	}
	return out
}

// state is a readable dump of every session and charger row, for comparing
// two runs.
func (h *harness) state() string {
	h.t.Helper()
	all, err := h.st.ListRecent(context.Background(), sessions.ListLimit)
	if err != nil {
		h.t.Fatalf("ListRecent: %v", err)
	}
	var lines []string
	for _, sw := range all {
		s := sw.Session
		stop := "open"
		if s.StoppedAt != nil {
			stop = fmt.Sprintf("%s@%d", s.StoppedAt.UTC().Format(time.RFC3339), *s.MeterStopWh)
		}
		lines = append(lines, fmt.Sprintf("session %s %s start %s@%d stop %s readings %v",
			s.ChargerID, s.Source, s.StartedAt.UTC().Format(time.RFC3339), s.MeterStartWh, stop, sw.Readings))
	}
	rows, err := h.st.ChargerStates(context.Background())
	if err != nil {
		h.t.Fatalf("ChargerStates: %v", err)
	}
	for id, r := range rows {
		lines = append(lines, fmt.Sprintf("charger %s %v held=%v observed %s", id, r.Status, r.Held, r.ObservedAt.UTC().Format(time.RFC3339)))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func requireStopped(t *testing.T, sw store.SessionWithReadings, at time.Time, wh int64) {
	t.Helper()
	s := sw.Session
	if s.StoppedAt == nil || !s.StoppedAt.Equal(at) || *s.MeterStopWh != wh {
		t.Fatalf("session %s stop = %v/%v, want %v @ %d Wh", s.ID, s.StoppedAt, s.MeterStopWh, at, wh)
	}
}

// MIR-6: occupied opens a mirror session, occupied again adds a reading,
// available stops it. MIR-4: 7,400 W for 300 s is 616 Wh, truncated per step.
func TestReconcile_MIR6_OccupancyLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if !h.apply(r0, ac("SG-1", datamall.Occupied)) {
		t.Error("first apply: changed = false, want true")
	}
	ss := h.sessionsOf("SG-1")
	if len(ss) != 1 {
		t.Fatalf("after start: %d sessions, want 1", len(ss))
	}
	s := ss[0].Session
	if s.Source != sessions.SourceMirror || s.MeterStartWh != 0 || !s.StartedAt.Equal(r0) || s.StoppedAt != nil {
		t.Fatalf("started session = %+v, want open mirror session at r0 with meter 0", s)
	}

	h.apply(r0.Add(mins(5)), ac("SG-1", datamall.Occupied))
	rs := h.sessionsOf("SG-1")[0].Readings
	if len(rs) != 1 || !rs[0].At.Equal(r0.Add(mins(5))) || rs[0].MeterWh != 616 {
		t.Fatalf("after 5 min: readings %+v, want one at r0+5m with 616 Wh", rs)
	}

	h.apply(r0.Add(mins(10)), ac("SG-1", datamall.Available))
	ss = h.sessionsOf("SG-1")
	if len(ss) != 1 {
		t.Fatalf("after stop: %d sessions, want 1", len(ss))
	}
	requireStopped(t, ss[0], r0.Add(mins(10)), 1_232)

	row, ok, err := h.st.Charger(context.Background(), "SG-1")
	if err != nil || !ok || row.Status != datamall.Available || !row.ObservedAt.Equal(r0.Add(mins(10))) {
		t.Errorf("charger row = %+v (found %v, err %v), want available observed at r0+10m", row, ok, err)
	}
}

// MIR-3: the same batch file twice is a no-op the second time, even when its
// content would otherwise cause actions.
func TestReconcile_MIR3_RepeatedFeedIsNoop(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.apply(r0, ac("SG-1", datamall.Occupied))
	before := h.state()

	if h.apply(r0, ac("SG-1", datamall.Occupied)) {
		t.Error("repeated feed: changed = true, want false")
	}
	if h.apply(r0, ac("SG-1", datamall.Available)) {
		t.Error("repeated timestamp with new content: changed = true, want false")
	}
	if after := h.state(); after != before {
		t.Errorf("state changed on a repeated feed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// MIR-3: after a restart (a new Reconciler, no memory) an older batch file
// must not stop a session before its last reading.
func TestReconcile_MIR3_OlderFeedAfterRestartIsNoop(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.apply(r0, ac("SG-1", datamall.Occupied))
	h.apply(r0.Add(mins(5)), ac("SG-1", datamall.Occupied))
	before := h.state()

	h.rec = h.reconciler(h.st) // restart
	if h.apply(r0, ac("SG-1", datamall.Available)) {
		t.Error("older feed: changed = true, want false")
	}
	if h.apply(r0.Add(mins(5)), ac("SG-1", datamall.Available)) {
		t.Error("equal feed: changed = true, want false")
	}
	if after := h.state(); after != before {
		t.Errorf("state changed on an older feed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// MIR-3: a timestamp more than 5 min ahead of the server clock is skipped and
// reported as ErrFutureSnapshot, so the poller does not count it as a good
// poll; exactly 5 min ahead is accepted (SES-2's skew).
func TestReconcile_MIR3_FutureFeedSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.clock = r0
	changed, err := h.rec.Apply(context.Background(), snapAt(r0.Add(sessions.MaxClockSkew+time.Second), ac("SG-1", datamall.Occupied)))
	if !errors.Is(err, ErrFutureSnapshot) || changed {
		t.Fatalf("future feed: Apply = (%v, %v), want (false, ErrFutureSnapshot)", changed, err)
	}
	if !strings.Contains(err.Error(), "future") {
		t.Errorf("error %q does not say the feed is in the future", err)
	}
	if _, ok, err := h.st.LatestObservation(context.Background()); err != nil || ok {
		t.Fatalf("future feed stored an observation (ok %v, err %v)", ok, err)
	}
	if len(h.sessionsOf("SG-1")) != 0 {
		t.Fatal("future feed opened a session")
	}

	changed, err = h.rec.Apply(context.Background(), snapAt(r0.Add(sessions.MaxClockSkew), ac("SG-1", datamall.Occupied)))
	if err != nil || !changed || len(h.sessionsOf("SG-1")) != 1 {
		t.Fatalf("feed exactly 5 min ahead: Apply = (%v, %v), want a session opened", changed, err)
	}
}

// MIR-5: after a 3 h gap the old session closes at its last point and a new
// one opens at this observation, carrying the meter (same occupancy).
func TestReconcile_MIR5_GapAfterSleep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.apply(r0, ac("SG-1", datamall.Occupied))
	h.apply(r0.Add(mins(5)), ac("SG-1", datamall.Occupied))
	h.apply(r0.Add(3*time.Hour+mins(5)), ac("SG-1", datamall.Occupied))

	ss := h.sessionsOf("SG-1")
	if len(ss) != 2 {
		t.Fatalf("%d sessions, want 2", len(ss))
	}
	requireStopped(t, ss[0], r0.Add(mins(5)), 616)
	next := ss[1].Session
	if !next.StartedAt.Equal(r0.Add(3*time.Hour+mins(5))) || next.MeterStartWh != 616 || next.StoppedAt != nil {
		t.Fatalf("new session = %+v, want open at r0+3h05m with meter 616", next)
	}
}

// cutStore wraps the real store and fails the failAt-th session write, as if
// the process died part-way through a cycle.
type cutStore struct {
	*store.Store
	failAt, n int
}

var errCut = errors.New("cut off")

func (c *cutStore) cut() error {
	c.n++
	if c.n == c.failAt {
		return errCut
	}
	return nil
}

func (c *cutStore) CreateSession(ctx context.Context, as sessions.Source, id string, at time.Time, wh int64, now time.Time) (sessions.Session, error) {
	if err := c.cut(); err != nil {
		return sessions.Session{}, err
	}
	return c.Store.CreateSession(ctx, as, id, at, wh, now)
}

func (c *cutStore) AddReading(ctx context.Context, as sessions.Source, id string, r billing.Reading, now time.Time) (sessions.Session, []billing.Reading, bool, error) {
	if err := c.cut(); err != nil {
		return sessions.Session{}, nil, false, err
	}
	return c.Store.AddReading(ctx, as, id, r, now)
}

func (c *cutStore) StopSession(ctx context.Context, as sessions.Source, id string, r billing.Reading, now time.Time) (sessions.Session, []billing.Reading, bool, error) {
	if err := c.cut(); err != nil {
		return sessions.Session{}, nil, false, err
	}
	return c.Store.StopSession(ctx, as, id, r, now)
}

// MIR-6: a cycle cut off after its first action is completed by the next
// poll (the same batch file, since observed_at was not advanced), ending in
// the same state as a run that was never cut.
func TestReconcile_MIR6_CutCycleCompletesNextPoll(t *testing.T) {
	t.Parallel()
	poll1 := []datamall.Connector{ac("SG-A", datamall.Occupied), ac("SG-B", datamall.Occupied)}
	// Actions in ChargerID order: stop A, read B, start C.
	poll2 := []datamall.Connector{ac("SG-A", datamall.Available), ac("SG-B", datamall.Occupied), ac("SG-C", datamall.Occupied)}
	t2 := r0.Add(mins(5))

	uncut := newHarness(t)
	uncut.apply(r0, poll1...)
	uncut.apply(t2, poll2...)

	cut := newHarness(t)
	cut.apply(r0, poll1...)
	cut.clock = t2
	_, err := cut.reconciler(&cutStore{Store: cut.st, failAt: 2}).Apply(context.Background(), snapAt(t2, poll2...))
	if !errors.Is(err, errCut) {
		t.Fatalf("cut cycle: err = %v, want the cut error", err)
	}
	if latest, _, err := cut.st.LatestObservation(context.Background()); err != nil || !latest.Equal(r0) {
		t.Fatalf("cut cycle advanced observed_at to %v (err %v); it must stay at r0", latest, err)
	}
	if cut.state() == uncut.state() {
		t.Fatal("cut cycle already matches the uncut run; the cut did not happen")
	}

	cut.apply(t2, poll2...) // the next poll fetches the same file
	if got, want := cut.state(), uncut.state(); got != want {
		t.Errorf("after the retry:\n%s\nwant (uncut run):\n%s", got, want)
	}
}

// MIR-7: an API session already active on a watched charger is left alone,
// no mirror session opens, and the skip is logged once across polls.
func TestReconcile_MIR7_APISessionOnWatchedCharger(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	api, err := h.st.CreateSession(context.Background(), sessions.SourceAPI, "SG-1", r0.Add(-mins(10)), 0, r0)
	if err != nil {
		t.Fatal(err)
	}
	h.apply(r0, ac("SG-1", datamall.Occupied))
	h.apply(r0.Add(mins(5)), ac("SG-1", datamall.Occupied))

	ss := h.sessionsOf("SG-1")
	if len(ss) != 1 || ss[0].Session.ID != api.ID || ss[0].Session.StoppedAt != nil || len(ss[0].Readings) != 0 {
		t.Fatalf("sessions = %+v, want only the untouched API session", ss)
	}
	if n := strings.Count(h.logs.String(), msgAPISession); n != 1 {
		t.Errorf("logged %q %d times, want once:\n%s", msgAPISession, n, h.logs)
	}
}

// MIR-12: an occupancy past 48 h stops at exactly started_at + 48 h with the
// meter interpolated, and no new session opens until the connector frees.
func TestReconcile_MIR12_LongOccupancy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := ac("SG-1", datamall.Occupied)
	c.PowerW = 1_000
	// Polls every 7 min: the last one before 48 h is at 411 × 7 = 2,877 min,
	// the next at 2,884 min (48 h 04 min) is past the limit.
	for k := 0; k <= 412; k++ {
		h.apply(r0.Add(mins(7*k)), c)
	}
	ss := h.sessionsOf("SG-1")
	if len(ss) != 1 {
		t.Fatalf("%d sessions, want 1", len(ss))
	}
	// 1,000 W × 420 s = 116 Wh per step (truncated) for 411 steps, then
	// 1,000 W × 180 s = 50 Wh to reach 48 h: 47,676 + 50.
	requireStopped(t, ss[0], r0.Add(sessions.MaxDuration), 47_726)
	if n, err := h.st.HeldCount(context.Background()); err != nil || n != 1 {
		t.Fatalf("HeldCount = %d, %v; want 1 while held", n, err)
	}

	h.apply(r0.Add(mins(7*413)), c) // 48 h 11 min, still occupied
	if n := len(h.sessionsOf("SG-1")); n != 1 {
		t.Fatalf("held connector opened a session: %d sessions", n)
	}

	h.apply(r0.Add(mins(7*414)), ac("SG-1", datamall.Available))
	if n, err := h.st.HeldCount(context.Background()); err != nil || n != 0 {
		t.Fatalf("HeldCount = %d, %v; want 0 once the connector frees", n, err)
	}
	h.apply(r0.Add(mins(7*415)), c)
	ss = h.sessionsOf("SG-1")
	if len(ss) != 2 || ss[1].Session.MeterStartWh != 0 || ss[1].Session.StoppedAt != nil {
		t.Fatalf("after freeing: sessions %+v, want a new open session with meter 0", ss)
	}
}

// MIR-8, LIVE-2: changed is true when there were actions or any charger's
// details changed, and false for an identical newer feed.
func TestReconcile_MIR8_ChangedReportsDetailChanges(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if !h.apply(r0, ac("SG-1", datamall.Available)) {
		t.Error("new charger: changed = false, want true")
	}
	if h.apply(r0.Add(mins(5)), ac("SG-1", datamall.Available)) {
		t.Error("identical feed: changed = true, want false")
	}
	priced := ac("SG-1", datamall.Available)
	priced.Price = "0.6000"
	if !h.apply(r0.Add(mins(10)), priced) {
		t.Error("price change: changed = false, want true")
	}
	if !h.apply(r0.Add(mins(15)), ac("SG-1", datamall.Unavailable)) {
		t.Error("status change: changed = false, want true")
	}
}

// fakeStore is an in-memory MirrorStore for the error paths, the connector cap
// and the poller tests. It keeps charger rows so LatestObservation and
// ChargerStates behave like the real store.
type fakeStore struct {
	mu       sync.Mutex
	chargers map[string]store.ChargerRow
	errs     map[string]error
	starts   []string
	samples  []store.OccupancySample
	createFn func(chargerID string) error
}

func newFakeStore() *fakeStore {
	return &fakeStore{chargers: map[string]store.ChargerRow{}, errs: map[string]error{}}
}

func (f *fakeStore) LatestObservation(context.Context) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var latest time.Time
	for _, r := range f.chargers {
		if r.ObservedAt.After(latest) {
			latest = r.ObservedAt
		}
	}
	return latest, !latest.IsZero(), f.errs["LatestObservation"]
}

func (f *fakeStore) ChargerStates(context.Context) (map[string]store.ChargerRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]store.ChargerRow, len(f.chargers))
	for k, v := range f.chargers {
		out[k] = v
	}
	return out, f.errs["ChargerStates"]
}

func (f *fakeStore) OpenMirrorSessions(context.Context) ([]store.SessionWithReadings, error) {
	return nil, f.errs["OpenMirrorSessions"]
}

func (f *fakeStore) LastMirrorStops(context.Context) (map[string]store.MirrorStop, error) {
	return nil, f.errs["LastMirrorStops"]
}

func (f *fakeStore) CreateSession(_ context.Context, _ sessions.Source, chargerID string, at time.Time, wh int64, _ time.Time) (sessions.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createFn != nil {
		if err := f.createFn(chargerID); err != nil {
			return sessions.Session{}, err
		}
	}
	f.starts = append(f.starts, chargerID)
	return sessions.Session{ChargerID: chargerID, StartedAt: at, MeterStartWh: wh, Source: sessions.SourceMirror}, nil
}

func (f *fakeStore) AddReading(context.Context, sessions.Source, string, billing.Reading, time.Time) (sessions.Session, []billing.Reading, bool, error) {
	return sessions.Session{}, nil, false, f.errs["AddReading"]
}

func (f *fakeStore) StopSession(context.Context, sessions.Source, string, billing.Reading, time.Time) (sessions.Session, []billing.Reading, bool, error) {
	return sessions.Session{}, nil, false, f.errs["StopSession"]
}

func (f *fakeStore) ApplyObservation(_ context.Context, rows []store.ChargerRow, samples []store.OccupancySample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ApplyObservation"]; err != nil {
		return err
	}
	for _, r := range rows {
		f.chargers[r.ChargerID] = r
	}
	f.samples = append(f.samples, samples...)
	return nil
}

func (f *fakeStore) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

// MIR-6: a store failure at any step aborts the cycle and is returned with
// context, and observed_at is not advanced unless the upsert succeeded.
func TestReconcile_MIR6_StoreErrorsAbortCycle(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, step := range []string{"LatestObservation", "ChargerStates", "OpenMirrorSessions", "LastMirrorStops", "ApplyObservation"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			fs := newFakeStore()
			fs.errs[step] = boom
			log, _ := newLogger()
			r := &Reconciler{Store: fs, Now: func() time.Time { return r0 }, Logger: log, GapAfter: GapAfter}
			changed, err := r.Apply(context.Background(), snapAt(r0, ac("SG-1", datamall.Available)))
			if !errors.Is(err, boom) || changed {
				t.Fatalf("Apply = (%v, %v), want (false, boom)", changed, err)
			}
		})
	}

	// A failing start that is not an API session aborts too.
	fs := newFakeStore()
	fs.createFn = func(string) error { return boom }
	log, _ := newLogger()
	r := &Reconciler{Store: fs, Now: func() time.Time { return r0 }, Logger: log, GapAfter: GapAfter}
	if _, err := r.Apply(context.Background(), snapAt(r0, ac("SG-1", datamall.Occupied))); !errors.Is(err, boom) {
		t.Fatalf("failed start: err = %v, want boom", err)
	}
	if len(fs.chargers) != 0 {
		t.Error("charger rows were upserted after a failed action; observed_at must not advance")
	}
}

// MIR-6: read and stop failures from the store abort the cycle.
func TestReconcile_MIR6_ReadAndStopErrorsAbortCycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.apply(r0, ac("SG-1", datamall.Occupied))
	boom := errors.New("boom")
	for _, tc := range []struct {
		method string
		st     datamall.Status
	}{{"AddReading", datamall.Occupied}, {"StopSession", datamall.Available}} {
		w := &failingStore{Store: h.st, method: tc.method, err: boom}
		h.clock = r0.Add(mins(5))
		if _, err := h.reconciler(w).Apply(context.Background(), snapAt(h.clock, ac("SG-1", tc.st))); !errors.Is(err, boom) {
			t.Errorf("%s failure: err = %v, want boom", tc.method, err)
		}
	}
}

// failingStore fails one write method of the real store.
type failingStore struct {
	*store.Store
	method string
	err    error
}

func (f *failingStore) AddReading(ctx context.Context, as sessions.Source, id string, r billing.Reading, now time.Time) (sessions.Session, []billing.Reading, bool, error) {
	if f.method == "AddReading" {
		return sessions.Session{}, nil, false, f.err
	}
	return f.Store.AddReading(ctx, as, id, r, now)
}

func (f *failingStore) StopSession(ctx context.Context, as sessions.Source, id string, r billing.Reading, now time.Time) (sessions.Session, []billing.Reading, bool, error) {
	if f.method == "StopSession" {
		return sessions.Session{}, nil, false, f.err
	}
	return f.Store.StopSession(ctx, as, id, r, now)
}

// MIR-2: at most MaxConnectors connectors are mirrored; the rest are dropped
// with a warning. MIR-4: skipped connectors and charger IDs that SES-1 would
// refuse are logged once, not every poll.
func TestReconcile_MIR2_CapAndSkippedLoggedOnce(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	log, logs := newLogger()
	clock := r0
	r := &Reconciler{Store: fs, Now: func() time.Time { return clock }, Logger: log, GapAfter: GapAfter}

	conns := make([]datamall.Connector, 0, MaxConnectors+2)
	for i := range MaxConnectors + 1 {
		conns = append(conns, ac(fmt.Sprintf("SG-%03d", i), datamall.Available))
	}
	conns = append(conns, ac("bad id", datamall.Occupied))
	snap := datamall.Snapshot{UpdatedAt: r0, Connectors: conns, Skipped: []string{"SKIP-1"}}
	for i := range 2 {
		clock = r0.Add(mins(5 * i))
		snap.UpdatedAt = clock
		if _, err := r.Apply(context.Background(), snap); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	if len(fs.chargers) != MaxConnectors {
		t.Errorf("%d chargers stored, want %d", len(fs.chargers), MaxConnectors)
	}
	if fs.startCount() != 0 {
		t.Errorf("a session was started for an invalid charger ID")
	}
	out := logs.String()
	for _, msg := range []string{msgSkipped, msgBadID} {
		if n := strings.Count(out, msg); n != 1 {
			t.Errorf("logged %q %d times, want once", msg, n)
		}
	}
	if !strings.Contains(out, msgOverCap) {
		t.Errorf("no warning for connectors over the cap; logs:\n%s", out)
	}

	empty := datamall.Snapshot{UpdatedAt: r0.Add(mins(10))}
	clock = empty.UpdatedAt
	if _, err := r.Apply(context.Background(), empty); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), msgNoConnectors) {
		t.Error("no warning when the feed selects no connectors")
	}
}

// MIR-7: after an API session ends and the mirror starts on that charger, a
// later API session is logged again.
func TestReconcile_MIR7_APISkipLoggedAgainAfterMirrorStart(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	apiActive := true
	fs.createFn = func(string) error {
		if apiActive {
			return &sessions.RuleError{Kind: sessions.KindConflict, Code: "active_session_exists", Message: "x"}
		}
		return nil
	}
	log, logs := newLogger()
	clock := r0
	r := &Reconciler{Store: fs, Now: func() time.Time { return clock }, Logger: log, GapAfter: GapAfter}
	apply := func(k int) {
		t.Helper()
		clock = r0.Add(mins(5 * k))
		if _, err := r.Apply(context.Background(), snapAt(clock, ac("SG-1", datamall.Occupied))); err != nil {
			t.Fatal(err)
		}
	}
	apply(0)
	apply(1)
	apiActive = false
	apply(2) // the fake has no open sessions, so the mirror starts again each poll
	apiActive = true
	apply(3)
	if n := strings.Count(logs.String(), msgAPISession); n != 2 {
		t.Errorf("logged %q %d times, want twice", msgAPISession, n)
	}
}

// noUpsertStore is the real store whose ApplyObservation fails, as if the
// process died after the actions but before observed_at was written.
type noUpsertStore struct{ *store.Store }

func (noUpsertStore) ApplyObservation(context.Context, []store.ChargerRow, []store.OccupancySample) error {
	return errCut
}

// MIR-12: when a cycle is cut after the 48 h stop but before Held is stored,
// the next poll still holds the connector (from the durable stop row) and
// does not open a new session.
func TestReconcile_MIR12_HoldSurvivesCutCycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := ac("SG-1", datamall.Occupied)
	ctx := context.Background()
	late := r0.Add(sessions.MaxDuration - mins(5))
	sess, err := h.st.CreateSession(ctx, sessions.SourceMirror, "SG-1", r0, 0, r0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.st.AddReading(ctx, sessions.SourceMirror, sess.ID, billing.Reading{At: late, MeterWh: 50_000}, late); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertChargers(ctx, []store.ChargerRow{{Connector: c, ObservedAt: late}}); err != nil {
		t.Fatal(err)
	}

	h.clock = late.Add(mins(7))
	if _, err := h.reconciler(noUpsertStore{h.st}).Apply(ctx, snapAt(h.clock, c)); !errors.Is(err, errCut) {
		t.Fatalf("cut cycle: err = %v, want the cut error", err)
	}
	ss := h.sessionsOf("SG-1")
	if len(ss) != 1 || ss[0].Session.StoppedAt == nil || !ss[0].Session.StoppedAt.Equal(r0.Add(sessions.MaxDuration)) {
		t.Fatalf("after the cut cycle: %+v, want the session stopped at 48 h", ss)
	}

	h.apply(late.Add(mins(14)), c)
	if n := len(h.sessionsOf("SG-1")); n != 1 {
		t.Fatalf("next poll opened a new session (%d sessions); the hold was lost", n)
	}
	if n, err := h.st.HeldCount(ctx); err != nil || n != 1 {
		t.Fatalf("HeldCount = %d, %v; want 1", n, err)
	}
}

// MIR-7, MIR-4, MIR-12: a skip for an API session stores the charger as
// occupied, but that occupancy has nothing to do with the charger's last
// mirror stop days earlier. When the API session ends and the mirror starts,
// the old stop must neither hold the charger (a 48 h stop) nor carry its meter.
func TestReconcile_MIR7_StaleStopNotCarried(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		stopAt    time.Duration // after r0
		stopMeter int64
	}{
		{"old 48 h stop", sessions.MaxDuration, 40_000},
		{"old normal stop", 2 * time.Hour, 7_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			ctx := context.Background()
			stopAt := r0.Add(tc.stopAt)
			old, err := h.st.CreateSession(ctx, sessions.SourceMirror, "SG-1", r0, 0, r0)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := h.st.StopSession(ctx, sessions.SourceMirror, old.ID, billing.Reading{At: stopAt, MeterWh: tc.stopMeter}, stopAt); err != nil {
				t.Fatal(err)
			}
			h.apply(stopAt.Add(mins(5)), ac("SG-1", datamall.Available)) // held cleared

			later := r0.Add(5 * 24 * time.Hour)
			api, err := h.st.CreateSession(ctx, sessions.SourceAPI, "SG-1", later.Add(-mins(10)), 0, later)
			if err != nil {
				t.Fatal(err)
			}
			h.apply(later, ac("SG-1", datamall.Occupied)) // MIR-7 skip
			if _, _, _, err := h.st.StopSession(ctx, sessions.SourceAPI, api.ID, billing.Reading{At: later.Add(mins(2)), MeterWh: 100}, later.Add(mins(2))); err != nil {
				t.Fatal(err)
			}
			h.apply(later.Add(mins(5)), ac("SG-1", datamall.Occupied))

			var mirrorSessions []store.SessionWithReadings
			for _, sw := range h.sessionsOf("SG-1") {
				if sw.Session.Source == sessions.SourceMirror && sw.Session.ID != old.ID {
					mirrorSessions = append(mirrorSessions, sw)
				}
			}
			if len(mirrorSessions) != 1 || mirrorSessions[0].Session.MeterStartWh != 0 || mirrorSessions[0].Session.StoppedAt != nil {
				t.Fatalf("new mirror sessions = %+v, want one open session starting at meter 0", mirrorSessions)
			}
			if n, err := h.st.HeldCount(ctx); err != nil || n != 0 {
				t.Fatalf("HeldCount = %d, %v; want 0", n, err)
			}
		})
	}
}

// MIR-5, MIR-4: a gap cycle cut between its stop and its start is completed
// by the next poll, and the new session still carries the meter. The gap stop
// sits exactly at the charger's last stored observation, so it counts as part
// of the current occupancy.
func TestReconcile_MIR5_CutGapStillCarriesMeter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.apply(r0, ac("SG-1", datamall.Occupied))
	h.apply(r0.Add(mins(5)), ac("SG-1", datamall.Occupied))

	late := r0.Add(3*time.Hour + mins(5))
	h.clock = late
	if _, err := h.reconciler(&cutStore{Store: h.st, failAt: 2}).Apply(context.Background(), snapAt(late, ac("SG-1", datamall.Occupied))); !errors.Is(err, errCut) {
		t.Fatalf("cut cycle: err = %v, want the cut error", err)
	}
	h.apply(late, ac("SG-1", datamall.Occupied)) // the next poll fetches the same file

	ss := h.sessionsOf("SG-1")
	if len(ss) != 2 {
		t.Fatalf("%d sessions, want 2", len(ss))
	}
	requireStopped(t, ss[0], r0.Add(mins(5)), 616)
	if next := ss[1].Session; !next.StartedAt.Equal(late) || next.MeterStartWh != 616 {
		t.Fatalf("new session = %+v, want start at r0+3h05m carrying 616 Wh", next)
	}
}

// connAt is a watched connector at a postal code, with a given status.
func connAt(id, postal string, st datamall.Status) datamall.Connector {
	c := ac(id, st)
	c.PostalCode = postal
	return c
}

// LOC-1: one sample per postal code present, counting the connectors'
// statuses, sorted by postal code. A watched postal code with no connectors
// in the snapshot gets no sample, never a 0/0/0 row.
func TestSamples_LOC1(t *testing.T) {
	t.Parallel()
	conns := []datamall.Connector{
		connAt("SG-1", "100002", datamall.Occupied),
		connAt("SG-2", "100001", datamall.Available),
		connAt("SG-3", "100001", datamall.Occupied),
		connAt("SG-4", "100002", datamall.Unavailable),
		connAt("SG-5", "100001", datamall.Unavailable),
		connAt("SG-6", "100002", datamall.Occupied),
		connAt("SG-7", "100001", datamall.Available),
	}
	got := Samples(conns, r0)
	want := []store.OccupancySample{
		{PostalCode: "100001", ObservedAt: r0, Occupied: 1, Available: 2, Unavailable: 1},
		{PostalCode: "100002", ObservedAt: r0, Occupied: 2, Available: 0, Unavailable: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("Samples = %+v, want %+v (no sample for a postal code without connectors)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got := Samples(nil, r0); len(got) != 0 {
		t.Errorf("Samples(no connectors) = %+v, want none", got)
	}
}

// samples returns every stored occupancy sample, by time then postal code.
func (h *harness) samples() []store.OccupancySample {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(), `SELECT postal_code, observed_at, occupied, available, unavailable
		FROM occupancy_samples ORDER BY observed_at, postal_code`)
	if err != nil {
		h.t.Fatalf("query samples: %v", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.OccupancySample, error) {
		var s store.OccupancySample
		err := row.Scan(&s.PostalCode, &s.ObservedAt, &s.Occupied, &s.Available, &s.Unavailable)
		s.ObservedAt = s.ObservedAt.UTC()
		return s, err
	})
	if err != nil {
		h.t.Fatalf("scan samples: %v", err)
	}
	return list
}

func requireSamples(t *testing.T, got, want []store.OccupancySample) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("samples = %+v, want %+v", got, want)
	}
	for i := range want {
		w := want[i]
		w.ObservedAt = w.ObservedAt.UTC()
		if got[i] != w {
			t.Fatalf("sample %d = %+v, want %+v", i, got[i], w)
		}
	}
}

// LOC-1: the first poll writes one sample per watched postal code. A repeated
// feed writes none (MIR-3). A newer feed writes new samples, and a postal code
// missing from it gets none.
func TestReconcile_LOC1_SamplePerAppliedSnapshot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	first := []datamall.Connector{
		connAt("SG-A1", "100001", datamall.Occupied),
		connAt("SG-A2", "100001", datamall.Available),
		connAt("SG-B1", "100002", datamall.Unavailable),
	}
	h.apply(r0, first...)
	want := []store.OccupancySample{
		{PostalCode: "100001", ObservedAt: r0, Occupied: 1, Available: 1},
		{PostalCode: "100002", ObservedAt: r0, Unavailable: 1},
	}
	requireSamples(t, h.samples(), want)

	h.apply(r0, first...)
	h.apply(r0, connAt("SG-A1", "100001", datamall.Available)) // same timestamp, new content
	requireSamples(t, h.samples(), want)

	t1 := r0.Add(mins(5))
	h.apply(t1, connAt("SG-A1", "100001", datamall.Occupied), connAt("SG-A2", "100001", datamall.Occupied))
	want = append(want, store.OccupancySample{PostalCode: "100001", ObservedAt: t1, Occupied: 2})
	requireSamples(t, h.samples(), want)
}

// LOC-1, MIR-6: a cycle cut off part-way writes no sample, and the retry of
// the same file writes it once.
func TestReconcile_LOC1_CutCycleSampledOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.apply(r0, connAt("SG-A", "100001", datamall.Occupied), connAt("SG-B", "100001", datamall.Occupied))

	t1 := r0.Add(mins(5))
	poll := []datamall.Connector{connAt("SG-A", "100001", datamall.Available), connAt("SG-B", "100001", datamall.Occupied)}
	h.clock = t1
	if _, err := h.reconciler(&cutStore{Store: h.st, failAt: 2}).Apply(context.Background(), snapAt(t1, poll...)); !errors.Is(err, errCut) {
		t.Fatalf("cut cycle: err = %v, want the cut error", err)
	}
	r0Sample := store.OccupancySample{PostalCode: "100001", ObservedAt: r0, Occupied: 2}
	requireSamples(t, h.samples(), []store.OccupancySample{r0Sample})

	h.apply(t1, poll...)
	h.apply(t1, poll...)
	requireSamples(t, h.samples(), []store.OccupancySample{
		r0Sample, {PostalCode: "100001", ObservedAt: t1, Occupied: 1, Available: 1},
	})
}

// LOC-1, MIR-2: the counts cover only the watched connectors after the cap.
func TestReconcile_LOC1_SamplesAfterCap(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	log, _ := newLogger()
	r := &Reconciler{Store: fs, Now: func() time.Time { return r0 }, Logger: log, GapAfter: GapAfter}
	conns := make([]datamall.Connector, 0, MaxConnectors+5)
	for i := range MaxConnectors + 5 {
		conns = append(conns, connAt(fmt.Sprintf("SG-%03d", i), "100001", datamall.Available))
	}
	if _, err := r.Apply(context.Background(), snapAt(r0, conns...)); err != nil {
		t.Fatal(err)
	}
	want := []store.OccupancySample{{PostalCode: "100001", ObservedAt: r0, Available: MaxConnectors}}
	if len(fs.samples) != 1 || fs.samples[0] != want[0] {
		t.Fatalf("samples = %+v, want %+v", fs.samples, want)
	}
}
