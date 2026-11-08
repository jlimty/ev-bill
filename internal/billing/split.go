package billing

import (
	"errors"
	"fmt"
	"math/bits"
	"time"
)

// Reading is a cumulative meter value at an instant.
type Reading struct {
	At      time.Time
	MeterWh int64
}

// Segment is contiguous time in one window. CostCents is filled in by pricing, not by Split.
type Segment struct {
	Window          Window
	Start, End      time.Time
	EnergyWh        int64
	RateCentsPerKWh int64
	CostCents       int64
}

var (
	// ErrReadingsNotIncreasing means reading times are equal or going backwards.
	ErrReadingsNotIncreasing = errors.New("reading times must be strictly increasing")
	// ErrEnergyDecreased means a cumulative meter value fell between readings.
	ErrEnergyDecreased = errors.New("meter value must not decrease")
	// ErrNegativeMeter means a meter value is below zero; a cumulative register is never negative.
	ErrNegativeMeter = errors.New("meter value must not be negative")
)

// Split divides the energy between consecutive readings across tariff windows
// (BIL-3), merges contiguous time in the same window (BIL-4) and conserves
// energy exactly (BIL-6). Fewer than two readings yield no segments.
func Split(t Tariff, readings []Reading) ([]Segment, error) {
	// Validate everything first so a bad reading never yields partial output.
	// Once every value is >= 0 and non-decreasing, b.MeterWh - a.MeterWh
	// cannot overflow int64 in splitInterval.
	var prev Reading
	for i, r := range readings {
		if r.MeterWh < 0 {
			return nil, fmt.Errorf("reading %d: %w", i, ErrNegativeMeter)
		}
		if i > 0 {
			if !r.At.After(prev.At) {
				return nil, fmt.Errorf("reading %d: %w", i, ErrReadingsNotIncreasing)
			}
			if r.MeterWh < prev.MeterWh {
				return nil, fmt.Errorf("reading %d: %w", i, ErrEnergyDecreased)
			}
		}
		prev = r
	}

	var segments []Segment
	for i := 1; i < len(readings); i++ {
		for _, p := range splitInterval(t, readings[i-1], readings[i]) {
			segments = appendMerged(segments, p)
		}
	}
	return segments, nil
}

// splitInterval cuts one reading interval at every tariff boundary strictly
// inside it and shares the energy between the pieces in proportion to time.
// Callers guarantee b.At is after a.At and b.MeterWh >= a.MeterWh.
func splitInterval(t Tariff, a, b Reading) []Segment {
	bounds := append([]time.Time{a.At.UTC()}, boundariesBetween(t, a.At, b.At)...)
	bounds = append(bounds, b.At.UTC())

	energy := b.MeterWh - a.MeterWh
	// Measure the whole from the same UTC bounds as the parts. UTC() drops any
	// monotonic clock reading, so mixing in b.At.Sub(a.At) could make a part
	// larger than the whole. Here part < whole holds by construction.
	intervalNs := bounds[len(bounds)-1].Sub(bounds[0])
	pieces := make([]Segment, 0, len(bounds)-1)
	var assigned int64
	for i := 0; i+1 < len(bounds); i++ {
		var wh int64
		if i+2 == len(bounds) {
			// BIL-6: the last piece takes whatever is left.
			wh = energy - assigned
		} else {
			wh = proportion(energy, bounds[i+1].Sub(bounds[i]), intervalNs)
		}
		assigned += wh
		w := t.WindowAt(bounds[i])
		pieces = append(pieces, Segment{
			Window:          w,
			Start:           bounds[i],
			End:             bounds[i+1],
			EnergyWh:        wh,
			RateCentsPerKWh: t.Rate(w),
		})
	}
	return pieces
}

// boundariesBetween returns, in order and in UTC, every local peak start and
// peak end strictly between from and to. An instant equal to either end is not
// a cut, so a reading exactly on a boundary makes no zero-length piece.
func boundariesBetween(t Tariff, from, to time.Time) []time.Time {
	var cuts []time.Time
	day := from.In(t.Location)
	y, m, d := day.Date()
	for ; ; d++ {
		for _, hour := range []int{t.PeakStartHour, t.PeakEndHour} {
			c := time.Date(y, m, d, hour, 0, 0, 0, t.Location)
			if c.After(from) && c.Before(to) {
				cuts = append(cuts, c.UTC())
			}
		}
		if !time.Date(y, m, d, 0, 0, 0, 0, t.Location).Before(to) {
			return cuts
		}
	}
}

// proportion returns floor(energy * part / whole) using 128-bit intermediates
// so large meter values and nanosecond durations cannot overflow.
//
// Invariants (guaranteed by Split's validation and by the caller): energy >= 0,
// whole > 0 and 0 <= part < whole. Because part < whole, the quotient is below
// 2^63, so bits.Div64's precondition (hi < divisor) holds and it cannot panic.
func proportion(energy int64, part, whole time.Duration) int64 {
	hi, lo := bits.Mul64(uint64(energy), uint64(part)) //nolint:gosec // G115: both are non-negative (see invariants)
	// Div64's second result is the remainder, which floor division discards.
	quotient, _ := bits.Div64(hi, lo, uint64(whole)) //nolint:gosec // G115: whole > 0
	return int64(quotient)                           //nolint:gosec // G115: quotient <= energy, which fits in int64
}

// appendMerged adds p, extending the last segment when it has the same window (BIL-4).
func appendMerged(segments []Segment, p Segment) []Segment {
	if n := len(segments); n > 0 && segments[n-1].Window == p.Window {
		segments[n-1].End = p.End
		segments[n-1].EnergyWh += p.EnergyWh
		return segments
	}
	return append(segments, p)
}
