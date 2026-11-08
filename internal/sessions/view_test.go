package sessions

import (
	"errors"
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
)

// SES-4: status is derived at read time.
func TestStatus_SES4(t *testing.T) {
	tariff := billing.SingaporeTariff()
	s, readings, _ := fixture() // last point 10:10
	last := at(10, 10, 0)

	cases := []struct {
		name string
		s    Session
		now  time.Time
		want string
	}{
		{"exactly 15m is charging", s, last.Add(15 * time.Minute), StatusCharging},
		{"15m+1s is stale", s, last.Add(15*time.Minute + time.Second), StatusStale},
		{"stopped is completed even when old", stopped(s, at(10, 30, 0), 500), at(20, 0, 0), StatusCompleted},
	}
	for _, c := range cases {
		v, err := BuildView(c.s, readings, tariff, c.now)
		if err != nil {
			t.Errorf("%s: err = %v", c.name, err)
			continue
		}
		if v.Status != c.want {
			t.Errorf("%s: status = %q, want %q", c.name, v.Status, c.want)
		}
	}

	// With no readings the start time is the last point.
	start := Session{ChargerID: "CP-01", StartedAt: at(10, 0, 0), MeterStartWh: 0}
	v, err := BuildView(start, nil, tariff, at(10, 15, 1))
	if err != nil || v.Status != StatusStale {
		t.Errorf("no readings 15m+1s: status=%q err=%v, want stale", v.Status, err)
	}
}

func overnight() (Session, []billing.Reading) {
	s := Session{ID: "s1", ChargerID: "CP-01", StartedAt: at(21, 40, 0), MeterStartWh: 0}
	readings := []billing.Reading{
		{At: at(22, 20, 0), MeterWh: 4800},
		{At: at(23, 0, 0), MeterWh: 9600},
	}
	return s, readings
}

// BIL-7: an active session is billed up to its latest reading.
func TestBuildView_BIL7_ActiveUsesLatestReading(t *testing.T) {
	s, readings := overnight()
	v, err := BuildView(s, readings, billing.SingaporeTariff(), at(23, 5, 0))
	if err != nil {
		t.Fatal(err)
	}
	if v.Bill.EnergyWh != 9600 || v.Bill.TotalCents != 595 {
		t.Errorf("bill = %d Wh, %d cents; want 9600 Wh, 595 cents", v.Bill.EnergyWh, v.Bill.TotalCents)
	}
	if v.Status != StatusCharging {
		t.Errorf("status = %q, want charging", v.Status)
	}
	if !v.LastReadingAt.Equal(at(23, 0, 0)) {
		t.Errorf("LastReadingAt = %v, want 23:00", v.LastReadingAt)
	}
}

// BIL-7: a completed session includes its stop point.
func TestBuildView_CompletedIncludesStop(t *testing.T) {
	s, readings := overnight()
	stop := time.Date(2026, 11, 7, 0, 15, 0, 0, sgt)
	s = stopped(s, stop, 18000)
	v, err := BuildView(s, readings, billing.SingaporeTariff(), stop.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusCompleted || v.Bill.EnergyWh != 18000 || v.Bill.TotalCents != 998 {
		t.Errorf("view = %q %d Wh %d cents; want completed 18000 Wh 998 cents", v.Status, v.Bill.EnergyWh, v.Bill.TotalCents)
	}
	if !v.LastReadingAt.Equal(stop) {
		t.Errorf("LastReadingAt = %v, want stop time", v.LastReadingAt)
	}
}

// SES-4 / BIL-7: a session with no readings bills nothing and is charging.
func TestBuildView_NoReadings(t *testing.T) {
	s := Session{ID: "s1", ChargerID: "CP-01", StartedAt: at(10, 0, 0), MeterStartWh: 100}
	v, err := BuildView(s, nil, billing.SingaporeTariff(), at(10, 5, 0))
	if err != nil {
		t.Fatal(err)
	}
	if v.Bill.EnergyWh != 0 || v.Bill.TotalCents != 0 || len(v.Bill.Segments) != 0 || v.Status != StatusCharging {
		t.Errorf("view = %+v, want empty charging bill", v)
	}
	if !v.LastReadingAt.Equal(at(10, 0, 0)) {
		t.Errorf("LastReadingAt = %v, want start time", v.LastReadingAt)
	}
}

// R1: a stop repeating the last reading must not hand Compute two equal
// timestamps; the bill equals the bill without the duplicate.
func TestBuildView_R1_StopRepeatsLastReading(t *testing.T) {
	s, readings, _ := fixture()
	tariff := billing.SingaporeTariff()
	now := at(10, 20, 0)

	withStop, err := BuildView(stopped(s, at(10, 10, 0), 300), readings, tariff, now)
	if err != nil {
		t.Fatalf("completed view with duplicate point: %v", err)
	}
	active, err := BuildView(s, readings, tariff, now)
	if err != nil {
		t.Fatal(err)
	}
	if withStop.Bill.EnergyWh != 200 || withStop.Bill.EnergyWh != active.Bill.EnergyWh || withStop.Bill.TotalCents != active.Bill.TotalCents {
		t.Errorf("bill with stop = %+v, without = %+v; want 200 Wh and equal", withStop.Bill, active.Bill)
	}
}

// BIL-7: a view whose points cannot be billed returns the billing error, wrapped.
func TestBuildView_BIL7_ComputeError(t *testing.T) {
	s, readings, now := fixture()
	// A stored reading at the stop instant with a different meter.
	_, err := BuildView(stopped(s, at(10, 10, 0), 999), readings, billing.SingaporeTariff(), now)
	if !errors.Is(err, billing.ErrReadingsNotIncreasing) {
		t.Errorf("err = %v, want ErrReadingsNotIncreasing", err)
	}
}
