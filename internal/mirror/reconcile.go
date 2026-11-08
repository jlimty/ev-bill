package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
)

// Log messages, named so tests can count them.
const (
	msgAPISession   = "mirror: skipping charger with an active API session"
	msgSkipped      = "mirror: connector left out by the feed parser"
	msgBadID        = "mirror: connector ignored, charger ID not allowed by SES-1"
	msgOverCap      = "mirror: connectors over the cap ignored"
	msgNoConnectors = "mirror: the watched postal codes select no connectors"
	msgPollFailed   = "mirror: poll failed"
)

// ErrFutureSnapshot is returned by Apply for a feed timestamp more than
// sessions.MaxClockSkew ahead of the server clock (MIR-3). The snapshot is
// skipped, and the poller counts it as a failed poll, not a good one.
var ErrFutureSnapshot = errors.New("mirror: feed timestamp is more than 5 min in the future")

// MirrorStore is the part of the store the reconciler uses.
type MirrorStore interface {
	LatestObservation(ctx context.Context) (time.Time, bool, error)
	ChargerStates(ctx context.Context) (map[string]store.ChargerRow, error)
	OpenMirrorSessions(ctx context.Context) ([]store.SessionWithReadings, error)
	LastMirrorStops(ctx context.Context) (map[string]store.MirrorStop, error)
	CreateSession(ctx context.Context, as sessions.Source, chargerID string, startedAt time.Time, meterStartWh int64, now time.Time) (sessions.Session, error)
	AddReading(ctx context.Context, as sessions.Source, id string, r billing.Reading, now time.Time) (sessions.Session, []billing.Reading, bool, error)
	StopSession(ctx context.Context, as sessions.Source, id string, stop billing.Reading, now time.Time) (sessions.Session, []billing.Reading, error)
	UpsertChargers(ctx context.Context, rows []store.ChargerRow) error
}

// Reconciler applies one snapshot at a time to the database (MIR-3..8). It
// keeps no state between snapshots except which warnings it has already
// logged: everything it decides from is read from the database.
type Reconciler struct {
	Store    MirrorStore
	Now      func() time.Time
	Logger   *slog.Logger
	GapAfter time.Duration

	mu     sync.Mutex
	logged map[string]bool
}

