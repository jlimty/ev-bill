package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/locations"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/testdb"
)

// Location tests build every time as an explicit UTC instant, so they mean the
// same thing under any TZ (make test-tz).
var (
	locSince = time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	locObs   = locSince.Add(20 * time.Hour) // the newest charger observation
	locNow   = locSince.Add(24 * time.Hour) // the clock for session writes
)

// siteCharger is a charger at a site with the given operator and price.
func siteCharger(id, postal, name, operator, price, priceType string, observed time.Time) store.ChargerRow {
	r := chargerRow(id, datamall.Available, false, observed)
	r.PostalCode, r.LocationName, r.Operator = postal, name, operator
	r.Price, r.PriceType = price, priceType
	return r
}

func applyObservation(t *testing.T, s *store.Store, rows []store.ChargerRow, samples []store.OccupancySample) {
	t.Helper()
	if err := s.ApplyObservation(ctx(), rows, samples); err != nil {
		t.Fatalf("ApplyObservation: %v", err)
	}
}

func locSession(t *testing.T, s *store.Store, as sessions.Source, charger string, start time.Time, wh int64) string {
	t.Helper()
	sess, err := s.CreateSession(ctx(), as, charger, start, wh, locNow)
	if err != nil {
		t.Fatalf("CreateSession(%s, %s): %v", as, charger, err)
	}
	return sess.ID
}

func locReading(t *testing.T, s *store.Store, as sessions.Source, id string, at time.Time, wh int64) {
	t.Helper()
	if _, _, _, err := s.AddReading(ctx(), as, id, billing.Reading{At: at, MeterWh: wh}, locNow); err != nil {
		t.Fatalf("AddReading(%s): %v", id, err)
	}
}

func locStop(t *testing.T, s *store.Store, as sessions.Source, id string, at time.Time, wh int64) {
	t.Helper()
	if _, _, _, err := s.StopSession(ctx(), as, id, billing.Reading{At: at, MeterWh: wh}, locNow); err != nil {
		t.Fatalf("StopSession(%s): %v", id, err)
	}
}

func listLocations(t *testing.T, s *store.Store) []store.LocationAggregate {
	t.Helper()
	got, err := s.Locations(ctx(), locSince)
	if err != nil {
		t.Fatalf("Locations: %v", err)
	}
	return got
}

// onlySite returns the single aggregate, failing unless there is exactly one
// and it has the postal code.
func onlySite(t *testing.T, got []store.LocationAggregate, postal string) store.LocationAggregate {
	t.Helper()
	if len(got) != 1 || got[0].PostalCode != postal {
		t.Fatalf("Locations = %+v, want one site %s", got, postal)
	}
	return got[0]
}

// LOC-2: only samples observed at or after since count, in the totals and in
// the hours. FirstSample is the earliest sample inside the window.
func TestStore_LOC2_SumsInWindow(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{siteCharger("SG-1", "100001", "Mall", "Testco", "0.50", "kWh", locObs)},
		[]store.OccupancySample{
			sample("100001", locSince.Add(-5*time.Minute), 9, 9, 9), // outside
			sample("100001", locSince, 1, 3, 0),                     // at since: inside
			sample("100001", locSince.Add(2*time.Hour), 2, 2, 1),    // another SGT hour
		})

	a := onlySite(t, listLocations(t, s), "100001")
	if a.Samples != 2 || a.Occupied != 3 || a.Available != 5 {
		t.Errorf("totals = %d samples, %d occupied, %d available; want 2, 3, 5", a.Samples, a.Occupied, a.Available)
	}
	if !a.FirstSample.Equal(locSince) || a.FirstSample.Location() != time.UTC {
		t.Errorf("FirstSample = %v, want %v in UTC", a.FirstSample, locSince)
	}
	var hours store.HourAggregate
	for _, h := range a.Hours {
		hours.Samples += h.Samples
		hours.Occupied += h.Occupied
		hours.Available += h.Available
	}
	if hours != (store.HourAggregate{Samples: 2, Occupied: 3, Available: 5}) {
		t.Errorf("sum of Hours = %+v, want the window's totals", hours)
	}
}

