// Package sessions holds the pure rules for charging sessions (SES-1 to SES-5)
// and the status and bill view built from them (BIL-7). It does no I/O: the
// store calls these checks inside a row-locked transaction and the API maps
// RuleError kinds to HTTP codes.
package sessions

import (
	"regexp"
	"sort"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
)

// Session is one charging session as stored. StoppedAt and MeterStopWh are
// both nil while the session is active.
type Session struct {
	ID, ChargerID string
	StartedAt     time.Time
	MeterStartWh  int64
	StoppedAt     *time.Time
	MeterStopWh   *int64
}

// Kind classifies a rule failure; the API maps it to an HTTP status.
type Kind int

const (
	// KindInvalid is a request that breaks a rule on its own terms (422).
	KindInvalid Kind = iota + 1
	// KindConflict is a request that clashes with stored state (409).
	KindConflict
	// KindNotFound is a missing session (404).
	KindNotFound
)

// RuleError is a business-rule failure with a stable machine-readable Code.
type RuleError struct {
	Kind    Kind
	Code    string
	Message string
}

// Error implements error.
func (e *RuleError) Error() string { return e.Code + ": " + e.Message }

// ErrNotFound is returned when a session does not exist.
var ErrNotFound = &RuleError{Kind: KindNotFound, Code: "not_found", Message: "session not found"}

func invalid(code, message string) error {
	return &RuleError{Kind: KindInvalid, Code: code, Message: message}
}

func conflict(code, message string) error {
	return &RuleError{Kind: KindConflict, Code: code, Message: message}
}

// Session statuses (SES-4).
const (
	StatusCharging  = "charging"
	StatusStale     = "stale"
	StatusCompleted = "completed"
)

// Session limits.
const (
	// StaleAfter is how long without a reading before a session is stale (SES-4).
	StaleAfter = 15 * time.Minute
	// MaxDuration is the longest a session may run (SES-5).
	MaxDuration = 48 * time.Hour
	// MaxClockSkew is how far a timestamp may be ahead of the server clock (SES-1, SES-2).
	MaxClockSkew = 5 * time.Minute
)

// MaxMeterWh is the largest accepted meter value, 10^15 Wh (a petawatt-hour,
// far beyond any real charger). Billing's SegmentCost multiplies Wh by a rate
// in int64 and would overflow near 1.5x10^17 Wh; this cap keeps every bill
// far from that limit.
const MaxMeterWh int64 = 1_000_000_000_000_000

// EarliestStart is the earliest started_at accepted (2000-01-01T00:00:00Z).
// Replays of older data are not supported: JSON encoding and the tariff are
// only meaningful for modern dates.
var EarliestStart = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

var chargerIDPattern = regexp.MustCompile(`^[A-Z0-9-]{1,32}$`)

// isStopped reports whether both stop fields are set.
func isStopped(s Session) bool { return s.StoppedAt != nil && s.MeterStopWh != nil }

func meterInRange(wh int64) bool { return wh >= 0 && wh <= MaxMeterWh }

// CheckStart validates a new session (SES-1). Historic start times are
// allowed so that replays of old sessions work, back to EarliestStart.
func CheckStart(chargerID string, startedAt time.Time, meterStartWh int64, now time.Time) error {
	if !chargerIDPattern.MatchString(chargerID) {
		return invalid("invalid_charger_id", "charger_id must be 1 to 32 characters of A-Z, 0-9 or -")
	}
	if !meterInRange(meterStartWh) {
		return invalid("invalid_reading", "meter_start_wh is out of range")
	}
	if startedAt.Before(EarliestStart) {
		return invalid("invalid_reading", "started_at is before 2000-01-01")
	}
	if startedAt.After(now.Add(MaxClockSkew)) {
		return invalid("invalid_reading", "started_at is too far in the future")
	}
	return nil
}

// ValidChargerID reports whether id is a legal charger_id (SES-1).
func ValidChargerID(id string) bool { return chargerIDPattern.MatchString(id) }

// points returns the start point followed by the stored readings (unsorted).
func points(s Session, readings []billing.Reading) []billing.Reading {
	all := make([]billing.Reading, 0, len(readings)+1)
	all = append(all, billing.Reading{At: s.StartedAt, MeterWh: s.MeterStartWh})
	return append(all, readings...)
}

