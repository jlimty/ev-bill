// Package mirror decides how a live snapshot of real charger statuses maps to
// billing sessions. This file is pure decision logic: it does no I/O.
package mirror

import (
	"sort"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/sessions"
)

const (
	// DCMaxW caps the assumed draw of a DC connector, in watts (MIR-4).
	DCMaxW int64 = 50_000
	// CapWh is the "battery full" cap per occupancy, in Wh (MIR-4).
	CapWh int64 = 60_000
	// MaxConnectors is the most connectors watched (MIR-2).
	MaxConnectors = 100
)

// ChargerState is what the previous cycle stored for one charger.
type ChargerState struct {
	Status datamall.Status
	// Held is true after a 48 h stop, until the connector is seen not
	// occupied (MIR-12).
	Held bool
}

// OpenSession is an open mirror session. Last is its latest point: the start,
// or the newest reading.
type OpenSession struct {
	ID, ChargerID string
	StartedAt     time.Time
	Last          billing.Reading
}

// ActionKind says what an Action does.
type ActionKind int

const (
	ActStart ActionKind = iota + 1
	ActRead
	ActStop
)

// Action is one step for the caller to apply through the store. For ActStart
// SessionID is empty, because the session does not exist yet.
type Action struct {
	Kind      ActionKind
	ChargerID string
	SessionID string
	At        time.Time
	MeterWh   int64
}

// LastStop describes the latest stopped mirror session of a charger.
type LastStop struct {
	MeterWh int64
	// AtLimit is true when that session lasted the full sessions.MaxDuration.
	AtLimit bool
}

// Input is everything Plan needs.
type Input struct {
	Observed   time.Time
	Connectors []datamall.Connector // already capped at MaxConnectors
	Prev       map[string]ChargerState
	Open       map[string]OpenSession // by ChargerID
	LastStop   map[string]LastStop    // latest stopped mirror session, by ChargerID
	GapAfter   time.Duration
}

// Result is the actions to apply, in order, and the state to store next.
type Result struct {
	Actions []Action
	Next    map[string]ChargerState
}

// PowerW is the power assumed for an occupied connector: its rating, capped at
// DCMaxW for DC (MIR-4).
func PowerW(c datamall.Connector) int64 {
	if c.Current == "DC" && c.PowerW > DCMaxW {
		return DCMaxW
	}
	return c.PowerW
}

// Meter estimates the meter at time at, for a connector drawing powerW since
// last. It is capped at CapWh and never below last.MeterWh (MIR-4).
func Meter(last billing.Reading, at time.Time, powerW int64) int64 {
	secs := int64(at.Sub(last.At) / time.Second)
	if secs < 0 {
		secs = 0
	}
	m := last.MeterWh + powerW*secs/3600
	if m > CapWh {
		m = CapWh
	}
	if m < last.MeterWh {
		m = last.MeterWh
	}
	return m
}

// Plan applies MIR-4, MIR-5, MIR-6 and MIR-12 to one snapshot. Connectors are
// handled in ChargerID order, then stops for unwatched open sessions in
// ChargerID order, so the result is deterministic.
func Plan(in Input) Result {
	res := Result{Next: make(map[string]ChargerState, len(in.Connectors))}

	conns := make([]datamall.Connector, len(in.Connectors))
	copy(conns, in.Connectors)
	sort.Slice(conns, func(i, j int) bool { return conns[i].ChargerID < conns[j].ChargerID })

	watched := make(map[string]bool, len(conns))
	for _, c := range conns {
		watched[c.ChargerID] = true
		occupied := c.Status == datamall.Occupied
		prev := in.Prev[c.ChargerID]
		held := prev.Held && occupied
		p := PowerW(c)

		if s, ok := in.Open[c.ChargerID]; ok {
			res.Actions = append(res.Actions, planOpen(in, c, s, prev, occupied, p, &held)...)
		} else if occupied {
			ls := in.LastStop[c.ChargerID]
			if prev.Held || (prev.Status == datamall.Occupied && ls.AtLimit) {
				// MIR-12: hold, including after a cut cycle lost Held.
				held = true
			} else {
				carry := int64(0)
				if prev.Status == datamall.Occupied {
					carry = ls.MeterWh
				}
				res.Actions = append(res.Actions, start(c.ChargerID, in.Observed, carry))
			}
		}
		res.Next[c.ChargerID] = ChargerState{Status: c.Status, Held: held}
	}

	var unwatched []string
	for id := range in.Open {
		if !watched[id] {
			unwatched = append(unwatched, id)
		}
	}
	sort.Strings(unwatched)
	for _, id := range unwatched {
		s := in.Open[id]
		res.Actions = append(res.Actions, stop(s, s.Last.At, s.Last.MeterWh)) // MIR-6
	}
	return res
}

// planOpen decides the actions for a connector that has an open session.
func planOpen(in Input, c datamall.Connector, s OpenSession, prev ChargerState, occupied bool, p int64, held *bool) []Action {
	// If prev.Held is set with an open session, held stays as it is while occupied.
	switch {
	case in.Observed.Sub(s.Last.At) > in.GapAfter: // MIR-5
		acts := []Action{stop(s, s.Last.At, s.Last.MeterWh)}
		if occupied && !prev.Held {
			acts = append(acts, start(c.ChargerID, in.Observed, s.Last.MeterWh))
		}
		return acts
	case occupied && !in.Observed.Before(s.StartedAt.Add(sessions.MaxDuration)): // MIR-12
		end := s.StartedAt.Add(sessions.MaxDuration)
		*held = true
		return []Action{stop(s, end, Meter(s.Last, end, p))}
	case occupied:
		return []Action{{Kind: ActRead, ChargerID: c.ChargerID, SessionID: s.ID, At: in.Observed, MeterWh: Meter(s.Last, in.Observed, p)}}
	default:
		// MIR-12: the store rejects a stop past started+48h, so clamp.
		end := in.Observed
		if limit := s.StartedAt.Add(sessions.MaxDuration); end.After(limit) {
			end = limit
		}
		return []Action{stop(s, end, Meter(s.Last, end, p))}
	}
}

func start(chargerID string, at time.Time, meter int64) Action {
	return Action{Kind: ActStart, ChargerID: chargerID, At: at, MeterWh: meter}
}

func stop(s OpenSession, at time.Time, meter int64) Action {
	return Action{Kind: ActStop, ChargerID: s.ChargerID, SessionID: s.ID, At: at, MeterWh: meter}
}
