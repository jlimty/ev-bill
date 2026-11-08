package store_test

import (
	"context"
	"errors"
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

func chargerRow(id string, st datamall.Status, held bool, observed time.Time) store.ChargerRow {
	return store.ChargerRow{
		Connector: datamall.Connector{
			ChargerID: id, LocationName: "Test Mall", Address: "1 Test Road", PostalCode: "100001",
			Operator: "Testco", Position: "B1", PlugType: "Type 2", Current: "AC", PowerW: 7_400,
			Price: "0.5000", PriceType: "", Status: st,
		},
		Held:       held,
		ObservedAt: observed,
	}
}

func mirrorCreate(t *testing.T, s *store.Store, charger string, start time.Time, wh int64) sessions.Session {
	t.Helper()
	sess, err := s.CreateSession(ctx(), sessions.SourceMirror, charger, start, wh, now)
	if err != nil {
		t.Fatalf("CreateSession(mirror, %s): %v", charger, err)
	}
	return sess
}

func mirrorStop(t *testing.T, s *store.Store, id string, at time.Time, wh int64) {
	t.Helper()
	if _, _, _, err := s.StopSession(ctx(), sessions.SourceMirror, id, billing.Reading{At: at, MeterWh: wh}, now); err != nil {
		t.Fatalf("StopSession(mirror, %s): %v", id, err)
	}
}

// MIR-8: chargers are upserted by charger_id, and MIR-3's "latest observation"
// is the newest observed_at.
func TestStore_MIR8_UpsertChargersAndLatestObservation(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	latest, ok, err := s.LatestObservation(ctx())
	if err != nil || ok || !latest.IsZero() {
		t.Fatalf("empty LatestObservation = (%v, %v, %v), want (zero, false, nil)", latest, ok, err)
	}
	if _, found, err := s.Charger(ctx(), "SG-1"); err != nil || found {
		t.Fatalf("Charger on empty table = found %v, err %v; want not found", found, err)
	}

	first := []store.ChargerRow{
		chargerRow("SG-1", datamall.Available, false, at(10, 0, 0, 0)),
		chargerRow("SG-2", datamall.Occupied, false, at(10, 0, 0, 0)),
	}
	if err := s.UpsertChargers(ctx(), first); err != nil {
		t.Fatalf("UpsertChargers: %v", err)
	}

	second := chargerRow("SG-1", datamall.Occupied, true, at(10, 5, 0, 0))
	second.Price = "0.6000"
	second.Position = "B2"
	if err := s.UpsertChargers(ctx(), []store.ChargerRow{second}); err != nil {
		t.Fatalf("second UpsertChargers: %v", err)
	}

	latest, ok, err = s.LatestObservation(ctx())
	if err != nil || !ok || !latest.Equal(at(10, 5, 0, 0)) {
		t.Fatalf("LatestObservation = (%v, %v, %v), want (%v, true, nil)", latest, ok, err, at(10, 5, 0, 0))
	}
	if latest.Location() != time.UTC {
		t.Errorf("LatestObservation location = %v, want UTC", latest.Location())
	}

	got, found, err := s.Charger(ctx(), "SG-1")
	if err != nil || !found {
		t.Fatalf("Charger(SG-1) = found %v, err %v", found, err)
	}
	second.ObservedAt = second.ObservedAt.UTC()
	if got != second {
		t.Errorf("Charger(SG-1) = %+v, want %+v (details updated)", got, second)
	}

	states, err := s.ChargerStates(ctx())
	if err != nil {
		t.Fatalf("ChargerStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("ChargerStates has %d rows, want 2 (upsert never deletes)", len(states))
	}
	if states["SG-1"] != second {
		t.Errorf("ChargerStates[SG-1] = %+v, want %+v", states["SG-1"], second)
	}
	if !states["SG-2"].ObservedAt.Equal(at(10, 0, 0, 0)) || states["SG-2"].Status != datamall.Occupied {
		t.Errorf("ChargerStates[SG-2] = %+v, want the first upsert's row", states["SG-2"])
	}
}

// MIR-3: an older observation never overwrites a newer charger row, so a
// late writer cannot move details or observed_at backwards.
func TestStore_MIR3_UpsertNeverMovesObservedAtBack(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if err := s.UpsertChargers(ctx(), []store.ChargerRow{chargerRow("SG-1", datamall.Occupied, false, at(10, 5, 0, 0))}); err != nil {
		t.Fatal(err)
	}
	older := chargerRow("SG-1", datamall.Available, false, at(10, 0, 0, 0))
	if err := s.UpsertChargers(ctx(), []store.ChargerRow{older}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Charger(ctx(), "SG-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != datamall.Occupied || !got.ObservedAt.Equal(at(10, 5, 0, 0)) {
		t.Errorf("Charger = %+v, want the newer row kept", got)
	}
}

// MIR-8: the batch is one transaction, so a bad row leaves nothing behind.
func TestStore_MIR8_UpsertChargersIsAtomic(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	bad := chargerRow("SG-2", datamall.Available, false, at(10, 0, 0, 0))
	bad.PowerW = 0 // violates CHECK (power_w > 0)
	err := s.UpsertChargers(ctx(), []store.ChargerRow{chargerRow("SG-1", datamall.Available, false, at(10, 0, 0, 0)), bad})
	if err == nil {
		t.Fatal("UpsertChargers with a bad row returned nil")
	}
	if _, ok, err := s.LatestObservation(ctx()); err != nil || ok {
		t.Fatalf("after a failed batch LatestObservation ok = %v, err = %v; want nothing stored", ok, err)
	}
	if err := s.UpsertChargers(ctx(), nil); err != nil {
		t.Fatalf("UpsertChargers(nil) = %v, want nil", err)
	}
}

// MIR-6, MIR-7: the reconciler sees only open mirror sessions, with their
// readings sorted by time.
func TestStore_MIR6_OpenMirrorSessionsExcludesAPIAndStopped(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	open := mirrorCreate(t, s, "M-OPEN", at(10, 0, 0, 0), 100)
	for _, r := range []billing.Reading{{At: at(10, 10, 0, 0), MeterWh: 300}, {At: at(10, 5, 0, 0), MeterWh: 200}} {
		if _, _, _, err := s.AddReading(ctx(), sessions.SourceMirror, open.ID, r, now); err != nil {
			t.Fatal(err)
		}
	}
	bare := mirrorCreate(t, s, "M-BARE", at(10, 0, 0, 0), 0)
	stopped := mirrorCreate(t, s, "M-STOPPED", at(10, 0, 0, 0), 0)
	mirrorStop(t, s, stopped.ID, at(10, 5, 0, 0), 10)
	mustCreate(t, s, "API-OPEN", at(10, 0, 0, 0), 0)

	got, err := s.OpenMirrorSessions(ctx())
	if err != nil {
		t.Fatalf("OpenMirrorSessions: %v", err)
	}
	byCharger := map[string]store.SessionWithReadings{}
	for _, sw := range got {
		byCharger[sw.Session.ChargerID] = sw
	}
	if len(got) != 2 || byCharger["M-OPEN"].Session.ID != open.ID || byCharger["M-BARE"].Session.ID != bare.ID {
		t.Fatalf("OpenMirrorSessions = %+v, want M-OPEN and M-BARE only", got)
	}
	rs := byCharger["M-OPEN"].Readings
	if len(rs) != 2 || !rs[0].At.Equal(at(10, 5, 0, 0)) || rs[0].MeterWh != 200 || rs[1].MeterWh != 300 {
		t.Errorf("M-OPEN readings = %+v, want 200 then 300 sorted by time", rs)
	}
	if rs[0].At.Location() != time.UTC {
		t.Errorf("reading location = %v, want UTC", rs[0].At.Location())
	}
	if br := byCharger["M-BARE"].Readings; br == nil || len(br) != 0 {
		t.Errorf("M-BARE readings = %#v, want an empty, non-nil slice", br)
	}
}

// MIR-4, MIR-12: the newest stopped mirror session per charger, ignoring API
// sessions and open ones.
func TestStore_MIR4_LastMirrorStopsNewestPerCharger(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	a1 := mirrorCreate(t, s, "M-A", at(8, 0, 0, 0), 0)
	mirrorStop(t, s, a1.ID, at(9, 0, 0, 0), 1_000)
	a2 := mirrorCreate(t, s, "M-A", at(9, 30, 0, 0), 1_000)
	mirrorStop(t, s, a2.ID, at(10, 0, 0, 0), 2_000)
	mirrorCreate(t, s, "M-A", at(10, 30, 0, 0), 2_000) // open: not a stop

	api := mustCreate(t, s, "API-B", at(8, 0, 0, 0), 0)
	if _, _, _, err := s.StopSession(ctx(), sessions.SourceAPI, api.ID, billing.Reading{At: at(9, 0, 0, 0), MeterWh: 5}, now); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastMirrorStops(ctx())
	if err != nil {
		t.Fatalf("LastMirrorStops: %v", err)
	}
	want := store.MirrorStop{MeterWh: 2_000, StartedAt: at(9, 30, 0, 0).UTC(), StoppedAt: at(10, 0, 0, 0).UTC()}
	if len(got) != 1 || got["M-A"] != want {
		t.Fatalf("LastMirrorStops = %+v, want only M-A = %+v", got, want)
	}
}

// MIR-12: HeldCount counts chargers held after a 48 h stop, among those the
// latest poll observed (rows for chargers that left scope are ignored).
func TestStore_MIR12_HeldCount(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if n, err := s.HeldCount(ctx()); err != nil || n != 0 {
		t.Fatalf("empty HeldCount = %d, %v; want 0", n, err)
	}
	rows := []store.ChargerRow{
		chargerRow("SG-1", datamall.Occupied, true, at(10, 0, 0, 0)),
		chargerRow("SG-2", datamall.Occupied, true, at(10, 0, 0, 0)),
		chargerRow("SG-3", datamall.Occupied, false, at(10, 0, 0, 0)),
	}
	if err := s.UpsertChargers(ctx(), rows); err != nil {
		t.Fatal(err)
	}
	if n, err := s.HeldCount(ctx()); err != nil || n != 2 {
		t.Fatalf("HeldCount = %d, %v; want 2", n, err)
	}

	// SG-2 leaves scope: only SG-1 and SG-3 are in the newer poll. A held row
	// that the latest poll did not observe is not counted.
	newer := []store.ChargerRow{
		chargerRow("SG-1", datamall.Occupied, true, at(10, 5, 0, 0)),
		chargerRow("SG-3", datamall.Occupied, false, at(10, 5, 0, 0)),
	}
	if err := s.UpsertChargers(ctx(), newer); err != nil {
		t.Fatal(err)
	}
	if n, err := s.HeldCount(ctx()); err != nil || n != 1 {
		t.Fatalf("HeldCount after SG-2 left scope = %d, %v; want 1", n, err)
	}
}

// MIR-9: retention deletes completed mirror sessions older than the cutoff,
// with their readings, and nothing else.
func TestStore_MIR9_DeleteCompletedMirrorBefore(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)

	old := mirrorCreate(t, s, "M-OLD", at(1, 0, 0, 0), 0)
	if _, _, _, err := s.AddReading(ctx(), sessions.SourceMirror, old.ID, billing.Reading{At: at(1, 5, 0, 0), MeterWh: 5}, now); err != nil {
		t.Fatal(err)
	}
	mirrorStop(t, s, old.ID, at(2, 0, 0, 0), 10)
	recent := mirrorCreate(t, s, "M-RECENT", at(3, 0, 0, 0), 0)
	mirrorStop(t, s, recent.ID, at(9, 0, 0, 0), 10)
	openOld := mirrorCreate(t, s, "M-OPEN", at(1, 0, 0, 0), 0)
	api := mustCreate(t, s, "API-OLD", at(1, 0, 0, 0), 0)
	if _, _, _, err := s.StopSession(ctx(), sessions.SourceAPI, api.ID, billing.Reading{At: at(2, 0, 0, 0), MeterWh: 5}, now); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteCompletedMirrorBefore(ctx(), at(8, 0, 0, 0))
	if err != nil || n != 1 {
		t.Fatalf("DeleteCompletedMirrorBefore = %d, %v; want 1, nil", n, err)
	}
	if _, _, err := s.GetSession(ctx(), old.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("old mirror session: err = %v, want ErrNotFound", err)
	}
	if c := countReadings(t, pool, old.ID); c != 0 {
		t.Errorf("old mirror session still has %d readings, want 0 (cascade)", c)
	}
	for _, id := range []string{recent.ID, openOld.ID, api.ID} {
		if _, _, err := s.GetSession(ctx(), id); err != nil {
			t.Errorf("session %s: %v, want kept", id, err)
		}
	}
	if n, err := s.DeleteCompletedMirrorBefore(ctx(), at(8, 0, 0, 0)); err != nil || n != 0 {
		t.Errorf("second delete = %d, %v; want 0, nil", n, err)
	}
}

// API-2: the mirror methods return plain errors when the database fails.
func TestStore_MIR_FailuresArePlainErrors(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	ops := map[string]func() error{
		"UpsertChargers": func() error {
			return s.UpsertChargers(cctx, []store.ChargerRow{chargerRow("SG-1", datamall.Available, false, at(10, 0, 0, 0))})
		},
		"ChargerStates":     func() error { _, err := s.ChargerStates(cctx); return err },
		"LatestObservation": func() error { _, _, err := s.LatestObservation(cctx); return err },
		"OpenMirrorSessions": func() error {
			_, err := s.OpenMirrorSessions(cctx)
			return err
		},
		"LastMirrorStops": func() error { _, err := s.LastMirrorStops(cctx); return err },
		"DeleteCompletedMirrorBefore": func() error {
			_, err := s.DeleteCompletedMirrorBefore(cctx, now)
			return err
		},
		"ApplyObservation": func() error {
			return s.ApplyObservation(cctx, nil, []store.OccupancySample{sample("100001", now, 1, 0, 0)})
		},
		"DeleteSamplesBefore": func() error {
			_, err := s.DeleteSamplesBefore(cctx, now)
			return err
		},
		"Charger":   func() error { _, _, err := s.Charger(cctx, "SG-1"); return err },
		"HeldCount": func() error { _, err := s.HeldCount(cctx); return err },
	}
	for name, op := range ops {
		requirePlainError(t, name, op())
	}
}

// API-2: with the readings table gone, loading open sessions' readings fails.
func TestStore_MIR6_OpenMirrorSessionsReadingsFailure(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	mirrorCreate(t, s, "M-1", at(10, 0, 0, 0), 0)
	if _, err := pool.Exec(ctx(), "DROP TABLE meter_readings"); err != nil {
		t.Fatal(err)
	}
	_, err := s.OpenMirrorSessions(ctx())
	requirePlainError(t, "OpenMirrorSessions", err)
}

// LIVE-4: the list carries each session's charger details for the page: a
// mirror session on a known charger has Charger filled in from the chargers
// table, and an API session on an unknown charger ID has Charger == nil. The
// readings still come back sorted for both.
func TestStore_LIVE4_ListRecentJoinsCharger(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	row := chargerRow("SG-LIVE", datamall.Occupied, false, at(10, 0, 0, 0))
	row.Current, row.PowerW, row.Operator = "DC", 50_000, "Fastco"
	if err := s.UpsertChargers(ctx(), []store.ChargerRow{row}); err != nil {
		t.Fatal(err)
	}
	m := mirrorCreate(t, s, "SG-LIVE", at(10, 0, 0, 0), 0)
	for _, r := range []billing.Reading{{At: at(10, 10, 0, 0), MeterWh: 300}, {At: at(10, 5, 0, 0), MeterWh: 200}} {
		if _, _, _, err := s.AddReading(ctx(), sessions.SourceMirror, m.ID, r, now); err != nil {
			t.Fatal(err)
		}
	}
	api := mustCreate(t, s, "API-UNKNOWN", at(9, 0, 0, 0), 0)

	got, err := s.ListRecent(ctx(), 10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(got) != 2 || got[0].Session.ID != m.ID || got[1].Session.ID != api.ID {
		t.Fatalf("ListRecent = %+v, want [mirror, api]", got)
	}
	c := got[0].Charger
	if c == nil {
		t.Fatal("LIVE-4: mirror session on a known charger has Charger == nil")
	}
	want := row
	want.ObservedAt = row.ObservedAt.UTC()
	if *c != want {
		t.Errorf("LIVE-4: Charger = %+v, want %+v", *c, want)
	}
	if c.ObservedAt.Location() != time.UTC {
		t.Errorf("LIVE-4: ObservedAt location = %v, want UTC", c.ObservedAt.Location())
	}
	if rs := got[0].Readings; len(rs) != 2 || rs[0].MeterWh != 200 || rs[1].MeterWh != 300 {
		t.Errorf("mirror readings = %+v, want 200 then 300", rs)
	}
	if got[1].Charger != nil {
		t.Errorf("LIVE-4: API session on an unknown charger has Charger = %+v, want nil", *got[1].Charger)
	}
	if rs := got[1].Readings; rs == nil || len(rs) != 0 {
		t.Errorf("API readings = %#v, want an empty, non-nil slice", rs)
	}
}

// API-2: with the chargers table gone, the joined list fails with a plain error.
func TestStore_LIVE4_ListRecentChargersFailure(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	mustCreate(t, s, "CH-1", at(10, 0, 0, 0), 0)
	if _, err := pool.Exec(ctx(), "DROP TABLE chargers"); err != nil {
		t.Fatal(err)
	}
	_, err := s.ListRecent(ctx(), 5)
	requirePlainError(t, "ListRecent", err)
}

// storedSamples returns every occupancy sample, by postal code then time.
func storedSamples(t *testing.T, pool *pgxpool.Pool) []store.OccupancySample {
	t.Helper()
	rows, err := pool.Query(ctx(), `SELECT postal_code, observed_at, occupied, available, unavailable
		FROM occupancy_samples ORDER BY postal_code, observed_at`)
	if err != nil {
		t.Fatalf("query samples: %v", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.OccupancySample, error) {
		var s store.OccupancySample
		err := row.Scan(&s.PostalCode, &s.ObservedAt, &s.Occupied, &s.Available, &s.Unavailable)
		s.ObservedAt = s.ObservedAt.UTC()
		return s, err
	})
	if err != nil {
		t.Fatalf("scan samples: %v", err)
	}
	return list
}

func sample(postal string, observed time.Time, occupied, available, unavailable int) store.OccupancySample {
	return store.OccupancySample{PostalCode: postal, ObservedAt: observed, Occupied: occupied, Available: available, Unavailable: unavailable}
}

// LOC-1: the charger rows and the occupancy samples of one observation are
// both stored. A sample whose (postal_code, observed_at) is already stored is
// left as it is, without an error (ON CONFLICT DO NOTHING, for two instances
// writing at once during a deploy overlap).
func TestStore_LOC1_ApplyObservationWritesBoth(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	t0 := at(10, 0, 0, 0)

	rows := []store.ChargerRow{
		chargerRow("SG-1", datamall.Occupied, false, t0),
		chargerRow("SG-2", datamall.Available, false, t0),
	}
	samples := []store.OccupancySample{sample("100001", t0, 1, 1, 0), sample("100002", t0, 0, 0, 2)}
	if err := s.ApplyObservation(ctx(), rows, samples); err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}
	states, err := s.ChargerStates(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states["SG-1"].Status != datamall.Occupied || !states["SG-2"].ObservedAt.Equal(t0) {
		t.Fatalf("ChargerStates = %+v, want SG-1 and SG-2 observed at t0", states)
	}
	want := []store.OccupancySample{sample("100001", t0.UTC(), 1, 1, 0), sample("100002", t0.UTC(), 0, 0, 2)}
	got := storedSamples(t, pool)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("samples = %+v, want %+v", got, want)
	}

	// The same key again, even with different counts, is a no-op.
	again := []store.OccupancySample{sample("100001", t0, 2, 0, 0)}
	if err := s.ApplyObservation(ctx(), nil, again); err != nil {
		t.Fatalf("repeated sample: %v, want nil (ON CONFLICT DO NOTHING)", err)
	}
	got = storedSamples(t, pool)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("after a repeated sample: %+v, want %+v unchanged", got, want)
	}
}

// LOC-1: the rows and the samples are one transaction. A bad charger row or a
// bad sample leaves neither the rows nor the samples behind.
func TestStore_LOC1_ApplyObservationIsAtomic(t *testing.T) {
	t.Parallel()
	t0 := at(10, 0, 0, 0)
	badRow := chargerRow("SG-2", datamall.Available, false, t0)
	badRow.PowerW = 0 // violates CHECK (power_w > 0)
	goodRows := []store.ChargerRow{chargerRow("SG-1", datamall.Available, false, t0)}
	goodSamples := []store.OccupancySample{sample("100001", t0, 0, 1, 0)}

	for _, tc := range []struct {
		name    string
		rows    []store.ChargerRow
		samples []store.OccupancySample
	}{
		{"bad charger row", append(goodRows, badRow), goodSamples},
		{"bad sample", goodRows, append(goodSamples, sample("100002", t0, -1, 0, 0))}, // CHECK (occupied >= 0)
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testdb.New(t)
			s := store.New(pool)
			err := s.ApplyObservation(ctx(), tc.rows, tc.samples)
			requirePlainError(t, "ApplyObservation", err)
			if _, ok, err := s.LatestObservation(ctx()); err != nil || ok {
				t.Fatalf("charger rows stored after a failed observation (ok = %v, err = %v)", ok, err)
			}
			if got := storedSamples(t, pool); len(got) != 0 {
				t.Fatalf("samples stored after a failed observation: %+v", got)
			}
		})
	}
}

