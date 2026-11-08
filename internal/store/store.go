package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/sessions"
)

// Constraint names that a unique violation (SQLSTATE 23505) maps to a rule error.
const (
	uniqueViolation       = "23505"
	activeSessionIndex    = "sessions_one_active_per_charger"
	meterReadingsPKConstr = "meter_readings_pkey"
)

const sessionColumns = `id::text, charger_id, started_at, meter_start_wh, stopped_at, meter_stop_wh`

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Store is the Postgres-backed session store.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store that uses pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// SessionWithReadings is a session with its readings sorted by time.
type SessionWithReadings struct {
	Session  sessions.Session
	Readings []billing.Reading
}

// querier is what both a pool and a transaction offer.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// norm rounds an instant to what Postgres stores: UTC with microseconds. Rule
// checks run on the normalised value so they see exactly what is stored.
func norm(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// CreateSession starts a session (SES-1). It fails with a Conflict
// active_session_exists if the charger already has an active session.
func (s *Store) CreateSession(ctx context.Context, chargerID string, startedAt time.Time, meterStartWh int64, now time.Time) (sessions.Session, error) {
	startedAt = norm(startedAt)
	if err := sessions.CheckStart(chargerID, startedAt, meterStartWh, now); err != nil {
		return sessions.Session{}, err
	}
	sess, err := scanSession(s.pool.QueryRow(ctx,
		`INSERT INTO sessions (charger_id, started_at, meter_start_wh) VALUES ($1, $2, $3) RETURNING `+sessionColumns,
		chargerID, startedAt, meterStartWh))
	if err != nil {
		if rerr := mapUnique(err); rerr != nil {
			return sessions.Session{}, rerr
		}
		return sessions.Session{}, fmt.Errorf("insert session: %w", err)
	}
	return sess, nil
}

// AddReading stores a meter reading (SES-2, SES-6) and returns the session with
// its readings as of the end of the same transaction. The session row is locked
// for the transaction, so concurrent calls for one session run one at a time
// and an identical repeat is reported as duplicate, not stored twice.
func (s *Store) AddReading(ctx context.Context, id string, r billing.Reading, now time.Time) (sessions.Session, []billing.Reading, bool, error) {
	id, ok := parseUUID(id)
	if !ok {
		return sessions.Session{}, nil, false, sessions.ErrNotFound
	}
	r.At = norm(r.At)

	var (
		out       sessions.Session
		readings  []billing.Reading
		duplicate bool
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		sess, rs, err := lockSession(ctx, tx, id)
		if err != nil {
			return err
		}
		dup, err := sessions.CheckReading(sess, rs, r, now)
		if err != nil {
			return err
		}
		out, readings, duplicate = sess, rs, dup
		if dup {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO meter_readings (session_id, at, meter_wh) VALUES ($1, $2, $3)`,
			id, r.At, r.MeterWh); err != nil {
			if rerr := mapUnique(err); rerr != nil {
				return rerr
			}
			return fmt.Errorf("insert reading: %w", err)
		}
		readings, err = loadReadings(ctx, tx, id)
		return err
	})
	if err != nil {
		return sessions.Session{}, nil, false, ruleOrWrap(err, "add reading")
	}
	return out, readings, duplicate, nil
}

// StopSession stops a session (SES-3). Repeating the identical stop returns
// the stored session and readings unchanged.
func (s *Store) StopSession(ctx context.Context, id string, stop billing.Reading, now time.Time) (sessions.Session, []billing.Reading, error) {
	id, ok := parseUUID(id)
	if !ok {
		return sessions.Session{}, nil, sessions.ErrNotFound
	}
	stop.At = norm(stop.At)

	var (
		out      sessions.Session
		readings []billing.Reading
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		sess, rs, err := lockSession(ctx, tx, id)
		if err != nil {
			return err
		}
		replay, err := sessions.CheckStop(sess, rs, stop, now)
		if err != nil {
			return err
		}
		readings = rs
		if replay {
			out = sess
			return nil
		}
		out, err = scanSession(tx.QueryRow(ctx,
			`UPDATE sessions SET stopped_at = $2, meter_stop_wh = $3 WHERE id = $1 RETURNING `+sessionColumns,
			id, stop.At, stop.MeterWh))
		if err != nil {
			return fmt.Errorf("update session: %w", err)
		}
		return nil
	})
	if err != nil {
		return sessions.Session{}, nil, ruleOrWrap(err, "stop session")
	}
	return out, readings, nil
}

// GetSession returns a session and its readings sorted by time, all in UTC.
func (s *Store) GetSession(ctx context.Context, id string) (sessions.Session, []billing.Reading, error) {
	id, ok := parseUUID(id)
	if !ok {
		return sessions.Session{}, nil, sessions.ErrNotFound
	}
	sess, err := scanSession(s.pool.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = $1`, id))
	if err != nil {
		return sessions.Session{}, nil, ruleOrWrap(notFound(err), "get session")
	}
	readings, err := loadReadings(ctx, s.pool, id)
	if err != nil {
		return sessions.Session{}, nil, err
	}
	return sess, readings, nil
}

// ListRecent returns up to limit sessions, newest started_at first, each with
// its readings. Readings for the whole page come from one query.
func (s *Store) ListRecent(ctx context.Context, limit int) ([]SessionWithReadings, error) {
	if limit < 1 {
		return nil, fmt.Errorf("list recent sessions: limit must be at least 1, got %d", limit)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+sessionColumns+` FROM sessions ORDER BY started_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SessionWithReadings, error) {
		sess, err := scanSession(row)
		return SessionWithReadings{Session: sess, Readings: []billing.Reading{}}, err
	})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	if len(list) == 0 {
		return list, nil
	}

	ids := make([]string, len(list))
	index := make(map[string]int, len(list))
	for i, item := range list {
		ids[i] = item.Session.ID
		index[item.Session.ID] = i
	}
	type row struct {
		sessionID string
		reading   billing.Reading
	}
	rrows, err := s.pool.Query(ctx,
		`SELECT session_id::text, at, meter_wh FROM meter_readings WHERE session_id = ANY($1::uuid[]) ORDER BY session_id, at`, ids)
	if err != nil {
		return nil, fmt.Errorf("list readings: %w", err)
	}
	all, err := pgx.CollectRows(rrows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.sessionID, &x.reading.At, &x.reading.MeterWh)
		x.reading.At = x.reading.At.UTC()
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("list readings: %w", err)
	}
	for _, x := range all {
		i, ok := index[x.sessionID]
		if !ok {
			return nil, fmt.Errorf("list readings: unexpected session id %s", x.sessionID)
		}
		list[i].Readings = append(list[i].Readings, x.reading)
	}
	return list, nil
}

// inTx runs fn in a READ COMMITTED transaction and commits it. The
// lock-then-reload design relies on READ COMMITTED: after FOR UPDATE returns,
// the following reads see the work of the transaction that held the lock. A
// rollback failure is joined to the error rather than dropped; errors.As still
// finds a RuleError inside the join.
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// lockSession locks the session row for the rest of the transaction and loads
// its readings sorted by time.
func lockSession(ctx context.Context, tx pgx.Tx, id string) (sessions.Session, []billing.Reading, error) {
	sess, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return sessions.Session{}, nil, notFound(err)
	}
	readings, err := loadReadings(ctx, tx, id)
	if err != nil {
		return sessions.Session{}, nil, err
	}
	return sess, readings, nil
}

func loadReadings(ctx context.Context, q querier, id string) ([]billing.Reading, error) {
	rows, err := q.Query(ctx, `SELECT at, meter_wh FROM meter_readings WHERE session_id = $1 ORDER BY at`, id)
	if err != nil {
		return nil, fmt.Errorf("load readings: %w", err)
	}
	readings, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (billing.Reading, error) {
		var x billing.Reading
		err := r.Scan(&x.At, &x.MeterWh)
		x.At = x.At.UTC()
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("load readings: %w", err)
	}
	return readings, nil
}

func scanSession(row pgx.Row) (sessions.Session, error) {
	var sess sessions.Session
	if err := row.Scan(&sess.ID, &sess.ChargerID, &sess.StartedAt, &sess.MeterStartWh, &sess.StoppedAt, &sess.MeterStopWh); err != nil {
		return sessions.Session{}, err
	}
	sess.StartedAt = sess.StartedAt.UTC()
	if sess.StoppedAt != nil {
		stopped := sess.StoppedAt.UTC()
		sess.StoppedAt = &stopped
	}
	return sess, nil
}

// parseUUID reports whether id is a well-formed UUID and returns it lowercased.
func parseUUID(id string) (string, bool) {
	if !uuidPattern.MatchString(id) {
		return "", false
	}
	return strings.ToLower(id), true
}

// notFound turns "no such row" into sessions.ErrNotFound and leaves other errors alone.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return sessions.ErrNotFound
	}
	return err
}

// mapUnique converts a unique violation on a known constraint into a rule
// error. It returns nil for any other error.
func mapUnique(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != uniqueViolation {
		return nil
	}
	switch pgErr.ConstraintName {
	case activeSessionIndex:
		return &sessions.RuleError{Kind: sessions.KindConflict, Code: "active_session_exists", Message: "charger already has an active session"}
	case meterReadingsPKConstr:
		// Only reachable if the session row lock is ever lost: under FOR UPDATE
		// the rules check reports a duplicate or conflict before the insert.
		return &sessions.RuleError{Kind: sessions.KindConflict, Code: "conflicting_reading", Message: "a different value exists at this timestamp"}
	}
	return nil
}

// ruleOrWrap returns rule errors (including ErrNotFound) unchanged and wraps
// everything else with context.
func ruleOrWrap(err error, what string) error {
	var re *sessions.RuleError
	if errors.As(err, &re) {
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}
