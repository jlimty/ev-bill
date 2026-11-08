package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/testdb"
)

// listBase is the first start time used by the list tests.
var listBase = locSince.Add(-24 * time.Hour)

func listPage(t *testing.T, s *store.Store, q store.ListQuery) store.ListPage {
	t.Helper()
	page, err := s.ListSessions(ctx(), q)
	if err != nil {
		t.Fatalf("ListSessions(%+v): %v", q, err)
	}
	return page
}

func pageIDs(p store.ListPage) []string {
	ids := make([]string, len(p.Items))
	for i, it := range p.Items {
		ids[i] = it.Session.ID
	}
	return ids
}

// seedStopped adds one stopped mirror session at charger, started at start
// with the given energy.
func seedStopped(t *testing.T, s *store.Store, charger string, start time.Time, wh int64) string {
	t.Helper()
	id := locSession(t, s, sessions.SourceMirror, charger, start, 0)
	locStop(t, s, sessions.SourceMirror, id, start.Add(time.Minute), wh)
	return id
}

// seedSite stores one charger at a postal code observed at locObs.
func seedSite(t *testing.T, s *store.Store, charger, postal string) {
	t.Helper()
	applyObservation(t, s, []store.ChargerRow{siteCharger(charger, postal, "Mall "+postal, "Testco", "0.50", "kWh", locObs)}, nil)
}

// NAV-3: the SQL energy is the energy billing prices, for every shape of
// session. Each session's billed energy, used as an energy cursor, must return
// exactly the rows after it, which holds only when the SQL energy equals it.
func TestStore_NAV3_EnergyMatchesBilling(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	m := sessions.SourceMirror
	start := listBase

	active := locSession(t, s, m, "A-1", start, 4000)
	locReading(t, s, m, active, start.Add(10*time.Minute), 4700)
	locReading(t, s, m, active, start.Add(20*time.Minute), 5200)

	stopped := locSession(t, s, m, "B-1", start.Add(time.Minute), 500)
	locStop(t, s, m, stopped, start.Add(31*time.Minute), 3500)

	none := locSession(t, s, m, "C-1", start.Add(2*time.Minute), 900)

	// Readings stored later than their timestamps: the later instant goes in first.
	outOfOrder := locSession(t, s, m, "D-1", start.Add(3*time.Minute), 50)
	locReading(t, s, m, outOfOrder, start.Add(33*time.Minute), 2050)
	locReading(t, s, m, outOfOrder, start.Add(13*time.Minute), 800)
	locReading(t, s, m, outOfOrder, start.Add(23*time.Minute), 1500)

	want := map[string]int64{stopped: 3000, outOfOrder: 2000, active: 1200, none: 0}
	order := []string{stopped, outOfOrder, active, none}
	page := listPage(t, s, store.ListQuery{Sort: store.SortEnergy, Desc: true, Limit: 10})
	if got := pageIDs(page); !slices.Equal(got, order) {
		t.Fatalf("energy order = %v, want %v", got, order)
	}
	for i, it := range page.Items {
		view, err := sessions.BuildView(it.Session, it.Readings, billing.SingaporeTariff(), locNow)
		if err != nil {
			t.Fatalf("BuildView(%s): %v", it.Session.ID, err)
		}
		if view.Bill.EnergyWh != want[it.Session.ID] {
			t.Errorf("billing energy of %s = %d, want %d", it.Session.ID, view.Bill.EnergyWh, want[it.Session.ID])
		}
		after := listPage(t, s, store.ListQuery{
			Sort: store.SortEnergy, Desc: true, Limit: 10,
			After: &store.ListCursor{EnergyWh: view.Bill.EnergyWh, ID: it.Session.ID},
		})
		if got := pageIDs(after); !slices.Equal(got, order[i+1:]) {
			t.Errorf("after billed energy %d of %s: got %v, want %v", view.Bill.EnergyWh, it.Session.ID, got, order[i+1:])
		}
	}
}

