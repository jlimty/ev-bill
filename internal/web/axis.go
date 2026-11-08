package web

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
)

// bpFull is the whole axis in basis points (VIEW-4): widths are integers, so
// no float rounding reaches the page.
const bpFull int64 = 10_000

// minSegmentBP is the narrowest a segment is drawn: 2% of the axis (VIEW-4).
const minSegmentBP int64 = 200

// axisPiece is one piece of the tariff axis: a segment (Class "peak" or
// "offpeak", with its Label and Times) or an empty stretch of the axis
// (Class "track"). WidthBP is its width in basis points.
type axisPiece struct {
	Class, Label, Times string
	WidthBP             int64
}

// Width is the piece's CSS width without the unit: "12.34".
func (a axisPiece) Width() string { return bpPercent(a.WidthBP) }

// wideBP is the width from which a piece has room for its label.
const wideBP int64 = 3_000

// Wide reports whether the piece is wide enough to show its label; narrower
// ones keep it for screen readers only.
func (a axisPiece) Wide() bool { return a.WidthBP >= wideBP }

// axisTick labels a tariff boundary on the axis. AtBP is its distance from
// the axis start in basis points.
type axisTick struct {
	Label string
	AtBP  int64
}

// Left is the tick's CSS offset without the unit: "12.34".
func (a axisTick) Left() string { return bpPercent(a.AtBP) }

// bpPercent writes basis points as a percentage with two decimals:
// 1234 -> "12.34".
func bpPercent(bp int64) string {
	return fmt.Sprintf("%d.%02d", bp/100, bp%100)
}

// boundaries lists the tariff's boundary instants on the local days around
// at: PeakStartHour:00 and PeakEndHour:00, in the tariff's own location, never
// the host's. Two days either side are enough for any instant, since
// consecutive boundaries are at most a day apart.
func boundaries(t billing.Tariff, at time.Time) []time.Time {
	local := at.In(t.Location)
	y, m, d := local.Date()
	var out []time.Time
	for day := d - 2; day <= d+2; day++ {
		out = append(out,
			time.Date(y, m, day, t.PeakStartHour, 0, 0, 0, t.Location),
			time.Date(y, m, day, t.PeakEndHour, 0, 0, 0, t.Location))
	}
	return out
}

// boundaryAtOrBefore is the latest tariff boundary that is not after at
// (VIEW-4): an instant exactly on a boundary is its own answer.
func boundaryAtOrBefore(t billing.Tariff, at time.Time) time.Time {
	var best time.Time
	for _, b := range boundaries(t, at) {
		if !b.After(at) && b.After(best) {
			best = b
		}
	}
	return best
}

// boundaryAtOrAfter is the earliest tariff boundary that is not before at
// (VIEW-4).
func boundaryAtOrAfter(t billing.Tariff, at time.Time) time.Time {
	var best time.Time
	for _, b := range boundaries(t, at) {
		if !b.Before(at) && (best.IsZero() || b.Before(best)) {
			best = b
		}
	}
	return best
}

// widthsBP shares the 10,000 basis points of the axis between pieces in
// proportion to their durations (VIEW-4). An entry marked fixed that would
// come out under minBP is pinned at minBP; every other entry shares what is
// left by the largest-remainder method, so the widths sum to exactly 10,000.
// Pinning can push another fixed entry under minBP, so it repeats until
// nothing changes. Durations are bounded by the session rules (48 h plus two
// boundary gaps), so duration x 10,000 fits int64.
func widthsBP(durations []time.Duration, minBP int64, fixed []bool) []int64 {
	n := len(durations)
	widths := make([]int64, n)
	var sum time.Duration
	for _, d := range durations {
		sum += d
	}
	if sum <= 0 {
		return widths
	}
	pinned := make([]bool, n)
	for {
		var pinnedBP, restNs int64
		for i, d := range durations {
			if pinned[i] {
				pinnedBP += minBP
			} else {
				restNs += int64(d)
			}
		}
		share := proportions(durations, pinned, restNs, bpFull-pinnedBP)
		changed := false
		for i := range durations {
			if pinned[i] {
				widths[i] = minBP
				continue
			}
			widths[i] = share[i]
			if fixed[i] && share[i] < minBP {
				pinned[i], changed = true, true
			}
		}
		if !changed {
			return widths
		}
	}
}

// proportions splits target between the entries that are not skipped, in
// proportion to their durations (total is their sum), as whole numbers that
// sum to exactly target: each gets its floor, and the leftover units go one
// each to the largest remainders, the earliest first on a tie. Skipped
// entries get 0. A zero total or target gives all zeros.
func proportions(durations []time.Duration, skip []bool, total, target int64) []int64 {
	out := make([]int64, len(durations))
	if total <= 0 || target <= 0 {
		return out
	}
	rem := make([]int64, len(durations))
	var given int64
	var order []int
	for i, d := range durations {
		if skip[i] {
			continue
		}
		num := int64(d) * target
		out[i], rem[i] = num/total, num%total
		given += out[i]
		order = append(order, i)
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(rem[b], rem[a]) })
	for _, i := range order {
		if given == target {
			break
		}
		out[i]++
		given++
	}
	return out
}

// axis builds the tariff axis for a session's segments (VIEW-4): it runs from
// the tariff boundary at or before the first segment's start to the boundary
// at or after the last one's end, with the segments filling their share and
// the rest of the axis drawn as empty track. Ticks label the axis ends and
// every tariff boundary between segments; they carry dates when the axis
// spans more than one local day. It returns nil, nil for no segments.
func (p *Pages) axis(segs []billing.Segment) ([]axisPiece, []axisTick) {
	if len(segs) == 0 {
		return nil, nil
	}
	first, last := segs[0].Start, segs[len(segs)-1].End
	from, to := boundaryAtOrBefore(p.Tariff, first), boundaryAtOrAfter(p.Tariff, last)
	datedTicks := !sameDay(from.In(p.Tariff.Location), to.In(p.Tariff.Location))
	datedRows := lasts24h(segs)

	var pieces []axisPiece
	var durations []time.Duration
	var fixed []bool
	add := func(piece axisPiece, d time.Duration, isSegment bool) {
		pieces, durations, fixed = append(pieces, piece), append(durations, d), append(fixed, isSegment)
	}
	if first.After(from) {
		add(axisPiece{Class: "track"}, first.Sub(from), false)
	}
	for _, s := range segs {
		label, class := windowLabel(s.Window)
		add(axisPiece{Class: class, Label: label, Times: p.segmentTimes(s, datedRows)}, s.End.Sub(s.Start), true)
	}
	if last.Before(to) {
		add(axisPiece{Class: "track"}, to.Sub(last), false)
	}
	for i, w := range widthsBP(durations, minSegmentBP, fixed) {
		pieces[i].WidthBP = w
	}

	// A tick at the axis start, after each piece that ends on a tariff
	// boundary, and at the axis end (which is always one).
	ticks := []axisTick{{Label: p.clock(from, datedTicks)}}
	at := from
	var cum int64
	for i, piece := range pieces {
		cum += piece.WidthBP
		at = at.Add(durations[i])
		if i == len(pieces)-1 {
			ticks = append(ticks, axisTick{Label: p.clock(to, datedTicks), AtBP: bpFull})
		} else if isBoundary(p.Tariff, at) {
			ticks = append(ticks, axisTick{Label: p.clock(at, datedTicks), AtBP: cum})
		}
	}
	return pieces, ticks
}

// isBoundary reports whether at is exactly a tariff boundary.
func isBoundary(t billing.Tariff, at time.Time) bool {
	return boundaryAtOrBefore(t, at).Equal(at)
}
