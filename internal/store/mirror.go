package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/datamall"
)

// ChargerRow is one row of the chargers table: a watched connector's details
// and status from the latest poll, whether it is held after a 48 h stop
// (MIR-12), and that poll's observation time (MIR-3, MIR-8).
type ChargerRow struct {
	datamall.Connector
	Held       bool
	ObservedAt time.Time
}

// MirrorStop is the latest stopped mirror session of a charger (MIR-4, MIR-12).
type MirrorStop struct {
	MeterWh              int64
	StartedAt, StoppedAt time.Time
}

const chargerColumns = `charger_id, location_name, address, postal_code, operator, position, plug_type,
	current_type, power_w, price, price_type, status, held, observed_at`

// upsertChargerSQL inserts or updates one charger by charger_id (MIR-8). The
// WHERE clause keeps a row whose observed_at is newer, so an older snapshot
// never moves details or the latest observation backwards (MIR-3).
const upsertChargerSQL = `INSERT INTO chargers (` + chargerColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (charger_id) DO UPDATE SET
	location_name = EXCLUDED.location_name, address = EXCLUDED.address,
	postal_code = EXCLUDED.postal_code, operator = EXCLUDED.operator,
	position = EXCLUDED.position, plug_type = EXCLUDED.plug_type,
	current_type = EXCLUDED.current_type, power_w = EXCLUDED.power_w,
	price = EXCLUDED.price, price_type = EXCLUDED.price_type,
	status = EXCLUDED.status, held = EXCLUDED.held, observed_at = EXCLUDED.observed_at
WHERE chargers.observed_at <= EXCLUDED.observed_at`

// UpsertChargers writes rows in one transaction: either every row is stored or
// none is. Chargers missing from rows are left as they are.
func (s *Store) UpsertChargers(ctx context.Context, rows []ChargerRow) error {
	if len(rows) == 0 {
		return nil
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec(ctx, upsertChargerSQL,
				r.ChargerID, r.LocationName, r.Address, r.PostalCode, r.Operator, r.Position, r.PlugType,
				r.Current, r.PowerW, r.Price, r.PriceType, int(r.Status), r.Held, norm(r.ObservedAt)); err != nil {
				return fmt.Errorf("charger %s: %w", r.ChargerID, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert chargers: %w", err)
	}
	return nil
}

// ChargerStates returns every charger row, keyed by charger_id.
func (s *Store) ChargerStates(ctx context.Context) (map[string]ChargerRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+chargerColumns+` FROM chargers`)
	if err != nil {
		return nil, fmt.Errorf("charger states: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChargerRow, error) { return scanCharger(row) })
	if err != nil {
		return nil, fmt.Errorf("charger states: %w", err)
	}
	out := make(map[string]ChargerRow, len(list))
	for _, r := range list {
		out[r.ChargerID] = r
	}
	return out, nil
}

// Charger returns one charger row. found is false if there is none.
func (s *Store) Charger(ctx context.Context, chargerID string) (row ChargerRow, found bool, err error) {
	row, err = scanCharger(s.pool.QueryRow(ctx, `SELECT `+chargerColumns+` FROM chargers WHERE charger_id = $1`, chargerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ChargerRow{}, false, nil
	}
	if err != nil {
		return ChargerRow{}, false, fmt.Errorf("get charger: %w", err)
	}
	return row, true, nil
}

// LatestObservation returns the newest observed_at (MIR-3). ok is false when
// no charger has been stored yet.
func (s *Store) LatestObservation(ctx context.Context) (latest time.Time, ok bool, err error) {
	var t *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT max(observed_at) FROM chargers`).Scan(&t); err != nil {
		return time.Time{}, false, fmt.Errorf("latest observation: %w", err)
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return t.UTC(), true, nil
}

// HeldCount returns how many chargers are held after a 48 h stop (MIR-12),
// among those the latest poll observed: rows whose observed_at is the newest
// in the table. Rows for chargers that have left scope are not counted.
func (s *Store) HeldCount(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM chargers WHERE held AND observed_at = (SELECT max(observed_at) FROM chargers)`).Scan(&n); err != nil {
		return 0, fmt.Errorf("held count: %w", err)
	}
	return n, nil
}

// OpenMirrorSessions returns every open mirror session with its readings
// sorted by time (MIR-6). API sessions are never included (MIR-7).
func (s *Store) OpenMirrorSessions(ctx context.Context) ([]SessionWithReadings, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE stopped_at IS NULL AND source = 'mirror' ORDER BY charger_id`)
	if err != nil {
		return nil, fmt.Errorf("open mirror sessions: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SessionWithReadings, error) {
		sess, err := scanSession(row)
		return SessionWithReadings{Session: sess, Readings: []billing.Reading{}}, err
	})
	if err != nil {
		return nil, fmt.Errorf("open mirror sessions: %w", err)
	}
	if err := attachReadings(ctx, s.pool, list); err != nil {
		return nil, fmt.Errorf("open mirror sessions: %w", err)
	}
	return list, nil
}

// LastMirrorStops returns, per charger, the stopped mirror session with the
// newest stopped_at (MIR-4 carry-over, MIR-12 hold).
func (s *Store) LastMirrorStops(ctx context.Context) (map[string]MirrorStop, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (charger_id) charger_id, meter_stop_wh, started_at, stopped_at
		FROM sessions
		WHERE source = 'mirror' AND stopped_at IS NOT NULL
		ORDER BY charger_id, stopped_at DESC, started_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("last mirror stops: %w", err)
	}
	out := map[string]MirrorStop{}
	var (
		id   string
		stop MirrorStop
	)
	_, err = pgx.ForEachRow(rows, []any{&id, &stop.MeterWh, &stop.StartedAt, &stop.StoppedAt}, func() error {
		out[id] = MirrorStop{MeterWh: stop.MeterWh, StartedAt: stop.StartedAt.UTC(), StoppedAt: stop.StoppedAt.UTC()}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("last mirror stops: %w", err)
	}
	return out, nil
}

// DeleteCompletedMirrorBefore deletes mirror sessions stopped before cutoff,
// with their readings (ON DELETE CASCADE), and returns how many sessions it
// deleted (MIR-9). Open sessions and API sessions are never deleted.
func (s *Store) DeleteCompletedMirrorBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM sessions WHERE source = 'mirror' AND stopped_at IS NOT NULL AND stopped_at < $1`, norm(cutoff))
	if err != nil {
		return 0, fmt.Errorf("delete completed mirror sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func scanCharger(row pgx.Row) (ChargerRow, error) {
	var (
		r      ChargerRow
		status int16
	)
	if err := row.Scan(chargerFields(&r, &status)...); err != nil {
		return ChargerRow{}, err
	}
	finishCharger(&r, status)
	return r, nil
}

// chargerFields returns the scan destinations for chargerColumns, in order.
// The status column goes to status; finishCharger then copies it into r.
func chargerFields(r *ChargerRow, status *int16) []any {
	return []any{&r.ChargerID, &r.LocationName, &r.Address, &r.PostalCode, &r.Operator, &r.Position, &r.PlugType,
		&r.Current, &r.PowerW, &r.Price, &r.PriceType, status, &r.Held, &r.ObservedAt}
}

// finishCharger completes a scanned row: the status and the time in UTC.
func finishCharger(r *ChargerRow, status int16) {
	r.Status = datamall.Status(status)
	r.ObservedAt = r.ObservedAt.UTC()
}