// LOC-2: out-of-service connectors are not capacity. A site whose every
// sample is out of service has Occupied = Available = 0, which Task 1 shows
// as n/a; the samples still count as observed time.
func TestStore_LOC2_AllOutOfServiceIsNA(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{siteCharger("SG-1", "100001", "Mall", "Testco", "0.50", "kWh", locObs)},
		[]store.OccupancySample{
			sample("100001", locSince.Add(time.Hour), 0, 0, 5),
			sample("100001", locSince.Add(2*time.Hour), 0, 0, 5),
		})

	a := onlySite(t, listLocations(t, s), "100001")
	if a.Samples != 2 || a.Occupied != 0 || a.Available != 0 {
		t.Fatalf("totals = %d samples, %d occupied, %d available; want 2, 0, 0", a.Samples, a.Occupied, a.Available)
	}
	if _, ok := locations.UtilizationBP(a.Occupied, a.Available); ok {
		t.Error("UtilizationBP ok = true, want n/a")
	}
}

// LOC-3: hours are Asia/Singapore hours, whatever the process TZ and whatever
// the database session's TimeZone (the test database may itself default to
// Asia/Singapore, which would hide a missing AT TIME ZONE). 15:30Z is 23:30
// SGT (hour 23, not hour 15) and 23:10Z is 07:10 SGT the next day.
func TestStore_LOC3_HourInSingapore(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	applyObservation(t, store.New(pool), []store.ChargerRow{siteCharger("SG-1", "100001", "Mall", "Testco", "0.50", "kWh", locObs)},
		[]store.OccupancySample{
			sample("100001", time.Date(2026, 11, 8, 15, 30, 0, 0, time.UTC), 1, 2, 0),
			sample("100001", time.Date(2026, 11, 8, 23, 10, 0, 0, time.UTC), 3, 0, 4),
		})

	for _, zone := range []string{"UTC", "America/New_York"} {
		t.Run(zone, func(t *testing.T) {
			cfg := pool.Config().Copy()
			cfg.ConnConfig.RuntimeParams["timezone"] = zone
			zoned, err := pgxpool.NewWithConfig(ctx(), cfg)
			if err != nil {
				t.Fatalf("pool with TimeZone %s: %v", zone, err)
			}
			t.Cleanup(zoned.Close)

			a := onlySite(t, listLocations(t, store.New(zoned)), "100001")
			if got := a.Hours[23]; got != (store.HourAggregate{Samples: 1, Occupied: 1, Available: 2}) {
				t.Errorf("Hours[23] = %+v, want the 23:30 SGT sample", got)
			}
			if got := a.Hours[7]; got != (store.HourAggregate{Samples: 1, Occupied: 3, Available: 0}) {
				t.Errorf("Hours[7] = %+v, want the 07:10 SGT sample", got)
			}
			for h, got := range a.Hours {
				if h != 7 && h != 23 && got.Samples != 0 {
					t.Errorf("Hours[%d].Samples = %d, want 0 (no data)", h, got.Samples)
				}
			}
		})
	}
}

// LOC-5: a postal code with samples but no charger in the newest observation
// gets no aggregate, even though its charger row is still in the table.
func TestStore_LOC5_RemovedSiteNoCard(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{siteCharger("OLD-1", "111111", "Gone", "Testco", "0.50", "kWh", locObs.Add(-time.Hour))},
		[]store.OccupancySample{sample("111111", locSince.Add(time.Hour), 1, 1, 0)})
	applyObservation(t, s, []store.ChargerRow{siteCharger("SG-1", "100001", "Mall", "Testco", "0.50", "kWh", locObs)},
		[]store.OccupancySample{sample("111111", locSince.Add(2*time.Hour), 1, 1, 0)})

	a := onlySite(t, listLocations(t, s), "100001")
	if a.Samples != 0 || !a.FirstSample.IsZero() {
		t.Errorf("site without samples: Samples = %d, FirstSample = %v; want 0 and the zero time", a.Samples, a.FirstSample)
	}
}

// LOC-5: with no charger stored there is no scope: an empty, non-nil slice and
// no error, even when samples exist.
func TestStore_LOC5_EmptyScope(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	got := listLocations(t, s)
	if got == nil || len(got) != 0 {
		t.Fatalf("Locations on an empty table = %#v, want an empty, non-nil slice", got)
	}
	applyObservation(t, s, nil, []store.OccupancySample{sample("100001", locSince.Add(time.Hour), 1, 1, 0)})
	if got := listLocations(t, s); got == nil || len(got) != 0 {
		t.Fatalf("Locations with samples but no chargers = %#v, want an empty, non-nil slice", got)
	}
}