// checkTime applies the time rules shared by readings and stops.
func checkTime(s Session, at, now time.Time) error {
	if at.Before(s.StartedAt) {
		return invalid("invalid_reading", "timestamp is before the session started")
	}
	if at.After(now.Add(MaxClockSkew)) {
		return invalid("invalid_reading", "timestamp is too far in the future")
	}
	if at.After(s.StartedAt.Add(MaxDuration)) {
		return invalid("session_too_long", "session would exceed 48 hours")
	}
	return nil
}

// CheckReading validates a meter value for an active session (SES-2, SES-5).
// duplicate is true for an identical repeat of a stored point, which the
// caller treats as a no-op.
func CheckReading(s Session, readings []billing.Reading, r billing.Reading, now time.Time) (duplicate bool, err error) {
	if isStopped(s) {
		return false, conflict("session_stopped", "session is already stopped")
	}
	if !meterInRange(r.MeterWh) {
		return false, invalid("invalid_reading", "meter_wh is out of range")
	}
	if err := checkTime(s, r.At, now); err != nil {
		return false, err
	}

	// Find the nearest earlier and later points while checking for the same instant.
	var before, after *billing.Reading
	all := points(s, readings)
	for i := range all {
		p := &all[i]
		switch {
		case p.At.Equal(r.At):
			if p.MeterWh == r.MeterWh {
				return true, nil
			}
			return false, conflict("conflicting_reading", "a different value exists at this timestamp")
		case p.At.Before(r.At):
			if before == nil || p.At.After(before.At) {
				before = p
			}
		default:
			if after == nil || p.At.Before(after.At) {
				after = p
			}
		}
	}
	if (before != nil && r.MeterWh < before.MeterWh) || (after != nil && r.MeterWh > after.MeterWh) {
		return false, invalid("invalid_reading", "meter_wh is inconsistent with neighbouring readings")
	}
	return false, nil
}

// CheckStop validates stopping a session (SES-3). replay is true when the
// session is already stopped with exactly this stop.
func CheckStop(s Session, readings []billing.Reading, stop billing.Reading, now time.Time) (replay bool, err error) {
	if isStopped(s) {
		if s.StoppedAt.Equal(stop.At) && *s.MeterStopWh == stop.MeterWh {
			return true, nil
		}
		return false, conflict("stop_conflict", "session is already stopped with a different stop")
	}
	if !meterInRange(stop.MeterWh) {
		return false, invalid("invalid_reading", "meter_stop_wh is out of range")
	}
	if err := checkTime(s, stop.At, now); err != nil {
		return false, err
	}

	all := points(s, readings)
	last := all[0]
	for _, p := range all {
		if p.At.Equal(stop.At) && p.MeterWh != stop.MeterWh {
			return false, conflict("conflicting_reading", "a different value exists at this timestamp")
		}
		if p.At.After(last.At) {
			last = p
		}
	}
	if stop.At.Before(last.At) {
		return false, invalid("invalid_reading", "stop is before the last reading")
	}
	if stop.MeterWh < last.MeterWh {
		return false, invalid("invalid_reading", "meter_stop_wh is below the last reading")
	}
	return false, nil
}

// Timeline returns the points to bill: the start point, the readings sorted by
// time, then the stop point if the session is stopped. A point identical (same
// instant and meter) to the one before it is dropped, so a stop that repeats
// the last reading does not hand billing two equal timestamps. The caller's
// slice is not modified.
func Timeline(s Session, readings []billing.Reading) []billing.Reading {
	all := points(s, readings)
	rest := all[1:]
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].At.Before(rest[j].At) })
	if isStopped(s) {
		all = append(all, billing.Reading{At: *s.StoppedAt, MeterWh: *s.MeterStopWh})
	}

	out := make([]billing.Reading, 0, len(all))
	for _, p := range all {
		if n := len(out); n > 0 && out[n-1].At.Equal(p.At) && out[n-1].MeterWh == p.MeterWh {
			continue
		}
		out = append(out, p)
	}
	return out
}
