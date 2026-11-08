package store_test

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/testdb"
)

// loc is the tariff's zone; every test time is built on 2026-11-06 in it.
var loc = billing.SingaporeTariff().Location

func at(h, m, s, ns int) time.Time { return time.Date(2026, 11, 6, h, m, s, ns, loc) }

// now is fixed so the clock-skew rules never depend on the real clock.
var now = at(12, 0, 0, 0)

func ctx() context.Context { return context.Background() }

// race runs fn(i) in n goroutines released together. The pool is warmed first
// (n open connections) so no goroutine is delayed by dialling; without that the
// first call commits before the others connect and the calls run serially,
// which would hide a missing row lock.
func race(t *testing.T, pool *pgxpool.Pool, n int, fn func(i int)) {
	t.Helper()
	conns := make([]*pgxpool.Conn, 0, n)
	for range n {
		c, err := pool.Acquire(ctx())
		if err != nil {
			t.Fatalf("warm pool: %v", err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	for i := range n {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			fn(i)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
}

func countReadings(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx(), `SELECT count(*) FROM meter_readings WHERE session_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count readings: %v", err)
	}
	return n
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	return store.New(testdb.New(t))
}

func requireRule(t *testing.T, err error, kind sessions.Kind, code string) {
	t.Helper()
	var re *sessions.RuleError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want *sessions.RuleError %s", err, code)
	}
	if re.Kind != kind || re.Code != code {
		t.Fatalf("rule error = kind %d code %q, want kind %d code %q", re.Kind, re.Code, kind, code)
	}
}

func mustCreate(t *testing.T, s *store.Store, charger string, start time.Time, wh int64) sessions.Session {
	t.Helper()
	sess, err := s.CreateSession(ctx(), sessions.SourceAPI, charger, start, wh, now)
	if err != nil {
		t.Fatalf("CreateSession(%s): %v", charger, err)
	}
	return sess
}

func TestMigrate_Idempotent_OPS4(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t) // testdb.New already ran Migrate once
	if err := store.Migrate(ctx(), pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := store.New(pool).Ping(ctx()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestCreateSession_SES1_OneActivePerCharger(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 1000)
	if first.ID == "" || first.ChargerID != "CH-1" || first.MeterStartWh != 1000 || first.StoppedAt != nil || first.MeterStopWh != nil {
		t.Fatalf("unexpected session %+v", first)
	}

	_, err := s.CreateSession(ctx(), sessions.SourceAPI, "CH-1", at(10, 5, 0, 0), 1000, now)
	requireRule(t, err, sessions.KindConflict, "active_session_exists")

	// A different charger is independent.
	mustCreate(t, s, "CH-2", at(10, 0, 0, 0), 0)

	if _, _, err := s.StopSession(ctx(), sessions.SourceAPI, first.ID, billing.Reading{At: at(10, 30, 0, 0), MeterWh: 2000}, now); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	mustCreate(t, s, "CH-1", at(11, 0, 0, 0), 2000)
}

func TestCreateSession_SES1_InvalidInputRunsRules(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.CreateSession(ctx(), sessions.SourceAPI, "bad id", at(10, 0, 0, 0), 0, now)
	requireRule(t, err, sessions.KindInvalid, "invalid_charger_id")
	_, err = s.CreateSession(ctx(), sessions.SourceAPI, "CH-1", now.Add(time.Hour), 0, now)
	requireRule(t, err, sessions.KindInvalid, "invalid_reading")
}

func TestCreateSession_SES1_ConcurrentOneWins(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	const n = 10
	errs := make([]error, n)
	race(t, pool, n, func(i int) {
		_, errs[i] = s.CreateSession(ctx(), sessions.SourceAPI, "CH-1", at(10, 0, 0, 0), 0, now)
	})

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		requireRule(t, err, sessions.KindConflict, "active_session_exists")
	}
	if ok != 1 {
		t.Fatalf("%d creates succeeded, want exactly 1", ok)
	}
}

// SES-2: accepted readings are stored; GetSession returns them sorted by time.
func TestAddReading_StoresAndReturnsSorted(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	sess := mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 0)
	for _, r := range []billing.Reading{{At: at(10, 10, 0, 0), MeterWh: 2000}, {At: at(10, 5, 0, 0), MeterWh: 1000}} {
		_, _, dup, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, r, now)
		if err != nil || dup {
			t.Fatalf("AddReading(%v) = %v, %v", r, dup, err)
		}
	}
	got, readings, err := s.GetSession(ctx(), sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("id = %s, want %s", got.ID, sess.ID)
	}
	want := []billing.Reading{{At: at(10, 5, 0, 0), MeterWh: 1000}, {At: at(10, 10, 0, 0), MeterWh: 2000}}
	if len(readings) != len(want) {
		t.Fatalf("readings = %v, want %v", readings, want)
	}
	for i := range want {
		if !readings[i].At.Equal(want[i].At) || readings[i].MeterWh != want[i].MeterWh {
			t.Fatalf("readings[%d] = %v, want %v", i, readings[i], want[i])
		}
	}
}

// BIL-2: timestamps are stored and returned in UTC.
func TestGetSession_ReturnsUTC(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	sess := mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 0)
	if sess.StartedAt.Location() != time.UTC {
		t.Fatalf("created StartedAt zone = %v, want UTC", sess.StartedAt.Location())
	}
	if _, _, _, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: at(10, 5, 0, 0), MeterWh: 10}, now); err != nil {
		t.Fatal(err)
	}
	stopAt := at(10, 20, 0, 0)
	if _, _, err := s.StopSession(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: stopAt, MeterWh: 20}, now); err != nil {
		t.Fatal(err)
	}
	got, readings, err := s.GetSession(ctx(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StartedAt.Location() != time.UTC || got.StoppedAt == nil || got.StoppedAt.Location() != time.UTC {
		t.Fatalf("session times not UTC: %+v", got)
	}
	if !got.StartedAt.Equal(at(10, 0, 0, 0)) || !got.StoppedAt.Equal(stopAt) {
		t.Fatalf("instants changed: %+v", got)
	}
	if len(readings) != 1 || readings[0].At.Location() != time.UTC {
		t.Fatalf("readings not UTC: %v", readings)
	}
}

// SES-2/SES-6: an identical repeat is a no-op, not a second row.
func TestAddReading_DuplicateIsNoop(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	sess := mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 0)
	r := billing.Reading{At: at(10, 5, 0, 0), MeterWh: 500}
	if _, _, dup, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, r, now); err != nil || dup {
		t.Fatalf("first = %v, %v", dup, err)
	}
	if _, _, dup, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, r, now); err != nil || !dup {
		t.Fatalf("second = %v, %v; want duplicate", dup, err)
	}
	if n := countReadings(t, pool, sess.ID); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// Postgres keeps microseconds, so two instants inside one microsecond are the
// same stored point; the second must be a replay, not a conflict.
// SES-2 + Review Focus 3: sub-microsecond differences are the same stored point.
func TestAddReading_SubMicrosecondReplayIsDuplicate(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	sess := mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 0)
	if _, _, dup, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: at(10, 10, 0, 123), MeterWh: 700}, now); err != nil || dup {
		t.Fatalf("first = %v, %v", dup, err)
	}
	if _, _, dup, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: at(10, 10, 0, 456), MeterWh: 700}, now); err != nil || !dup {
		t.Fatalf("second = %v, %v; want duplicate", dup, err)
	}
	if n := countReadings(t, pool, sess.ID); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestAddReading_SES2_ConflictingValueAtSameTimestamp(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	sess := mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 0)
	if _, _, _, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: at(10, 5, 0, 0), MeterWh: 500}, now); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: at(10, 5, 0, 0), MeterWh: 600}, now)
	requireRule(t, err, sessions.KindConflict, "conflicting_reading")
}

// API-2: unknown or malformed ids are 404 (ErrNotFound).
func TestAddReading_UnknownOrMalformedID(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	r := billing.Reading{At: at(10, 5, 0, 0), MeterWh: 1}
	for _, id := range []string{"00000000-0000-4000-8000-000000000000", "nope", ""} {
		if _, _, _, err := s.AddReading(ctx(), sessions.SourceAPI, id, r, now); !errors.Is(err, sessions.ErrNotFound) {
			t.Errorf("AddReading(%q) err = %v, want ErrNotFound", id, err)
		}
		if _, _, err := s.StopSession(ctx(), sessions.SourceAPI, id, r, now); !errors.Is(err, sessions.ErrNotFound) {
			t.Errorf("StopSession(%q) err = %v, want ErrNotFound", id, err)
		}
		if _, _, err := s.GetSession(ctx(), id); !errors.Is(err, sessions.ErrNotFound) {
			t.Errorf("GetSession(%q) err = %v, want ErrNotFound", id, err)
		}
	}
}

func TestStopSession_SES3(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	sess := mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 0)
	if _, _, _, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: at(10, 5, 0, 0), MeterWh: 500}, now); err != nil {
		t.Fatal(err)
	}
	stop := billing.Reading{At: at(10, 30, 0, 0), MeterWh: 3000}
	stopped, readings, err := s.StopSession(ctx(), sessions.SourceAPI, sess.ID, stop, now)
	if err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if stopped.StoppedAt == nil || stopped.MeterStopWh == nil {
		t.Fatalf("stopped has nil stop fields: %+v", stopped)
	}
	if !stopped.StoppedAt.Equal(stop.At) || *stopped.MeterStopWh != 3000 {
		t.Fatalf("stopped = %+v", stopped)
	}
	if len(readings) != 1 || readings[0].MeterWh != 500 {
		t.Fatalf("readings = %v", readings)
	}

	_, _, _, err = s.AddReading(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: at(10, 40, 0, 0), MeterWh: 4000}, now)
	requireRule(t, err, sessions.KindConflict, "session_stopped")

	replay, rReadings, err := s.StopSession(ctx(), sessions.SourceAPI, sess.ID, stop, now)
	if err != nil {
		t.Fatalf("identical stop: %v", err)
	}
	if replay.StoppedAt == nil || replay.MeterStopWh == nil {
		t.Fatalf("replay has nil stop fields: %+v", replay)
	}
	if replay.ID != stopped.ID || !replay.StoppedAt.Equal(*stopped.StoppedAt) || *replay.MeterStopWh != *stopped.MeterStopWh || len(rReadings) != 1 {
		t.Fatalf("replay = %+v %v, want unchanged", replay, rReadings)
	}

	_, _, err = s.StopSession(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: at(10, 31, 0, 0), MeterWh: 3500}, now)
	requireRule(t, err, sessions.KindConflict, "stop_conflict")
}

// UI-1/API list: newest first, limited, each with sorted readings.
func TestListRecent_NewestFirstWithLimit(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a := mustCreate(t, s, "CH-A", at(8, 0, 0, 0), 0)
	b := mustCreate(t, s, "CH-B", at(9, 0, 0, 0), 0)
	c := mustCreate(t, s, "CH-C", at(10, 0, 0, 0), 0)
	for _, sess := range []sessions.Session{a, b, c} {
		for _, m := range []int{5, 1} { // inserted out of order on purpose
			if _, _, _, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: sess.StartedAt.Add(time.Duration(m) * time.Minute), MeterWh: int64(m)}, now); err != nil {
				t.Fatal(err)
			}
		}
	}

	got, err := s.ListRecent(ctx(), 2)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(got) != 2 || got[0].Session.ID != c.ID || got[1].Session.ID != b.ID {
		t.Fatalf("got %+v, want [c b]", got)
	}
	for _, g := range got {
		if len(g.Readings) != 2 || !g.Readings[0].At.Before(g.Readings[1].At) {
			t.Fatalf("readings for %s = %v, want 2 sorted", g.Session.ID, g.Readings)
		}
	}

	if _, err := s.ListRecent(ctx(), 0); err == nil {
		t.Fatal("limit 0: want error")
	}
}

// Review Focus 5 (SES-6): concurrent identical readings are idempotent.
func TestAddReading_SES6_ConcurrentDuplicates(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	sess := mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 0)
	r := billing.Reading{At: at(10, 5, 0, 0), MeterWh: 500}

	const n = 10
	dups := make([]bool, n)
	errs := make([]error, n)
	race(t, pool, n, func(i int) {
		_, _, dups[i], errs[i] = s.AddReading(ctx(), sessions.SourceAPI, sess.ID, r, now)
	})

	fresh := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if !dups[i] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d calls reported duplicate=false, want exactly 1", fresh)
	}
	if got := countReadings(t, pool, sess.ID); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
}

// SES-3/SES-6: a reading racing a stop is either stored before the stop (and
// not after it) or rejected with session_stopped.
func TestAddReadingVsStop_SES6_Consistent(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	sess := mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 0)
	// The reading is later than the stop, so storing it after the stop (or
	// stopping after it) would be inconsistent and is detectable.
	reading := billing.Reading{At: at(10, 20, 0, 0), MeterWh: 900}
	stop := billing.Reading{At: at(10, 10, 0, 0), MeterWh: 600}

	var addErr, stopErr error
	race(t, pool, 2, func(i int) {
		if i == 0 {
			_, _, _, addErr = s.AddReading(ctx(), sessions.SourceAPI, sess.ID, reading, now)
		} else {
			_, _, stopErr = s.StopSession(ctx(), sessions.SourceAPI, sess.ID, stop, now)
		}
	})

	got, readings, err := s.GetSession(ctx(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case addErr == nil && stopErr == nil:
		t.Fatalf("both succeeded, but the reading is after the stop: reading=%v stop=%v", reading, stop)
	case addErr == nil:
		// Reading first; the stop must then be invalid (before the last reading).
		requireRule(t, stopErr, sessions.KindInvalid, "invalid_reading")
		if got.StoppedAt != nil || len(readings) != 1 {
			t.Fatalf("state = %+v %v, want active with 1 reading", got, readings)
		}
	default:
		requireRule(t, addErr, sessions.KindConflict, "session_stopped")
		if stopErr != nil || got.StoppedAt == nil || len(readings) != 0 {
			t.Fatalf("state = %+v %v stopErr=%v, want stopped with no readings", got, readings, stopErr)
		}
	}
}

// OPS-4: the Down migration works and the schema can be re-applied.
func TestMigrate_DownThenUp_OPS4(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	fsys, err := fs.Sub(store.MigrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	p, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx(), 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx(), `SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name IN ('sessions','meter_readings')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tables after down = %d, %v; want 0", n, err)
	}
	if err := store.Migrate(ctx(), pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// SES-2: AddReading returns the session and all readings, sorted, from its own
// transaction, for both a new reading and a duplicate.
func TestAddReading_ReturnsSessionAndReadings(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	sess := mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 0)
	r1 := billing.Reading{At: at(10, 10, 0, 0), MeterWh: 2000}
	r2 := billing.Reading{At: at(10, 5, 0, 0), MeterWh: 1000}
	for i, r := range []billing.Reading{r1, r2, r2} {
		got, readings, dup, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, r, now)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got.ID != sess.ID || dup != (i == 2) {
			t.Errorf("call %d: id %s dup %v", i, got.ID, dup)
		}
		wantLen := min(i+1, 2)
		if len(readings) != wantLen {
			t.Fatalf("call %d: %d readings, want %d", i, len(readings), wantLen)
		}
	}
}

// createAs starts a session written by the given source.
func createAs(t *testing.T, s *store.Store, as sessions.Source, charger string) sessions.Session {
	t.Helper()
	sess, err := s.CreateSession(ctx(), as, charger, at(10, 0, 0, 0), 1000, now)
	if err != nil {
		t.Fatalf("CreateSession(%s, %s): %v", as, charger, err)
	}
	return sess
}

// MIR-7: the API cannot write to a mirror session, and nothing changes.
func TestStore_MIR7_APICannotWriteMirrorSession(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	sess := createAs(t, s, sessions.SourceMirror, "CH-M")
	existing := billing.Reading{At: at(10, 5, 0, 0), MeterWh: 1500}
	if _, _, _, err := s.AddReading(ctx(), sessions.SourceMirror, sess.ID, existing, now); err != nil {
		t.Fatalf("mirror AddReading: %v", err)
	}

	// A new reading, an identical replay of the stored one, and a stop.
	for _, r := range []billing.Reading{{At: at(10, 10, 0, 0), MeterWh: 1800}, existing} {
		_, _, _, err := s.AddReading(ctx(), sessions.SourceAPI, sess.ID, r, now)
		requireRule(t, err, sessions.KindConflict, "mirror_session")
	}
	_, _, err := s.StopSession(ctx(), sessions.SourceAPI, sess.ID, billing.Reading{At: at(10, 20, 0, 0), MeterWh: 2000}, now)
	requireRule(t, err, sessions.KindConflict, "mirror_session")

	got, readings, err := s.GetSession(ctx(), sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.StoppedAt != nil || got.MeterStopWh != nil {
		t.Errorf("MIR-7: mirror session was stopped by the API: %+v", got)
	}
	if len(readings) != 1 || !readings[0].At.Equal(existing.At) || readings[0].MeterWh != existing.MeterWh {
		t.Errorf("MIR-7: readings = %v, want only %v", readings, existing)
	}
}

// MIR-7: the mirror cannot write to an api session either.
func TestStore_MIR7_MirrorCannotWriteAPISession(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	sess := createAs(t, s, sessions.SourceAPI, "CH-A")
	_, _, _, err := s.AddReading(ctx(), sessions.SourceMirror, sess.ID, billing.Reading{At: at(10, 5, 0, 0), MeterWh: 1500}, now)
	requireRule(t, err, sessions.KindConflict, "api_session")
	_, _, err = s.StopSession(ctx(), sessions.SourceMirror, sess.ID, billing.Reading{At: at(10, 20, 0, 0), MeterWh: 2000}, now)
	requireRule(t, err, sessions.KindConflict, "api_session")
}

// MIR-7: GetSession and ListRecent return the source the session was created with.
func TestStore_MIR7_SourceRoundTrips(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	api := createAs(t, s, sessions.SourceAPI, "CH-A")
	mirror := createAs(t, s, sessions.SourceMirror, "CH-M")
	if api.Source != sessions.SourceAPI || mirror.Source != sessions.SourceMirror {
		t.Fatalf("MIR-7: created sources = %q, %q", api.Source, mirror.Source)
	}
	for _, want := range []sessions.Session{api, mirror} {
		got, _, err := s.GetSession(ctx(), want.ID)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.Source != want.Source {
			t.Errorf("MIR-7: GetSession source = %q, want %q", got.Source, want.Source)
		}
	}
	list, err := s.ListRecent(ctx(), 10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	have := map[string]sessions.Source{}
	for _, item := range list {
		have[item.Session.ID] = item.Session.Source
	}
	if have[api.ID] != sessions.SourceAPI || have[mirror.ID] != sessions.SourceMirror {
		t.Errorf("MIR-7: ListRecent sources = %v", have)
	}
}
