package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/testdb"
)

func siteActivity(t *testing.T, s *store.Store, postals ...string) map[string]store.SiteActivity {
	t.Helper()
	got, err := s.SiteActivity(ctx(), postals)
	if err != nil {
		t.Fatalf("SiteActivity: %v", err)
	}
	return got
}

func sessionIDs(items []store.SessionWithReadings) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.Session.ID
	}
	return ids
}

// LOC-6, NAV-2: a site's activity holds exactly the sessions the site filter
// lists: every mirror session at a charger with that postal code, oldest
// first, with its readings. API sessions are left out, and there is no time
// filter, so a session from before the site's first sample counts.
func TestStore_LOC6_SiteActivityMatchesNAV2(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{
		siteCharger("SG-A", "100001", "Mall A", "Testco", "0.50", "kWh", locObs),
		siteCharger("SG-B", "100002", "Mall B", "Testco", "0.50", "kWh", locObs),
	}, []store.OccupancySample{sample("100001", locSince.Add(5*time.Hour), 1, 1, 0)})

	early := locSession(t, s, sessions.SourceMirror, "SG-A", locSince.Add(time.Hour), 0) // before the first sample
	locStop(t, s, sessions.SourceMirror, early, locSince.Add(2*time.Hour), 1_000)
	api := locSession(t, s, sessions.SourceAPI, "SG-A", locSince.Add(2*time.Hour), 0)
	locStop(t, s, sessions.SourceAPI, api, locSince.Add(3*time.Hour), 9_999)
	open := locSession(t, s, sessions.SourceMirror, "SG-A", locSince.Add(6*time.Hour), 0)
	locReading(t, s, sessions.SourceMirror, open, locSince.Add(6*time.Hour+5*time.Minute), 200)
	other := locSession(t, s, sessions.SourceMirror, "SG-B", locSince.Add(time.Hour), 0)
	locStop(t, s, sessions.SourceMirror, other, locSince.Add(2*time.Hour), 500)

	got := siteActivity(t, s, "100001", "100002")
	a, b := got["100001"], got["100002"]
	if ids := sessionIDs(a.Sessions); len(ids) != 2 || ids[0] != early || ids[1] != open {
		t.Fatalf("site A sessions = %v, want [%s %s]", ids, early, open)
	}
	if n := len(a.Sessions[1].Readings); n != 1 {
		t.Errorf("open session readings = %d, want 1", n)
	}
	if a.Sessions[0].Charger == nil || a.Sessions[0].Charger.PostalCode != "100001" {
		t.Errorf("site A session charger = %+v, want the 100001 row", a.Sessions[0].Charger)
	}
	if ids := sessionIDs(b.Sessions); len(ids) != 1 || ids[0] != other {
		t.Errorf("site B sessions = %v, want [%s]", ids, other)
	}
}

// LOC-6, NAV-2: a charger that has left the newest observation keeps its
// chargers row, so its sessions still count at its site, as the site filter
// lists them.
func TestStore_LOC6_SiteActivityOutOfScopeCharger(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{siteCharger("SG-OLD", "100001", "Mall", "Testco", "0.50", "kWh", locSince)}, nil)
	applyObservation(t, s, []store.ChargerRow{siteCharger("SG-NEW", "100001", "Mall", "Testco", "0.50", "kWh", locObs)}, nil)
	id := locSession(t, s, sessions.SourceMirror, "SG-OLD", locSince.Add(time.Hour), 0)

	a := siteActivity(t, s, "100001")["100001"]
	if ids := sessionIDs(a.Sessions); len(ids) != 1 || ids[0] != id {
		t.Errorf("sessions = %v, want [%s] from the out-of-scope charger", ids, id)
	}
}

// LOC-8: FirstSample is the site's earliest stored sample, in UTC. A site with
// sessions but no sample has a zero FirstSample and keeps its sessions, and a
// postal code with nothing stored maps to an empty activity.
func TestStore_LOC8_FirstSample(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	applyObservation(t, s, []store.ChargerRow{
		siteCharger("SG-A", "100001", "Mall A", "Testco", "0.50", "kWh", locObs),
		siteCharger("SG-B", "100002", "Mall B", "Testco", "0.50", "kWh", locObs),
	}, []store.OccupancySample{
		sample("100001", locSince.Add(2*time.Hour), 1, 1, 0),
		sample("100001", locSince.Add(time.Hour), 1, 1, 0),
	})
	id := locSession(t, s, sessions.SourceMirror, "SG-B", locSince.Add(time.Hour), 0)

	got := siteActivity(t, s, "100001", "100002", "100003")
	if a := got["100001"]; !a.FirstSample.Equal(locSince.Add(time.Hour)) || a.FirstSample.Location() != time.UTC {
		t.Errorf("site A FirstSample = %v, want %v in UTC", a.FirstSample, locSince.Add(time.Hour))
	}
	if b := got["100002"]; !b.FirstSample.IsZero() || len(b.Sessions) != 1 || b.Sessions[0].Session.ID != id {
		t.Errorf("site B = %+v, want zero FirstSample and session %s", b, id)
	}
	if c, ok := got["100003"]; !ok || !c.FirstSample.IsZero() || len(c.Sessions) != 0 {
		t.Errorf("unknown site = %+v (present %v), want an empty activity", c, ok)
	}
}

// LOC-6: a failed query is a plain error that names SiteActivity.
func TestStore_LOC6_SiteActivityFailures(t *testing.T) {
	t.Parallel()
	for _, drop := range []string{"", "occupancy_samples", "meter_readings"} {
		t.Run("drop "+drop, func(t *testing.T) {
			t.Parallel()
			pool := testdb.New(t)
			s := store.New(pool)
			applyObservation(t, s, []store.ChargerRow{siteCharger("SG-1", "100001", "Mall", "Testco", "0.50", "kWh", locObs)}, nil)
			locSession(t, s, sessions.SourceMirror, "SG-1", locSince.Add(time.Hour), 0)
			c := ctx()
			if drop == "" {
				cctx, cancel := context.WithCancel(ctx())
				cancel()
				c = cctx
			} else if _, err := pool.Exec(ctx(), "DROP TABLE "+drop); err != nil {
				t.Fatal(err)
			}
			_, err := s.SiteActivity(c, []string{"100001"})
			requirePlainError(t, "SiteActivity", err)
			if err != nil && !strings.Contains(err.Error(), "site activity") {
				t.Errorf("err = %v, want it to name site activity", err)
			}
		})
	}
}