// LOC-1: an observation with no rows and no samples writes nothing and needs
// no transaction, so even a cancelled context succeeds. Samples without rows
// are still written.
func TestStore_LOC1_ApplyObservationEmptyAndSamplesOnly(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ApplyObservation(cctx, nil, nil); err != nil {
		t.Fatalf("ApplyObservation(nil, nil) = %v, want nil without touching the database", err)
	}
	t0 := at(10, 0, 0, 0)
	if err := s.ApplyObservation(ctx(), nil, []store.OccupancySample{sample("100001", t0, 3, 0, 1)}); err != nil {
		t.Fatalf("samples only: %v", err)
	}
	if got := storedSamples(t, pool); len(got) != 1 || got[0] != sample("100001", t0.UTC(), 3, 0, 1) {
		t.Fatalf("samples = %+v, want the one sample", got)
	}
}

// LOC-1, MIR-9: retention deletes samples observed before the cutoff and
// keeps the rest, including one exactly at the cutoff.
func TestStore_LOC1_DeleteSamplesBefore(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	cutoff := at(8, 0, 0, 0)
	samples := []store.OccupancySample{
		sample("100001", at(1, 0, 0, 0), 1, 0, 0),
		sample("100002", at(7, 59, 59, 0), 1, 0, 0),
		sample("100001", cutoff, 0, 1, 0),
		sample("100002", at(9, 0, 0, 0), 0, 0, 1),
	}
	if err := s.ApplyObservation(ctx(), nil, samples); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteSamplesBefore(ctx(), cutoff)
	if err != nil || n != 2 {
		t.Fatalf("DeleteSamplesBefore = %d, %v; want 2, nil", n, err)
	}
	got := storedSamples(t, pool)
	want := []store.OccupancySample{sample("100001", cutoff.UTC(), 0, 1, 0), sample("100002", at(9, 0, 0, 0).UTC(), 0, 0, 1)}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("samples kept = %+v, want %+v", got, want)
	}
	if n, err := s.DeleteSamplesBefore(ctx(), cutoff); err != nil || n != 0 {
		t.Errorf("second delete = %d, %v; want 0, nil", n, err)
	}
}