// LOC-6: billing sessions, energy and value per site. Energy is the stop meter,
// else the highest reading, else the start meter, minus the start meter. Value
// is round-half-up(Wh × price ÷ 100,000) per session at the charger's current
// price. API sessions and sessions started before since are left out.
func TestStore_LOC6_EnergyAndValue(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{
		siteCharger("SG-A", "100001", "Mall A", "Testco", "0.6976", "kWh", locObs),
		siteCharger("SG-B", "100002", "Mall B", "Testco", "0.70", "kWh", locObs),
		siteCharger("SG-C", "100002", "Mall B", "Testco", "0.70", "kWh", locObs),
	}, []store.OccupancySample{ // both sites watched from since onwards
		sample("100001", locSince, 1, 1, 0),
		sample("100002", locSince, 1, 1, 0),
	})

	// Before since: left out.
	early := locSession(t, s, sessions.SourceMirror, "SG-A", locSince.Add(-2*time.Hour), 0)
	locStop(t, s, sessions.SourceMirror, early, locSince.Add(-time.Hour), 5_000)
	// Started exactly at since, so inside. Stopped, 0 -> 1,000 Wh at
	// 69.76 c/kWh: 69.76 c rounds to 70 c.
	stopped := locSession(t, s, sessions.SourceMirror, "SG-A", locSince, 0)
	locReading(t, s, sessions.SourceMirror, stopped, locSince.Add(90*time.Minute), 400)
	locStop(t, s, sessions.SourceMirror, stopped, locSince.Add(2*time.Hour), 1_000)
	// API session on SG-B: left out.
	api := locSession(t, s, sessions.SourceAPI, "SG-B", locSince.Add(time.Hour), 0)
	locStop(t, s, sessions.SourceAPI, api, locSince.Add(2*time.Hour), 9_999)
	// Open, last reading 500 Wh at 70 c/kWh: 35 c.
	open := locSession(t, s, sessions.SourceMirror, "SG-B", locSince.Add(3*time.Hour), 0)
	locReading(t, s, sessions.SourceMirror, open, locSince.Add(3*time.Hour+5*time.Minute), 200)
	locReading(t, s, sessions.SourceMirror, open, locSince.Add(3*time.Hour+10*time.Minute), 500)
	// Open with no readings and a non-zero start: 0 Wh, 0 c.
	locSession(t, s, sessions.SourceMirror, "SG-C", locSince.Add(4*time.Hour), 300)

	got := listLocations(t, s)
	if len(got) != 2 {
		t.Fatalf("Locations = %+v, want two sites", got)
	}
	a, b := got[0], got[1]
	if a.PostalCode != "100001" || a.Sessions != 1 || a.EnergyWh != 1_000 || a.ValueCents != 70 || a.PriceNASessions != 0 {
		t.Errorf("site A = %s: %d sessions, %d Wh, %d c, %d n/a; want 100001: 1, 1000, 70, 0",
			a.PostalCode, a.Sessions, a.EnergyWh, a.ValueCents, a.PriceNASessions)
	}
	if b.PostalCode != "100002" || b.Sessions != 2 || b.EnergyWh != 500 || b.ValueCents != 35 || b.PriceNASessions != 0 {
		t.Errorf("site B = %s: %d sessions, %d Wh, %d c, %d n/a; want 100002: 2, 500, 35, 0",
			b.PostalCode, b.Sessions, b.EnergyWh, b.ValueCents, b.PriceNASessions)
	}
}

// LOC-6: a session on a charger whose price is unknown counts as a billing
// session and in the energy, but not in the value, and is noted as price n/a.
// A free charger's session is valued at 0 and is not n/a.
func TestStore_LOC6_PriceNACounted(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{
		siteCharger("SG-1", "100001", "Mall", "Testco", "0.50", "", locObs),
		siteCharger("SG-2", "100001", "Mall", "Testco", "", "free", locObs),
	}, []store.OccupancySample{sample("100001", locSince, 1, 1, 0)})
	na := locSession(t, s, sessions.SourceMirror, "SG-1", locSince.Add(time.Hour), 0)
	locStop(t, s, sessions.SourceMirror, na, locSince.Add(2*time.Hour), 800)
	free := locSession(t, s, sessions.SourceMirror, "SG-2", locSince.Add(time.Hour), 100)
	locStop(t, s, sessions.SourceMirror, free, locSince.Add(2*time.Hour), 300)

	a := onlySite(t, listLocations(t, s), "100001")
	if a.Sessions != 2 || a.EnergyWh != 1_000 || a.ValueCents != 0 || a.PriceNASessions != 1 {
		t.Errorf("site = %d sessions, %d Wh, %d c, %d n/a; want 2, 1000, 0, 1",
			a.Sessions, a.EnergyWh, a.ValueCents, a.PriceNASessions)
	}
}

