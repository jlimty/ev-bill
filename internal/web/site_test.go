package web

import (
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/locations"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
)

// LOC-6: a site's totals are its sessions billed exactly as the list bills
// them (BIL rules at the 62/48 tariff), summed. A stopped session from 22:30
// to 23:30 SGT of 1,000 Wh is 500 Wh peak (31 c) and 500 Wh off-peak (24 c);
// an open one with a reading 1,000 Wh above its start, all off-peak, is 48 c.
func TestBillSite_LOC6(t *testing.T) {
	stop := sgt(23, 30, 6)
	stopWh := int64(1_000)
	items := []store.SessionWithReadings{
		{Session: sessions.Session{ID: "a", ChargerID: "C-1", Source: sessions.SourceMirror,
			StartedAt: sgt(22, 30, 6), MeterStartWh: 0, StoppedAt: &stop, MeterStopWh: &stopWh}},
		{Session: sessions.Session{ID: "b", ChargerID: "C-2", Source: sessions.SourceMirror,
			StartedAt: sgt(0, 0, 7), MeterStartWh: 100},
			Readings: []billing.Reading{{At: sgt(0, 10, 7), MeterWh: 1_100}}},
	}
	got, err := billSite(items, billing.SingaporeTariff(), fixedNow())
	if err != nil {
		t.Fatalf("billSite: %v", err)
	}
	if want := (siteTotals{Sessions: 2, EnergyWh: 2_000, CostCents: 103}); got != want {
		t.Errorf("billSite = %+v, want %+v", got, want)
	}
	if got, err := billSite(nil, billing.SingaporeTariff(), fixedNow()); err != nil || got != (siteTotals{}) {
		t.Errorf("billSite(nil) = %+v, %v; want zero totals", got, err)
	}
}

// LOC-6: a session the billing rules reject is an error, not a silent zero.
func TestBillSite_LOC6_BillErrorReturned(t *testing.T) {
	items := []store.SessionWithReadings{{
		Session:  sessions.Session{ID: "bad", ChargerID: "C-1", Source: sessions.SourceMirror, StartedAt: sgt(0, 0, 7), MeterStartWh: 500},
		Readings: []billing.Reading{{At: sgt(0, 10, 7), MeterWh: 100}}, // the meter goes backwards
	}}
	if _, err := billSite(items, billing.SingaporeTariff(), fixedNow()); err == nil {
		t.Error("billSite with a backwards meter: want an error, got nil")
	}
}

// LOC-6, NAV-2: the totals line names the count, the estimated energy and the
// estimated cost, with the noun's singular for one. The card shows it as is;
// the tariff is named by the header key, the notice and the drill-down.
func TestTotalsLine_LOC6(t *testing.T) {
	for _, tc := range []struct {
		n    siteTotals
		want string
	}{
		{siteTotals{7, 205_600, 11_418}, "7 billing sessions · 205.6 kWh est. · $114.18 est."},
		{siteTotals{1, 1_000, 55}, "1 billing session · 1.0 kWh est. · $0.55 est."},
		{siteTotals{}, "0 billing sessions · 0.0 kWh est. · $0.00 est."},
	} {
		if got := totalsLine(tc.n, "billing session", "billing sessions"); got != tc.want {
			t.Errorf("totalsLine(%+v) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// LOC-8: the observing line dates the site's first observation in Singapore
// time until it is 14 days old (within the window slack), then reads "Last 14
// days"; a site never observed has no line. Times are UTC instants, so the
// test means the same under any TZ (make test-tz).
func TestObservingLine_LOC8(t *testing.T) {
	p := &Pages{Tariff: billing.SingaporeTariff(), Now: fixedNow}
	now := time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC)
	start := time.Date(2026, 11, 8, 8, 50, 0, 0, time.UTC) // 16:50 SGT
	if got, want := p.observingLine(now, start), "Observing since 8 Nov, 16:50"; got != want {
		t.Errorf("observingLine = %q, want %q", got, want)
	}
	full := now.Add(-locations.Window + fullWindowSlack)
	if got, want := p.observingLine(now, full), "Last 14 days"; got != want {
		t.Errorf("observingLine at the full window = %q, want %q", got, want)
	}
	if got, want := p.observingLine(now, full.Add(time.Minute)), "Observing since"; len(got) < len(want) || got[:len(want)] != want {
		t.Errorf("observingLine a minute short of the full window = %q, want it to start %q", got, want)
	}
	lastYear := time.Date(2025, 12, 31, 15, 0, 0, 0, time.UTC) // 23:00 SGT
	if got, want := p.observingLine(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), lastYear), "Observing since 31 Dec 2025, 23:00"; got != want {
		t.Errorf("observingLine across a year = %q, want %q", got, want)
	}
	if got := p.observingLine(now, time.Time{}); got != "" {
		t.Errorf("observingLine with no observation = %q, want empty", got)
	}
}

// LOC-8: observation starts at the earlier of the first sample and the first
// session; with neither it is zero.
func TestObservingStart_LOC8(t *testing.T) {
	sample := time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC)
	session := time.Date(2026, 11, 8, 8, 50, 0, 0, time.UTC)
	withSession := []store.SessionWithReadings{{Session: sessions.Session{StartedAt: session}}}
	for _, tc := range []struct {
		name string
		a    store.SiteActivity
		want time.Time
	}{
		{"session first", store.SiteActivity{FirstSample: sample, Sessions: withSession}, session},
		{"sample first", store.SiteActivity{FirstSample: session.Add(-time.Hour), Sessions: withSession}, session.Add(-time.Hour)},
		{"no sessions", store.SiteActivity{FirstSample: sample}, sample},
		{"no sample", store.SiteActivity{Sessions: withSession}, session},
		{"neither", store.SiteActivity{}, time.Time{}},
	} {
		if got := observingStart(tc.a); !got.Equal(tc.want) {
			t.Errorf("%s: observingStart = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// NAV-2 (posted price in the charger line): a charger's posted price today,
// per kWh, trailing zeros trimmed to two places, integers only; "posted free"
// when free; "posted price n/a" when the price is not per kWh or unparseable;
// nothing when the session's charger has no row.
func TestChargerPrice_NAV2(t *testing.T) {
	ch := func(price, typ string) *store.ChargerRow {
		return &store.ChargerRow{Connector: datamall.Connector{Price: price, PriceType: typ}}
	}
	for _, tc := range []struct {
		name string
		c    *store.ChargerRow
		want string
	}{
		{"four places", ch("0.6976", "kWh"), "posted $0.6976/kWh"},
		{"three places", ch("0.5450", "kWh"), "posted $0.545/kWh"},
		{"trailing zeros", ch("0.70", "kWh"), "posted $0.70/kWh"},
		{"whole dollars", ch("1", "kWh"), "posted $1.00/kWh"},
		{"free", ch("", "free"), "posted free"},
		{"per hour", ch("0.50", "$/h"), "posted price n/a"},
		{"no type", ch("0.50", ""), "posted price n/a"},
		{"unparseable", ch("abc", "kWh"), "posted price n/a"},
		{"no charger row", nil, ""},
	} {
		if got := chargerPrice(tc.c); got != tc.want {
			t.Errorf("%s: chargerPrice = %q, want %q", tc.name, got, tc.want)
		}
	}
}
