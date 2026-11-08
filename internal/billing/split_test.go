package billing

import (
	"errors"
	"math"
	"math/rand"
	"testing"
	"time"
)

// at builds a Singapore-time instant. Day 1 is 2026-11-06.
func at(tariff Tariff, day, hour, minute int) time.Time {
	return time.Date(2026, time.November, 5+day, hour, minute, 0, 0, tariff.Location)
}

func reading(tm time.Time, wh int64) Reading { return Reading{At: tm, MeterWh: wh} }

// wantSeg is the expected shape of a segment (times compared with Equal).
type wantSeg struct {
	window     Window
	start, end time.Time
	wh, rate   int64
}

func checkSegments(t *testing.T, got []Segment, want []wantSeg) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d segments, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Window != w.window || !g.Start.Equal(w.start) || !g.End.Equal(w.end) ||
			g.EnergyWh != w.wh || g.RateCentsPerKWh != w.rate || g.CostCents != 0 {
			t.Errorf("segment %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestWindowAt_BIL2_Boundaries(t *testing.T) {
	tariff := SingaporeTariff()
	cases := []struct {
		name string
		at   time.Time
		want Window
	}{
		{"06:59:59", time.Date(2026, time.November, 6, 6, 59, 59, 0, tariff.Location), OffPeak},
		{"07:00", time.Date(2026, time.November, 6, 7, 0, 0, 0, tariff.Location), Peak},
		{"22:59:59", time.Date(2026, time.November, 6, 22, 59, 59, 0, tariff.Location), Peak},
		{"23:00", time.Date(2026, time.November, 6, 23, 0, 0, 0, tariff.Location), OffPeak},
		{"00:00", time.Date(2026, time.November, 7, 0, 0, 0, 0, tariff.Location), OffPeak},
	}
	for _, c := range cases {
		if got := tariff.WindowAt(c.at); got != c.want {
			t.Errorf("%s: WindowAt = %q, want %q", c.name, got, c.want)
		}
		// The same instant expressed in UTC must give the same window.
		if got := tariff.WindowAt(c.at.UTC()); got != c.want {
			t.Errorf("%s (UTC): WindowAt = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRate_BIL2(t *testing.T) {
	tariff := SingaporeTariff()
	if got := tariff.Rate(Peak); got != 62 {
		t.Errorf("Rate(Peak) = %d, want 62", got)
	}
	if got := tariff.Rate(OffPeak); got != 48 {
		t.Errorf("Rate(OffPeak) = %d, want 48", got)
	}
}

func TestSplit_BIL4_SingleWindow(t *testing.T) {
	tr := SingaporeTariff()
	got, err := Split(tr, []Reading{reading(at(tr, 1, 10, 0), 0), reading(at(tr, 1, 12, 0), 14400)})
	if err != nil {
		t.Fatal(err)
	}
	checkSegments(t, got, []wantSeg{{Peak, at(tr, 1, 10, 0), at(tr, 1, 12, 0), 14400, 62}})
}

func TestSplit_BIL3_PeakToOffPeak(t *testing.T) {
	tr := SingaporeTariff()
	got, err := Split(tr, []Reading{reading(at(tr, 1, 22, 0), 0), reading(at(tr, 2, 0, 0), 12000)})
	if err != nil {
		t.Fatal(err)
	}
	checkSegments(t, got, []wantSeg{
		{Peak, at(tr, 1, 22, 0), at(tr, 1, 23, 0), 6000, 62},
		{OffPeak, at(tr, 1, 23, 0), at(tr, 2, 0, 0), 6000, 48},
	})
}

func TestSplit_BIL4_MidnightIsNotABoundary(t *testing.T) {
	tr := SingaporeTariff()
	got, err := Split(tr, []Reading{reading(at(tr, 1, 22, 30), 0), reading(at(tr, 2, 1, 30), 9000)})
	if err != nil {
		t.Fatal(err)
	}
	checkSegments(t, got, []wantSeg{
		{Peak, at(tr, 1, 22, 30), at(tr, 1, 23, 0), 1500, 62},
		{OffPeak, at(tr, 1, 23, 0), at(tr, 2, 1, 30), 7500, 48},
	})
}

func TestSplit_BIL4_PeakOffPeakPeak(t *testing.T) {
	tr := SingaporeTariff()
	got, err := Split(tr, []Reading{reading(at(tr, 1, 22, 0), 0), reading(at(tr, 2, 8, 0), 60000)})
	if err != nil {
		t.Fatal(err)
	}
	checkSegments(t, got, []wantSeg{
		{Peak, at(tr, 1, 22, 0), at(tr, 1, 23, 0), 6000, 62},
		{OffPeak, at(tr, 1, 23, 0), at(tr, 2, 7, 0), 48000, 48},
		{Peak, at(tr, 2, 7, 0), at(tr, 2, 8, 0), 6000, 62},
	})
}

func TestSplit_BIL4_MergesAcrossReadings(t *testing.T) {
	tr := SingaporeTariff()
	got, err := Split(tr, []Reading{
		reading(at(tr, 1, 21, 40), 0),
		reading(at(tr, 1, 22, 20), 4800),
		reading(at(tr, 1, 23, 0), 9600),
		reading(at(tr, 2, 0, 15), 18000),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkSegments(t, got, []wantSeg{
		{Peak, at(tr, 1, 21, 40), at(tr, 1, 23, 0), 9600, 62},
		{OffPeak, at(tr, 1, 23, 0), at(tr, 2, 0, 15), 8400, 48},
	})
}

func TestSplit_ReadingOnBoundaryMakesNoEmptySegment(t *testing.T) {
	tr := SingaporeTariff()
	got, err := Split(tr, []Reading{
		reading(at(tr, 1, 22, 0), 0),
		reading(at(tr, 1, 23, 0), 6000),
		reading(at(tr, 2, 0, 0), 10800),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d segments, want 2: %+v", len(got), got)
	}
	for i, s := range got {
		if s.Start.Equal(s.End) {
			t.Errorf("segment %d is empty: %+v", i, s)
		}
	}
}

// BIL-4: a reading exactly on a boundary must not create a zero-length piece
// at either end of the session.
func TestSplit_BIL4_LastReadingOnBoundary(t *testing.T) {
	tr := SingaporeTariff()
	got, err := Split(tr, []Reading{reading(at(tr, 1, 22, 0), 0), reading(at(tr, 1, 23, 0), 6000)})
	if err != nil {
		t.Fatal(err)
	}
	checkSegments(t, got, []wantSeg{{Peak, at(tr, 1, 22, 0), at(tr, 1, 23, 0), 6000, 62}})
}

func TestSplit_BIL4_FirstReadingOnBoundary(t *testing.T) {
	tr := SingaporeTariff()
	got, err := Split(tr, []Reading{reading(at(tr, 1, 23, 0), 0), reading(at(tr, 2, 0, 0), 6000)})
	if err != nil {
		t.Fatal(err)
	}
	checkSegments(t, got, []wantSeg{{OffPeak, at(tr, 1, 23, 0), at(tr, 2, 0, 0), 6000, 48}})
}

func TestSplit_BIL6_RemainderToLastPiece(t *testing.T) {
	tr := SingaporeTariff()
	got, err := Split(tr, []Reading{reading(at(tr, 1, 22, 0), 0), reading(at(tr, 1, 23, 30), 1000)})
	if err != nil {
		t.Fatal(err)
	}
	checkSegments(t, got, []wantSeg{
		{Peak, at(tr, 1, 22, 0), at(tr, 1, 23, 0), 666, 62},
		{OffPeak, at(tr, 1, 23, 0), at(tr, 1, 23, 30), 334, 48},
	})
}

// BIL-3 with an interval shorter than a millisecond: must not panic or lose energy.
func TestSplit_BIL3_SubMillisecondIntervalAcrossBoundary(t *testing.T) {
	tr := SingaporeTariff()
	start := time.Date(2026, 11, 6, 22, 59, 59, 999_999_500, tr.Location)
	end := time.Date(2026, 11, 6, 23, 0, 0, 500, tr.Location)
	got, err := Split(tr, []Reading{reading(start, 0), reading(end, 1000)})
	if err != nil {
		t.Fatal(err)
	}
	checkSegments(t, got, []wantSeg{
		{Peak, start, at(tr, 1, 23, 0), 500, 62},
		{OffPeak, at(tr, 1, 23, 0), end, 500, 48},
	})
}

func TestSplit_ZeroEnergyIntervalKeepsSegment(t *testing.T) {
	tr := SingaporeTariff()
	got, err := Split(tr, []Reading{reading(at(tr, 1, 10, 0), 500), reading(at(tr, 1, 11, 0), 500)})
	if err != nil {
		t.Fatal(err)
	}
	checkSegments(t, got, []wantSeg{{Peak, at(tr, 1, 10, 0), at(tr, 1, 11, 0), 0, 62}})
}

func TestSplit_FewerThanTwoReadings(t *testing.T) {
	tr := SingaporeTariff()
	for name, in := range map[string][]Reading{
		"nil":    nil,
		"single": {reading(at(tr, 1, 10, 0), 0)},
	} {
		got, err := Split(tr, in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: got %d segments, want 0", name, len(got))
		}
	}
}

func TestSplit_RejectsBadInput(t *testing.T) {
	tr := SingaporeTariff()
	cases := []struct {
		name string
		in   []Reading
		want error
	}{
		{"equal times", []Reading{reading(at(tr, 1, 10, 0), 0), reading(at(tr, 1, 10, 0), 10)}, ErrReadingsNotIncreasing},
		{"decreasing times", []Reading{reading(at(tr, 1, 10, 0), 0), reading(at(tr, 1, 9, 0), 10)}, ErrReadingsNotIncreasing},
		{"falling meter", []Reading{reading(at(tr, 1, 10, 0), 100), reading(at(tr, 1, 11, 0), 99)}, ErrEnergyDecreased},
		// BIL-6 / Review Focus 1: negative values must not wrap the subtraction.
		{"negative first reading", []Reading{reading(at(tr, 1, 10, 0), -1), reading(at(tr, 1, 11, 0), 10)}, ErrNegativeMeter},
		{"subtraction overflow repro", []Reading{
			reading(at(tr, 1, 10, 0), math.MinInt64/2-10), reading(at(tr, 1, 11, 0), math.MaxInt64/2+10),
		}, ErrNegativeMeter},
		{"bad second pair", []Reading{
			reading(at(tr, 1, 10, 0), 0), reading(at(tr, 1, 11, 0), 10), reading(at(tr, 1, 12, 0), 5),
		}, ErrEnergyDecreased},
	}
	for _, c := range cases {
		got, err := Split(tr, c.in)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
		if got != nil {
			t.Errorf("%s: got segments %+v with an error", c.name, got)
		}
	}
}

// BIL-6 with values that would overflow naive int64 products.
func TestSplit_LargeValuesNoOverflow(t *testing.T) {
	tr := SingaporeTariff()
	cases := []struct {
		name  string
		total int64
		want  [4]int64
	}{
		{"10 million Wh", 10_000_000, [4]int64{3333333, 1666666, 3333333, 1666668}},
		{"10^15 Wh", 1_000_000_000_000_000, [4]int64{333333333333333, 166666666666666, 333333333333333, 166666666666668}},
	}
	for _, c := range cases {
		got, err := Split(tr, []Reading{reading(at(tr, 1, 7, 0), 0), reading(at(tr, 3, 7, 0), c.total)})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		checkSegments(t, got, []wantSeg{
			{Peak, at(tr, 1, 7, 0), at(tr, 1, 23, 0), c.want[0], 62},
			{OffPeak, at(tr, 1, 23, 0), at(tr, 2, 7, 0), c.want[1], 48},
			{Peak, at(tr, 2, 7, 0), at(tr, 2, 23, 0), c.want[2], 62},
			{OffPeak, at(tr, 2, 23, 0), at(tr, 3, 7, 0), c.want[3], 48},
		})
	}
}

// localBoundaryAtOrAfter returns the first local 07:00 or 23:00 at or after tm.
func localBoundaryAtOrAfter(tr Tariff, tm time.Time) time.Time {
	y, m, d := tm.In(tr.Location).Date()
	for _, c := range []time.Time{
		time.Date(y, m, d, tr.PeakStartHour, 0, 0, 0, tr.Location),
		time.Date(y, m, d, tr.PeakEndHour, 0, 0, 0, tr.Location),
		time.Date(y, m, d+1, tr.PeakStartHour, 0, 0, 0, tr.Location),
	} {
		if !c.Before(tm) {
			return c
		}
	}
	return tm // unreachable: tomorrow's 07:00 is always at or after tm
}

// Invariants from BIL-3, BIL-4 and BIL-6 over random monotonic readings.
// snapMask bit i snaps reading i onto a local 07:00/23:00 instant; shortMask
// bit i makes the interval before reading i between 1 ns and 1 ms.
func FuzzSplit(f *testing.F) {
	f.Add(int64(1), uint8(2), uint16(0), uint16(0))
	f.Add(int64(42), uint8(5), uint16(0), uint16(0))
	f.Add(int64(-7), uint8(9), uint16(0xFFFF), uint16(0))      // every reading on a boundary
	f.Add(int64(1<<40), uint8(3), uint16(0), uint16(0xFFFF))   // all intervals tiny
	f.Add(int64(99), uint8(8), uint16(0xAAAA), uint16(0x5555)) // mixed
	f.Fuzz(func(t *testing.T, seed int64, count uint8, snapMask, shortMask uint16) {
		tr := SingaporeTariff()
		rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test data, not security
		n := int(count%10) + 2

		// Keep timestamps in 2000-2100: Singapore changed its UTC offset in 1982
		// and time.Sub saturates for spans beyond about 292 years.
		minUnix := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		spanSec := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).Unix() - minUnix
		now := time.Unix(minUnix+rng.Int63n(spanSec), rng.Int63n(1_000_000_000))

		// The meter starts anywhere in the lower half of the budget, so the
		// final value stays <= 10^15 Wh and every value is >= 0.
		const maxTotalWh = 1_000_000_000_000_000
		meter := rng.Int63n(maxTotalWh / 2)
		readings := make([]Reading, 0, n)
		for i := 0; i < n; i++ {
			if i > 0 {
				step := time.Duration(1 + rng.Int63n(int64(3*24*time.Hour)))
				if shortMask&(1<<i) != 0 {
					step = time.Duration(1 + rng.Int63n(int64(time.Millisecond)))
				}
				now = now.Add(step)
				if snapMask&(1<<i) != 0 {
					now = localBoundaryAtOrAfter(tr, now)
				}
				meter += rng.Int63n(maxTotalWh / 2 / int64(n))
			}
			readings = append(readings, reading(now, meter))
		}

		segs, err := Split(tr, readings)
		if err != nil {
			t.Fatalf("Split: %v", err)
		}
		if len(segs) == 0 {
			t.Fatal("no segments")
		}
		var sum int64
		for i, s := range segs {
			if s.EnergyWh < 0 {
				t.Errorf("segment %d has negative energy %d", i, s.EnergyWh)
			}
			if !s.Start.Before(s.End) {
				t.Errorf("segment %d is empty or reversed: %+v", i, s)
			}
			if s.RateCentsPerKWh != tr.Rate(s.Window) {
				t.Errorf("segment %d has rate %d for window %q", i, s.RateCentsPerKWh, s.Window)
			}
			sum += s.EnergyWh
			if i+1 < len(segs) {
				if !s.End.Equal(segs[i+1].Start) {
					t.Errorf("gap between segment %d and %d", i, i+1)
				}
				if s.Window == segs[i+1].Window {
					t.Errorf("segments %d and %d share window %q", i, i+1, s.Window)
				}
			}
		}
		if want := readings[len(readings)-1].MeterWh - readings[0].MeterWh; sum != want {
			t.Errorf("energy sum = %d, want %d", sum, want)
		}
		if !segs[0].Start.Equal(readings[0].At) {
			t.Errorf("first segment starts %v, want %v", segs[0].Start, readings[0].At)
		}
		if last := segs[len(segs)-1]; !last.End.Equal(readings[len(readings)-1].At) {
			t.Errorf("last segment ends %v, want %v", last.End, readings[len(readings)-1].At)
		}
	})
}