// LOC-6 "over the same window", LOC-2: a site's sessions cover the same window
// as its utilization, which starts at its first sample when that is later than
// since. A session started before the first sample is left out, one started at
// exactly the first sample counts, and a site with no sample has no sessions,
// energy or value.
func TestStore_LOC6_SessionsInSiteWindow(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	first := locSince.Add(2 * time.Hour)
	applyObservation(t, s, []store.ChargerRow{
		siteCharger("SG-A1", "100001", "Watched", "Testco", "0.50", "kWh", locObs),
		siteCharger("SG-A2", "100001", "Watched", "Testco", "0.50", "kWh", locObs),
		siteCharger("SG-A3", "100001", "Watched", "Testco", "0.50", "kWh", locObs),
		siteCharger("SG-B1", "100002", "Unwatched", "Testco", "0.50", "kWh", locObs),
	}, []store.OccupancySample{sample("100001", first, 1, 1, 0)})

	before := locSession(t, s, sessions.SourceMirror, "SG-A1", first.Add(-time.Minute), 0)
	locStop(t, s, sessions.SourceMirror, before, first.Add(time.Hour), 4_000)
	atFirst := locSession(t, s, sessions.SourceMirror, "SG-A2", first, 0)
	locStop(t, s, sessions.SourceMirror, atFirst, first.Add(time.Hour), 1_000) // 50 c
	after := locSession(t, s, sessions.SourceMirror, "SG-A3", first.Add(time.Hour), 0)
	locStop(t, s, sessions.SourceMirror, after, first.Add(2*time.Hour), 600) // 30 c
	unwatched := locSession(t, s, sessions.SourceMirror, "SG-B1", first, 0)
	locStop(t, s, sessions.SourceMirror, unwatched, first.Add(time.Hour), 2_000)

	got := listLocations(t, s)
	if len(got) != 2 {
		t.Fatalf("Locations = %+v, want two sites", got)
	}
	a, b := got[0], got[1]
	if !a.FirstSample.Equal(first) {
		t.Fatalf("FirstSample = %v, want %v", a.FirstSample, first)
	}
	if a.Sessions != 2 || a.EnergyWh != 1_600 || a.ValueCents != 80 || a.PriceNASessions != 0 {
		t.Errorf("watched site = %d sessions, %d Wh, %d c, %d n/a; want 2, 1600, 80, 0 (the session before the first sample left out)",
			a.Sessions, a.EnergyWh, a.ValueCents, a.PriceNASessions)
	}
	if b.Samples != 0 || b.Sessions != 0 || b.EnergyWh != 0 || b.ValueCents != 0 || b.PriceNASessions != 0 {
		t.Errorf("site with no sample = %d samples, %d sessions, %d Wh, %d c, %d n/a; want all 0",
			b.Samples, b.Sessions, b.EnergyWh, b.ValueCents, b.PriceNASessions)
	}
}

// LOC-5: chargers sort by charger_id in byte order, as Go compares strings,
// whatever the database's collation, so the cell order agrees with Go. In byte order "-" (0x2D)
// comes before "1" (0x31); en_US.UTF-8 under glibc ignores the hyphen and
// would put AB1 first.
func TestStore_LOC5_ChargerIDByteOrder(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{
		siteCharger("AB1", "100001", "One Name", "Testco", "0.50", "kWh", locObs),
		siteCharger("AB-2", "100001", "Dash Name", "Testco", "0.50", "kWh", locObs),
	}, nil)

	a := onlySite(t, listLocations(t, s), "100001")
	ids := make([]string, len(a.Chargers))
	for i, c := range a.Chargers {
		ids[i] = c.ChargerID
	}
	if want := []string{"AB-2", "AB1"}; !slices.Equal(ids, want) || !slices.IsSorted(ids) {
		t.Errorf("charger order = %v, want %v (byte order)", ids, want)
	}
	if a.Name != "Dash Name" {
		t.Errorf("Name = %q, want Dash Name (a one-one tie goes to the name that sorts first)", a.Name)
	}
}

