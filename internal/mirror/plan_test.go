package mirror

import (
	"reflect"
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

const gap = 11 * time.Minute

func conn(id string, st datamall.Status, cur string, w int64) datamall.Connector {
	return datamall.Connector{ChargerID: id, Status: st, Current: cur, PowerW: w}
}

func open(id, ch string, started, lastAt time.Time, wh int64) OpenSession {
	return OpenSession{ID: id, ChargerID: ch, StartedAt: started, Last: billing.Reading{At: lastAt, MeterWh: wh}}
}

func act(k ActionKind, ch, sid string, at time.Time, wh int64) Action {
	return Action{Kind: k, ChargerID: ch, SessionID: sid, At: at, MeterWh: wh}
}

func TestPowerW_MIR4(t *testing.T) {
	tests := []struct {
		name string
		c    datamall.Connector
		want int64
	}{
		{"AC rated", conn("A", datamall.Occupied, "AC", 22_000), 22_000},
		{"DC capped", conn("A", datamall.Occupied, "DC", 180_000), 50_000},
		{"DC under cap", conn("A", datamall.Occupied, "DC", 30_000), 30_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PowerW(tc.c); got != tc.want {
				t.Fatalf("PowerW = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMeter_MIR4(t *testing.T) {
	tests := []struct {
		name  string
		last  billing.Reading
		at    time.Time
		power int64
		want  int64
	}{
		{"7.4 kW for 30 min", billing.Reading{At: t0, MeterWh: 0}, t0.Add(30 * time.Minute), 7_400, 3_700},
		{"cap reached", billing.Reading{At: t0, MeterWh: 59_000}, t0.Add(time.Hour), 50_000, 60_000},
		{"flat at cap", billing.Reading{At: t0, MeterWh: 60_000}, t0.Add(time.Hour), 50_000, 60_000},
		{"at before last", billing.Reading{At: t0, MeterWh: 1_234}, t0.Add(-time.Minute), 50_000, 1_234},
		{"sub-second ignored", billing.Reading{At: t0, MeterWh: 0}, t0.Add(1500 * time.Millisecond), 3_600_000, 1_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Meter(tc.last, tc.at, tc.power); got != tc.want {
				t.Fatalf("Meter = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPlan_MIR6_StartReadStop(t *testing.T) {
	last := t0.Add(-5 * time.Minute)
	started := t0.Add(-time.Hour)
	tests := []struct {
		name string
		st   datamall.Status
		open bool
		want []Action
	}{
		{"occupied, no session: start", datamall.Occupied, false, []Action{act(ActStart, "C1", "", t0, 0)}},
		{"occupied, open: read", datamall.Occupied, true, []Action{act(ActRead, "C1", "S1", t0, 1_000+22_000*300/3600)}},
		{"available, open: stop", datamall.Available, true, []Action{act(ActStop, "C1", "S1", t0, 1_000+22_000*300/3600)}},
		{"unavailable, open: stop", datamall.Unavailable, true, []Action{act(ActStop, "C1", "S1", t0, 1_000+22_000*300/3600)}},
		{"available, no session: nothing", datamall.Available, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Observed: t0, GapAfter: gap, Connectors: []datamall.Connector{conn("C1", tc.st, "AC", 22_000)}}
			if tc.open {
				in.Open = map[string]OpenSession{"C1": open("S1", "C1", started, last, 1_000)}
			}
			res := Plan(in)
			if !reflect.DeepEqual(res.Actions, tc.want) {
				t.Fatalf("actions = %+v, want %+v", res.Actions, tc.want)
			}
			if got := res.Next["C1"]; got != (ChargerState{Status: tc.st}) {
				t.Fatalf("next = %+v", got)
			}
		})
	}
}

func TestPlan_MIR5_GapClosesAtLastPointAndCarries(t *testing.T) {
	last := t0.Add(-12 * time.Minute)
	in := Input{
		Observed: t0, GapAfter: gap,
		Connectors: []datamall.Connector{conn("C1", datamall.Occupied, "AC", 22_000)},
		Prev:       map[string]ChargerState{"C1": {Status: datamall.Occupied}},
		Open:       map[string]OpenSession{"C1": open("S1", "C1", t0.Add(-time.Hour), last, 4_000)},
	}
	want := []Action{act(ActStop, "C1", "S1", last, 4_000), act(ActStart, "C1", "", t0, 4_000)}
	if got := Plan(in).Actions; !reflect.DeepEqual(got, want) {
		t.Fatalf("actions = %+v, want %+v", got, want)
	}
}

func TestPlan_MIR5_GapThresholdIsStrict(t *testing.T) {
	last := t0.Add(-gap) // exactly the threshold is not a gap
	in := Input{
		Observed: t0, GapAfter: gap,
		Connectors: []datamall.Connector{conn("C1", datamall.Occupied, "AC", 3_600)},
		Open:       map[string]OpenSession{"C1": open("S1", "C1", t0.Add(-time.Hour), last, 0)},
	}
	want := []Action{act(ActRead, "C1", "S1", t0, 660)} // 3,600 W for 11 min
	if got := Plan(in).Actions; !reflect.DeepEqual(got, want) {
		t.Fatalf("actions = %+v, want %+v", got, want)
	}
}

func TestPlan_MIR5_GapAndFreeOnlyCloses(t *testing.T) {
	last := t0.Add(-12 * time.Minute)
	in := Input{
		Observed: t0, GapAfter: gap,
		Connectors: []datamall.Connector{conn("C1", datamall.Available, "AC", 22_000)},
		Open:       map[string]OpenSession{"C1": open("S1", "C1", t0.Add(-time.Hour), last, 4_000)},
	}
	want := []Action{act(ActStop, "C1", "S1", last, 4_000)}
	if got := Plan(in).Actions; !reflect.DeepEqual(got, want) {
		t.Fatalf("actions = %+v, want %+v", got, want)
	}
}

func TestPlan_MIR12_StopsAt48hAndHolds(t *testing.T) {
	started := t0.Add(-48*time.Hour - 3*time.Minute)
	last := t0.Add(-5 * time.Minute)
	end := started.Add(48 * time.Hour)
	c := []datamall.Connector{conn("C1", datamall.Occupied, "AC", 7_200)}

	// 1. Crossing 48 h: stop at exactly started+48h, interpolated, and hold.
	res := Plan(Input{
		Observed: t0, GapAfter: gap, Connectors: c,
		Open: map[string]OpenSession{"C1": open("S1", "C1", started, last, 10_000)},
	})
	// last is 5 min before t0; end is 3 min before t0, so 2 min after last: 7200*120/3600 = 240.
	want := []Action{act(ActStop, "C1", "S1", end, 10_240)}
	if !reflect.DeepEqual(res.Actions, want) {
		t.Fatalf("actions = %+v, want %+v", res.Actions, want)
	}
	if !res.Next["C1"].Held {
		t.Fatal("Next.Held = false, want true")
	}

	// 2. Held and still occupied: nothing.
	res = Plan(Input{
		Observed: t0.Add(5 * time.Minute), GapAfter: gap, Connectors: c,
		Prev: map[string]ChargerState{"C1": {Status: datamall.Occupied, Held: true}},
	})
	if len(res.Actions) != 0 || !res.Next["C1"].Held {
		t.Fatalf("held cycle: actions = %+v, next = %+v", res.Actions, res.Next["C1"])
	}

	// 3. Seen available: Held clears.
	res = Plan(Input{
		Observed: t0.Add(10 * time.Minute), GapAfter: gap,
		Connectors: []datamall.Connector{conn("C1", datamall.Available, "AC", 7_200)},
		Prev:       map[string]ChargerState{"C1": {Status: datamall.Occupied, Held: true}},
	})
	if len(res.Actions) != 0 || res.Next["C1"].Held {
		t.Fatalf("free cycle: actions = %+v, next = %+v", res.Actions, res.Next["C1"])
	}

	// 4. Occupied again: a fresh session at meter 0.
	res = Plan(Input{
		Observed: t0.Add(15 * time.Minute), GapAfter: gap, Connectors: c,
		Prev:     map[string]ChargerState{"C1": {Status: datamall.Available}},
		LastStop: map[string]LastStop{"C1": {MeterWh: 10_240, AtLimit: true}},
	})
	want = []Action{act(ActStart, "C1", "", t0.Add(15*time.Minute), 0)}
	if !reflect.DeepEqual(res.Actions, want) {
		t.Fatalf("restart actions = %+v, want %+v", res.Actions, want)
	}
}

func TestPlan_MIR12_GapOnHeldChargerDoesNotReopen(t *testing.T) {
	// A held connector with a stale open session: close it, do not reopen.
	last := t0.Add(-20 * time.Minute)
	res := Plan(Input{
		Observed: t0, GapAfter: gap,
		Connectors: []datamall.Connector{conn("C1", datamall.Occupied, "AC", 7_200)},
		Prev:       map[string]ChargerState{"C1": {Status: datamall.Occupied, Held: true}},
		Open:       map[string]OpenSession{"C1": open("S1", "C1", t0.Add(-time.Hour), last, 100)},
	})
	want := []Action{act(ActStop, "C1", "S1", last, 100)}
	if !reflect.DeepEqual(res.Actions, want) {
		t.Fatalf("actions = %+v, want %+v", res.Actions, want)
	}
}

func TestPlan_MIR4_CapAcrossSessions(t *testing.T) {
	last := t0.Add(-12 * time.Minute)
	c := []datamall.Connector{conn("C1", datamall.Occupied, "DC", 180_000)}
	res := Plan(Input{
		Observed: t0, GapAfter: gap, Connectors: c,
		Prev: map[string]ChargerState{"C1": {Status: datamall.Occupied}},
		Open: map[string]OpenSession{"C1": open("S1", "C1", t0.Add(-time.Hour), last, 59_000)},
	})
	want := []Action{act(ActStop, "C1", "S1", last, 59_000), act(ActStart, "C1", "", t0, 59_000)}
	if !reflect.DeepEqual(res.Actions, want) {
		t.Fatalf("split actions = %+v, want %+v", res.Actions, want)
	}
	// 10 min later at 50 kW (8,333 Wh, inside the gap threshold): the new
	// session's meter is capped at 60,000, not 67,333.
	next := t0.Add(10 * time.Minute)
	res = Plan(Input{
		Observed: next, GapAfter: gap, Connectors: c,
		Prev: map[string]ChargerState{"C1": {Status: datamall.Occupied}},
		Open: map[string]OpenSession{"C1": open("S2", "C1", t0, t0, 59_000)},
	})
	want = []Action{act(ActRead, "C1", "S2", next, 60_000)}
	if !reflect.DeepEqual(res.Actions, want) {
		t.Fatalf("read actions = %+v, want %+v", res.Actions, want)
	}
}

func TestPlan_MIR4_CarryAfterCutCycle(t *testing.T) {
	tests := []struct {
		name string
		prev datamall.Status
		want int64
	}{
		{"was occupied: carry", datamall.Occupied, 5_000},
		{"was available: zero", datamall.Available, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := Plan(Input{
				Observed: t0, GapAfter: gap,
				Connectors: []datamall.Connector{conn("C1", datamall.Occupied, "AC", 7_200)},
				Prev:       map[string]ChargerState{"C1": {Status: tc.prev}},
				LastStop:   map[string]LastStop{"C1": {MeterWh: 5_000}},
			})
			want := []Action{act(ActStart, "C1", "", t0, tc.want)}
			if !reflect.DeepEqual(res.Actions, want) {
				t.Fatalf("actions = %+v, want %+v", res.Actions, want)
			}
		})
	}
}

func TestPlan_MIR6_UnwatchedOpenSessionClosesAtLastPoint(t *testing.T) {
	l1 := t0.Add(-4 * time.Minute)
	l2 := t0.Add(-3 * time.Minute)
	res := Plan(Input{
		Observed: t0, GapAfter: gap,
		Connectors: []datamall.Connector{conn("C2", datamall.Available, "AC", 7_200)},
		Open: map[string]OpenSession{
			"Z9": open("S9", "Z9", t0.Add(-time.Hour), l2, 900),
			"B1": open("S1", "B1", t0.Add(-time.Hour), l1, 700),
		},
	})
	want := []Action{act(ActStop, "B1", "S1", l1, 700), act(ActStop, "Z9", "S9", l2, 900)}
	if !reflect.DeepEqual(res.Actions, want) {
		t.Fatalf("actions = %+v, want %+v", res.Actions, want)
	}
	if len(res.Next) != 1 || res.Next["C2"] != (ChargerState{Status: datamall.Available}) {
		t.Fatalf("next = %+v, want only C2", res.Next)
	}
}

func TestPlan_Deterministic(t *testing.T) {
	last := t0.Add(-5 * time.Minute)
	in := Input{
		Observed: t0, GapAfter: gap,
		Connectors: []datamall.Connector{
			conn("C3", datamall.Occupied, "AC", 7_200),
			conn("C1", datamall.Occupied, "DC", 100_000),
			conn("C2", datamall.Available, "AC", 7_200),
		},
		Open: map[string]OpenSession{
			"C2": open("S2", "C2", t0.Add(-time.Hour), last, 10),
			"Y":  open("SY", "Y", t0.Add(-time.Hour), last, 20),
			"X":  open("SX", "X", t0.Add(-time.Hour), last, 30),
		},
	}
	want := []Action{
		act(ActStart, "C1", "", t0, 0),
		act(ActStop, "C2", "S2", t0, 10+7_200*300/3600),
		act(ActStart, "C3", "", t0, 0),
		act(ActStop, "X", "SX", last, 30),
		act(ActStop, "Y", "SY", last, 20),
	}
	for i := 0; i < 20; i++ {
		if got := Plan(in).Actions; !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: actions = %+v, want %+v", i, got, want)
		}
	}
	if in.Connectors[0].ChargerID != "C3" {
		t.Fatal("Plan reordered the caller's connectors")
	}
}

func TestPlan_MIR12_FreeAfter48hStopsAtLimit(t *testing.T) {
	started := t0.Add(-48*time.Hour - 2*time.Minute)
	last := t0.Add(-5 * time.Minute)
	end := started.Add(48 * time.Hour)
	res := Plan(Input{
		Observed: t0, GapAfter: gap,
		Connectors: []datamall.Connector{conn("C1", datamall.Available, "AC", 7_200)},
		Open:       map[string]OpenSession{"C1": open("S1", "C1", started, last, 10_000)},
	})
	// end is 3 min after last: 7200*180/3600 = 360.
	want := []Action{act(ActStop, "C1", "S1", end, 10_360)}
	if !reflect.DeepEqual(res.Actions, want) {
		t.Fatalf("actions = %+v, want %+v", res.Actions, want)
	}
	if res.Next["C1"].Held {
		t.Fatal("Next.Held = true, want false")
	}
}

func TestPlan_MIR12_ExactlyAt48hStopsAndHolds(t *testing.T) {
	started := t0.Add(-48 * time.Hour)
	last := t0.Add(-5 * time.Minute)
	res := Plan(Input{
		Observed: t0, GapAfter: gap,
		Connectors: []datamall.Connector{conn("C1", datamall.Occupied, "AC", 7_200)},
		Open:       map[string]OpenSession{"C1": open("S1", "C1", started, last, 10_000)},
	})
	want := []Action{act(ActStop, "C1", "S1", t0, 10_600)} // 7200*300/3600 = 600
	if !reflect.DeepEqual(res.Actions, want) || !res.Next["C1"].Held {
		t.Fatalf("actions = %+v, next = %+v", res.Actions, res.Next["C1"])
	}
}

func TestPlan_MIR12_CutCycleAfter48hStaysHeld(t *testing.T) {
	prev := map[string]ChargerState{"C1": {Status: datamall.Occupied}}
	ls := map[string]LastStop{"C1": {MeterWh: 9_000, AtLimit: true}}
	res := Plan(Input{
		Observed: t0, GapAfter: gap, Prev: prev, LastStop: ls,
		Connectors: []datamall.Connector{conn("C1", datamall.Occupied, "AC", 7_200)},
	})
	if len(res.Actions) != 0 || !res.Next["C1"].Held {
		t.Fatalf("actions = %+v, next = %+v", res.Actions, res.Next["C1"])
	}
	res = Plan(Input{
		Observed: t0.Add(5 * time.Minute), GapAfter: gap, Prev: prev, LastStop: ls,
		Connectors: []datamall.Connector{conn("C1", datamall.Available, "AC", 7_200)},
	})
	if len(res.Actions) != 0 || res.Next["C1"].Held {
		t.Fatalf("free: actions = %+v, next = %+v", res.Actions, res.Next["C1"])
	}
}