// Apply reconciles snap with the database. changed is true when it wrote a
// session or any watched charger's details or state changed. A failed write
// aborts the cycle before observed_at is stored, so the next poll retries the
// same file in full; every step is idempotent (MIR-6).
func (r *Reconciler) Apply(ctx context.Context, snap datamall.Snapshot) (changed bool, err error) {
	observed := snap.UpdatedAt
	latest, ok, err := r.Store.LatestObservation(ctx)
	if err != nil {
		return false, fmt.Errorf("reconcile: %w", err)
	}
	if ok && !observed.After(latest) {
		return false, nil // MIR-3: repeated or older file
	}
	now := r.Now()
	if observed.After(now.Add(sessions.MaxClockSkew)) {
		// MIR-3, SES-2: skipped, and reported so the poller does not count it
		// as a good poll.
		return false, fmt.Errorf("%w: observed %s, server clock %s",
			ErrFutureSnapshot, observed.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}

	conns := r.scope(snap)
	in, prev, err := r.load(ctx, observed, conns)
	if err != nil {
		return false, err
	}
	res := Plan(in)

	applied := 0
	for _, a := range res.Actions {
		done, err := r.do(ctx, a, now)
		if err != nil {
			return false, err
		}
		if done {
			applied++
		}
	}

	// Written last: a cycle cut off above leaves observed_at where it was.
	rows := make([]store.ChargerRow, 0, len(conns))
	detailsChanged := false
	for _, c := range conns {
		row := store.ChargerRow{Connector: c, Held: res.Next[c.ChargerID].Held, ObservedAt: observed}
		if old, ok := prev[c.ChargerID]; !ok || old.Connector != row.Connector || old.Held != row.Held {
			detailsChanged = true
		}
		rows = append(rows, row)
	}
	if err := r.Store.UpsertChargers(ctx, rows); err != nil {
		return false, fmt.Errorf("reconcile: %w", err)
	}
	return applied > 0 || detailsChanged, nil
}

// scope returns the connectors to mirror: those with a charger ID that SES-1
// accepts, capped at MaxConnectors (MIR-2). The snapshot is sorted by charger
// ID, so the cap always drops the same connectors. Each warning is logged once.
func (r *Reconciler) scope(snap datamall.Snapshot) []datamall.Connector {
	for _, id := range snap.Skipped {
		if r.once("skipped:" + id) {
			r.Logger.Warn(msgSkipped, "charger_id", id)
		}
	}
	conns := make([]datamall.Connector, 0, len(snap.Connectors))
	for _, c := range snap.Connectors {
		if !sessions.ValidChargerID(c.ChargerID) {
			if r.once("bad-id:" + c.ChargerID) {
				r.Logger.Warn(msgBadID, "charger_id", c.ChargerID)
			}
			continue
		}
		conns = append(conns, c)
	}
	if len(conns) > MaxConnectors {
		if r.once("over-cap") {
			r.Logger.Warn(msgOverCap, "watched", MaxConnectors, "ignored", len(conns)-MaxConnectors)
		}
		conns = conns[:MaxConnectors]
	}
	if len(conns) == 0 && r.once("no-connectors") {
		r.Logger.Warn(msgNoConnectors)
	}
	return conns
}

// load reads the planner's input from the database. prev is the charger rows
// as stored, for spotting detail changes.
func (r *Reconciler) load(ctx context.Context, observed time.Time, conns []datamall.Connector) (Input, map[string]store.ChargerRow, error) {
	prev, err := r.Store.ChargerStates(ctx)
	if err != nil {
		return Input{}, nil, fmt.Errorf("reconcile: %w", err)
	}
	openList, err := r.Store.OpenMirrorSessions(ctx)
	if err != nil {
		return Input{}, nil, fmt.Errorf("reconcile: %w", err)
	}
	stops, err := r.Store.LastMirrorStops(ctx)
	if err != nil {
		return Input{}, nil, fmt.Errorf("reconcile: %w", err)
	}

	in := Input{
		Observed:   observed,
		Connectors: conns,
		Prev:       make(map[string]ChargerState, len(prev)),
		Open:       make(map[string]OpenSession, len(openList)),
		LastStop:   make(map[string]LastStop, len(stops)),
		GapAfter:   r.GapAfter,
	}
	for id, row := range prev {
		in.Prev[id] = ChargerState{Status: row.Status, Held: row.Held}
	}
	for _, sw := range openList {
		tl := sessions.Timeline(sw.Session, sw.Readings) // start point, then readings by time
		in.Open[sw.Session.ChargerID] = OpenSession{
			ID: sw.Session.ID, ChargerID: sw.Session.ChargerID, StartedAt: sw.Session.StartedAt, Last: tl[len(tl)-1],
		}
	}
	for id, s := range stops {
		// A stop belongs to the current occupancy only if it is at or after the
		// charger's last stored observation. A gap stop sits at the last point,
		// which is that poll's observed_at; a 48 h stop falls after it. An older
		// stop is from an earlier occupancy, for example before a MIR-7 skip or
		// before the charger left scope, and must neither carry its meter
		// (MIR-4) nor hold the charger (MIR-12).
		row, ok := prev[id]
		if !ok || s.StoppedAt.Before(row.ObservedAt) {
			continue
		}
		in.LastStop[id] = LastStop{MeterWh: s.MeterWh, AtLimit: s.StoppedAt.Sub(s.StartedAt) >= sessions.MaxDuration}
	}
	return in, prev, nil
}

// do applies one action as the mirror. done is false when a start was skipped
// because an API session is active on that charger (MIR-7).
func (r *Reconciler) do(ctx context.Context, a Action, now time.Time) (done bool, err error) {
	point := billing.Reading{At: a.At, MeterWh: a.MeterWh}
	switch a.Kind {
	case ActStart:
		_, err = r.Store.CreateSession(ctx, sessions.SourceMirror, a.ChargerID, a.At, a.MeterWh, now)
		var re *sessions.RuleError
		if errors.As(err, &re) && re.Code == "active_session_exists" {
			if r.once("api:" + a.ChargerID) {
				r.Logger.Info(msgAPISession, "charger_id", a.ChargerID)
			}
			return false, nil
		}
		if err == nil {
			r.forget("api:" + a.ChargerID) // log again if an API session returns later
		}
	case ActRead:
		_, _, _, err = r.Store.AddReading(ctx, sessions.SourceMirror, a.SessionID, point, now)
	case ActStop:
		_, _, err = r.Store.StopSession(ctx, sessions.SourceMirror, a.SessionID, point, now)
	default:
		err = fmt.Errorf("unknown action kind %d", a.Kind)
	}
	if err != nil {
		return false, fmt.Errorf("reconcile %s: %w", a.ChargerID, err)
	}
	return true, nil
}

// once reports whether key has not been seen before, and marks it seen.
func (r *Reconciler) once(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logged[key] {
		return false
	}
	if r.logged == nil {
		r.logged = map[string]bool{}
	}
	r.logged[key] = true
	return true
}

func (r *Reconciler) forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.logged, key)
}