// LOC-5: each site lists the chargers of the newest observation sorted by
// charger_id and its operators distinct and sorted. Sites sort by postal code. A charger row from an older
// observation at the same postal code is not in scope.
func TestStore_LOC5_ChargersAndOperators(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{siteCharger("SG-0", "100002", "Old Name", "Oldco", "0.50", "kWh", locObs.Add(-time.Hour))}, nil)
	rows := []store.ChargerRow{
		siteCharger("SG-3", "100002", "Plaza East", "Zeta", "0.50", "kWh", locObs),
		siteCharger("SG-1", "100002", "Plaza", "Zeta", "0.50", "kWh", locObs),
		siteCharger("SG-2", "100002", "Plaza West", "Alpha", "0.50", "kWh", locObs),
		siteCharger("AA-9", "100001", "Arcade", "Beta", "0.50", "kWh", locObs),
	}
	applyObservation(t, s, rows, nil)

	got := listLocations(t, s)
	if len(got) != 2 || got[0].PostalCode != "100001" || got[1].PostalCode != "100002" {
		t.Fatalf("Locations = %+v, want sites 100001 then 100002", got)
	}
	if got[0].Name != "Arcade" || !slices.Equal(got[0].Operators, []string{"Beta"}) {
		t.Errorf("site 100001 = %q, %v; want Arcade, [Beta]", got[0].Name, got[0].Operators)
	}
	p := got[1]
	if p.Name != "Plaza" {
		t.Errorf("Name = %q, want Plaza (three names tie, so the one that sorts first)", p.Name)
	}
	if !slices.Equal(p.Operators, []string{"Alpha", "Zeta"}) {
		t.Errorf("Operators = %v, want [Alpha Zeta]", p.Operators)
	}
	want := []store.ChargerRow{rows[1], rows[2], rows[0]}
	if len(p.Chargers) != len(want) {
		t.Fatalf("Chargers = %+v, want SG-1, SG-2, SG-3", p.Chargers)
	}
	for i := range want {
		want[i].ObservedAt = want[i].ObservedAt.UTC()
		if p.Chargers[i] != want[i] {
			t.Errorf("Chargers[%d] = %+v, want %+v", i, p.Chargers[i], want[i])
		}
	}
}

// VIEW-5: a site's name is the location name held by the most connectors in
// the newest observation, ties going to the name that sorts first, and the
// other names are its Also. It does not follow the lowest charger_id: the name
// stays when that connector drops out of the next observation.
func TestStore_VIEW5_MajorityName(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{
		siteCharger("SG-1", "100001", "A", "Testco", "0.50", "kWh", locObs),
		siteCharger("SG-2", "100001", "B", "Testco", "0.50", "kWh", locObs),
		siteCharger("SG-3", "100001", "B", "Testco", "0.50", "kWh", locObs),
	}, nil)
	a := onlySite(t, listLocations(t, s), "100001")
	if a.Name != "B" || !slices.Equal(a.Also, []string{"A"}) {
		t.Errorf("Name, Also = %q, %v; want B, [A]", a.Name, a.Also)
	}

	// SG-1, the lowest charger_id, leaves the feed.
	applyObservation(t, s, []store.ChargerRow{
		siteCharger("SG-2", "100001", "B", "Testco", "0.50", "kWh", locObs.Add(time.Minute)),
		siteCharger("SG-3", "100001", "B", "Testco", "0.50", "kWh", locObs.Add(time.Minute)),
	}, nil)
	a = onlySite(t, listLocations(t, s), "100001")
	if a.Name != "B" || a.Also == nil || len(a.Also) != 0 {
		t.Errorf("after SG-1 left: Name, Also = %q, %v; want B, empty", a.Name, a.Also)
	}
}

// API-2: when the database fails at any of the three queries, Locations
// returns a plain error.
func TestStore_LOC_LocationsFailures(t *testing.T) {
	t.Parallel()
	seed := func(t *testing.T, s *store.Store) {
		t.Helper()
		applyObservation(t, s, []store.ChargerRow{siteCharger("SG-1", "100001", "Mall", "Testco", "0.50", "kWh", locObs)},
			[]store.OccupancySample{sample("100001", locSince.Add(time.Hour), 1, 1, 0)})
		locSession(t, s, sessions.SourceMirror, "SG-1", locSince.Add(time.Hour), 0)
	}
	for _, tc := range []struct{ name, drop string }{
		{"cancelled context", ""},
		{"no samples table", "occupancy_samples"},
		{"no readings table", "meter_readings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testdb.New(t)
			s := store.New(pool)
			seed(t, s)
			c := ctx()
			if tc.drop == "" {
				cctx, cancel := context.WithCancel(ctx())
				cancel()
				c = cctx
			} else if _, err := pool.Exec(ctx(), "DROP TABLE "+tc.drop); err != nil {
				t.Fatal(err)
			}
			_, err := s.Locations(c, locSince)
			requirePlainError(t, "Locations", err)
		})
	}
}