// NAV-3: both sorts, both directions, ties broken on id in the same direction.
func TestStore_NAV3_SortAndTieBreak(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedSite(t, s, "SG-1", "100001")
	// Energies 5, 5, 9, 1 with distinct starts; the two 5s tie on energy.
	a := seedStopped(t, s, "SG-1", listBase, 5)
	b := seedStopped(t, s, "SG-1", listBase.Add(time.Hour), 5)
	c := seedStopped(t, s, "SG-1", listBase.Add(2*time.Hour), 9)
	d := seedStopped(t, s, "SG-1", listBase.Add(3*time.Hour), 1)
	lo, hi := a, b
	if lo > hi {
		lo, hi = hi, lo
	}

	tests := []struct {
		name string
		q    store.ListQuery
		want []string
	}{
		{"started desc", store.ListQuery{Sort: store.SortStarted, Desc: true}, []string{d, c, b, a}},
		{"started asc", store.ListQuery{Sort: store.SortStarted}, []string{a, b, c, d}},
		{"energy desc", store.ListQuery{Sort: store.SortEnergy, Desc: true}, []string{c, hi, lo, d}},
		{"energy asc", store.ListQuery{Sort: store.SortEnergy}, []string{d, lo, hi, c}},
	}
	for _, tt := range tests {
		tt.q.Limit = 10
		if got := pageIDs(listPage(t, s, tt.q)); !slices.Equal(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

// NAV-2: the filter covers every mirror session at the site, however old its
// charger's row, and nothing else.
func TestStore_NAV2_SiteFilter(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	// SG-OLD's row is from an older observation than SG-NEW's.
	applyObservation(t, s, []store.ChargerRow{
		siteCharger("SG-OLD", "100001", "Mall", "Testco", "0.50", "kWh", locObs.Add(-time.Hour)),
		siteCharger("SG-OTHER", "200002", "Other", "Testco", "0.50", "kWh", locObs.Add(-time.Hour)),
	}, nil)
	applyObservation(t, s, []store.ChargerRow{
		siteCharger("SG-NEW", "100001", "Mall", "Testco", "0.50", "kWh", locObs),
	}, nil)
	old := seedStopped(t, s, "SG-OLD", listBase, 10)
	cur := seedStopped(t, s, "SG-NEW", listBase.Add(time.Hour), 20)
	seedStopped(t, s, "SG-OTHER", listBase.Add(2*time.Hour), 30)
	// An api session reusing a mirrored charger ID.
	if _, err := s.CreateSession(ctx(), sessions.SourceAPI, "SG-NEW", listBase.Add(3*time.Hour), 0, locNow); err != nil {
		t.Fatalf("CreateSession(api): %v", err)
	}

	page := listPage(t, s, store.ListQuery{Site: "100001", Sort: store.SortStarted, Desc: true, Limit: 10})
	if got, want := pageIDs(page), []string{cur, old}; !slices.Equal(got, want) {
		t.Errorf("site items = %v, want %v", got, want)
	}
	if page.Total != 2 {
		t.Errorf("Total = %d, want 2", page.Total)
	}
	all := listPage(t, s, store.ListQuery{Sort: store.SortStarted, Desc: true, Limit: 10})
	if all.Total != 4 || len(all.Items) != 4 {
		t.Errorf("unfiltered Total = %d, items = %d, want 4 and 4", all.Total, len(all.Items))
	}
	for _, it := range page.Items {
		if it.Charger == nil {
			t.Errorf("session %s has no chargers row", it.Session.ID)
		}
	}
	none := listPage(t, s, store.ListQuery{Site: "999999", Limit: 10})
	if len(none.Items) != 0 || none.Total != 0 || none.HasNext || none.HasPrev {
		t.Errorf("absent site page = %+v, want empty", none)
	}
}

// seedMany stores n stopped sessions with starts two minutes apart.
func seedMany(t *testing.T, s *store.Store, n int) {
	t.Helper()
	seedSite(t, s, "SG-1", "100001")
	for i := range n {
		seedStopped(t, s, "SG-1", listBase.Add(time.Duration(i)*2*time.Minute), int64(i%7))
	}
}

func cursorOf(it store.SessionWithReadings, wh int64) *store.ListCursor {
	return &store.ListCursor{StartedAt: it.Session.StartedAt, EnergyWh: wh, ID: it.Session.ID}
}

// NAV-4: forward through three pages and back gives the same rows, with the
// flags the paging semantics define.
func TestStore_NAV4_ForwardThenBack(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedMany(t, s, 120)
	base := store.ListQuery{Site: "100001", Sort: store.SortStarted, Desc: true, Limit: 50}

	p1 := listPage(t, s, base)
	q2 := base
	q2.After = cursorOf(p1.Items[len(p1.Items)-1], 0)
	p2 := listPage(t, s, q2)
	q3 := base
	q3.After = cursorOf(p2.Items[len(p2.Items)-1], 0)
	p3 := listPage(t, s, q3)

	q2b := base
	q2b.Before = cursorOf(p3.Items[0], 0)
	b2 := listPage(t, s, q2b)
	q1b := base
	q1b.Before = cursorOf(b2.Items[0], 0)
	b1 := listPage(t, s, q1b)

	type flags struct{ prev, next bool }
	checks := []struct {
		name string
		page store.ListPage
		n    int
		want flags
	}{
		{"page 1", p1, 50, flags{false, true}},
		{"page 2", p2, 50, flags{true, true}},
		{"page 3", p3, 20, flags{true, false}},
		{"back to 2", b2, 50, flags{true, true}},
		{"back to 1", b1, 50, flags{false, true}},
	}
	for _, c := range checks {
		if len(c.page.Items) != c.n {
			t.Errorf("%s: %d items, want %d", c.name, len(c.page.Items), c.n)
		}
		if got := (flags{c.page.HasPrev, c.page.HasNext}); got != c.want {
			t.Errorf("%s: flags = %+v, want %+v", c.name, got, c.want)
		}
		if c.page.Total != 120 {
			t.Errorf("%s: Total = %d, want 120", c.name, c.page.Total)
		}
	}
	if !slices.Equal(pageIDs(b2), pageIDs(p2)) {
		t.Errorf("back to 2 = %v, want %v", pageIDs(b2), pageIDs(p2))
	}
	if !slices.Equal(pageIDs(b1), pageIDs(p1)) {
		t.Errorf("back to 1 = %v, want %v", pageIDs(b1), pageIDs(p1))
	}
}

// NAV-4: paging forward and back works on the energy sort and on ascending
// order: the pages are disjoint and complete, and paging back from the end
// returns the same pages down to HasPrev=false.
func TestStore_NAV4_EnergyAscendingPages(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedMany(t, s, 30)
	energy := func(it store.SessionWithReadings) int64 { return *it.Session.MeterStopWh - it.Session.MeterStartWh }
	for _, q := range []store.ListQuery{
		{Site: "100001", Sort: store.SortEnergy, Limit: 7},
		{Site: "100001", Sort: store.SortEnergy, Desc: true, Limit: 7},
		{Site: "100001", Sort: store.SortStarted, Limit: 7},
	} {
		full := q
		full.Limit = 30
		all := pageIDs(listPage(t, s, full))

		var pages []store.ListPage
		fwd := q
		for {
			p := listPage(t, s, fwd)
			pages = append(pages, p)
			if !p.HasNext {
				break
			}
			last := p.Items[len(p.Items)-1]
			fwd.After = &store.ListCursor{StartedAt: last.Session.StartedAt, EnergyWh: energy(last), ID: last.Session.ID}
		}
		var got []string
		for _, p := range pages {
			got = append(got, pageIDs(p)...)
		}
		if !slices.Equal(got, all) {
			t.Errorf("%+v: paged ids differ from the single page:\n got %v\nwant %v", q, got, all)
		}

		for i := len(pages) - 1; i > 0; i-- {
			first := pages[i].Items[0]
			back := q
			back.Before = &store.ListCursor{StartedAt: first.Session.StartedAt, EnergyWh: energy(first), ID: first.Session.ID}
			p := listPage(t, s, back)
			if !slices.Equal(pageIDs(p), pageIDs(pages[i-1])) {
				t.Errorf("%+v: back to page %d = %v, want %v", q, i, pageIDs(p), pageIDs(pages[i-1]))
			}
			if p.HasPrev != (i-1 > 0) || !p.HasNext {
				t.Errorf("%+v: back to page %d flags prev=%v next=%v", q, i, p.HasPrev, p.HasNext)
			}
		}
	}
}

// NAV-4: the cursor row may be deleted; the page after it is still right.
func TestStore_NAV4_CursorRowGone(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	s := store.New(pool)
	seedMany(t, s, 5)
	base := store.ListQuery{Sort: store.SortStarted, Desc: true, Limit: 10}
	full := listPage(t, s, base)
	cursor := full.Items[1]
	q := base
	q.After = cursorOf(cursor, 0)
	want := pageIDs(full)[2:]

	// Remove the cursor's own session (readings cascade).
	if _, err := pool.Exec(ctx(), `DELETE FROM sessions WHERE id = $1`, cursor.Session.ID); err != nil {
		t.Fatalf("delete cursor session: %v", err)
	}
	if got := pageIDs(listPage(t, s, q)); !slices.Equal(got, want) {
		t.Errorf("after deleted cursor = %v, want %v", got, want)
	}
}

// NAV-4: two sessions 1 microsecond apart split cleanly across a page boundary.
func TestStore_NAV4_MicrosecondCursor(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedSite(t, s, "SG-1", "100001")
	t0 := listBase.Add(time.Hour).Truncate(time.Second)
	first := seedStopped(t, s, "SG-1", t0, 1)
	second := seedStopped(t, s, "SG-1", t0.Add(time.Microsecond), 1)

	q := store.ListQuery{Sort: store.SortStarted, Desc: true, Limit: 1}
	p1 := listPage(t, s, q)
	if got := pageIDs(p1); !slices.Equal(got, []string{second}) || !p1.HasNext {
		t.Fatalf("page 1 = %v hasNext %v, want [%s] true", got, p1.HasNext, second)
	}
	q.After = cursorOf(p1.Items[0], 0)
	p2 := listPage(t, s, q)
	if got := pageIDs(p2); !slices.Equal(got, []string{first}) || p2.HasNext || !p2.HasPrev {
		t.Fatalf("page 2 = %v next %v prev %v, want [%s] false true", got, p2.HasNext, p2.HasPrev, first)
	}
}

// NAV-2: the site's name is the majority name at its newest observation, and
// an absent postal code is not found.
func TestStore_NAV2_SiteName(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	row := func(id, name string, observed time.Time) store.ChargerRow {
		return siteCharger(id, "100001", name, "Testco", "0.50", "kWh", observed)
	}
	// An older observation of the same site with a different majority name.
	applyObservation(t, s, []store.ChargerRow{
		row("SG-4", "Old Mall", locObs.Add(-time.Hour)), row("SG-5", "Old Mall", locObs.Add(-time.Hour)),
		row("SG-6", "Old Mall", locObs.Add(-time.Hour)),
	}, nil)
	applyObservation(t, s, []store.ChargerRow{
		row("SG-1", "Zeta Mall", locObs), row("SG-2", "Zeta Mall", locObs), row("SG-3", "Alpha Mall", locObs),
		siteCharger("SG-9", "300003", "Elsewhere", "Testco", "0.50", "kWh", locObs.Add(time.Hour)),
	}, nil)
	name, found, err := s.SiteName(ctx(), "100001")
	if err != nil || !found || name != "Zeta Mall" {
		t.Errorf("SiteName(100001) = %q, %v, %v; want Zeta Mall, true, nil", name, found, err)
	}
	// Only the site's own newest observation counts, not the global newest one.
	name, found, err = s.SiteName(ctx(), "300003")
	if err != nil || !found || name != "Elsewhere" {
		t.Errorf("SiteName(300003) = %q, %v, %v", name, found, err)
	}
	name, found, err = s.SiteName(ctx(), "999999")
	if err != nil || found || name != "" {
		t.Errorf("SiteName(999999) = %q, %v, %v; want empty, false, nil", name, found, err)
	}
}

// NAV-4: a page needs a positive limit, at most one cursor and a known sort.
func TestStore_ListSessions_BadQuery(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	cur := &store.ListCursor{StartedAt: listBase, ID: "00000000-0000-0000-0000-000000000000"}
	for name, q := range map[string]store.ListQuery{
		"limit 0":      {Limit: 0},
		"both cursors": {Limit: 5, After: cur, Before: cur},
		"unknown sort": {Limit: 5, Sort: store.SessionSort(99)},
	} {
		if _, err := s.ListSessions(ctx(), q); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// API-2: a failing database gives a plain error from both the list and the
// name lookup, at the count, the page and the readings queries.
func TestStore_ListSessions_Failures(t *testing.T) {
	t.Parallel()
	for _, drop := range []string{"", "chargers", "meter_readings"} {
		t.Run("drop "+drop, func(t *testing.T) {
			t.Parallel()
			pool := testdb.New(t)
			s := store.New(pool)
			seedMany(t, s, 2)
			c := ctx()
			if drop == "" {
				cctx, cancel := context.WithCancel(ctx())
				cancel()
				c = cctx
			} else if _, err := pool.Exec(ctx(), "DROP TABLE "+drop); err != nil {
				t.Fatal(err)
			}
			_, err := s.ListSessions(c, store.ListQuery{Limit: 5})
			requirePlainError(t, "ListSessions", err)
			if drop != "meter_readings" {
				_, _, err = s.SiteName(c, "100001")
				requirePlainError(t, "SiteName", err)
			}
		})
	}
}
