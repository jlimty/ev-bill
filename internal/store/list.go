package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/locations"
)

// SessionSort is the column the session list is sorted on (NAV-3).
type SessionSort int

const (
	SortStarted SessionSort = iota // started_at
	SortEnergy                     // energy in Wh, as energyWhSQL computes it
)

// ListCursor is the boundary row of a page. Only the field of the query's sort
// is read: StartedAt for SortStarted, EnergyWh for SortEnergy. ID breaks ties.
type ListCursor struct {
	StartedAt time.Time
	EnergyWh  int64
	ID        string
}

// ListQuery asks for one page of the session list (NAV-2, NAV-3, NAV-4).
type ListQuery struct {
	Site   string      // "" = all sessions; else mirror sessions whose charger has a chargers row with this postal code
	Sort   SessionSort // the sort column
	Desc   bool        // the display direction
	After  *ListCursor // rows after the cursor in display order
	Before *ListCursor // rows before it; at most one of After, Before
	Limit  int         // page size, at least 1
}

// ListPage is one page in display order. HasPrev and HasNext say whether rows
// exist before and after it; Total counts the whole filter, ignoring cursors.
type ListPage struct {
	Items            []SessionWithReadings
	HasPrev, HasNext bool
	Total            int64
}

// listSelectSQL selects sessions with their chargers row, if any (LIVE-4). The
// LEFT JOIN keeps sessions whose charger ID has no row; for them every c.*
// column is NULL, so has_charger is false and COALESCE turns the NULLs into
// zero values that scanCharger's destinations can hold. The zero values are
// never used: Charger stays nil. ListRecent and ListSessions share it.
const listSelectSQL = `SELECT s.id::text, s.charger_id, s.source, s.started_at, s.meter_start_wh, s.stopped_at, s.meter_stop_wh,
	c.charger_id IS NOT NULL AS has_charger,
	COALESCE(c.charger_id, ''), COALESCE(c.location_name, ''), COALESCE(c.address, ''), COALESCE(c.postal_code, ''),
	COALESCE(c.operator, ''), COALESCE(c.position, ''), COALESCE(c.plug_type, ''), COALESCE(c.current_type, ''),
	COALESCE(c.power_w, 0), COALESCE(c.price, ''), COALESCE(c.price_type, ''), COALESCE(c.status, 0),
	COALESCE(c.held, false), COALESCE(c.observed_at, 'epoch'::timestamptz)
FROM sessions s LEFT JOIN chargers c ON c.charger_id = s.charger_id`

// ListSessions returns one page of sessions in display order, each with its
// readings and its chargers row, if any (NAV-2, NAV-3, NAV-4). It fetches
// Limit+1 rows; the extra row only says whether the page has a neighbour in the
// direction it was read. A Before cursor reads the reversed order and then
// reverses the rows, so the page is the one just before the cursor. The count
// and the page are separate statements, so Total can briefly disagree with the
// page when a poll lands in between.
func (s *Store) ListSessions(ctx context.Context, q ListQuery) (ListPage, error) {
	if q.Limit < 1 {
		return ListPage{}, fmt.Errorf("list sessions: limit must be at least 1, got %d", q.Limit)
	}
	if q.After != nil && q.Before != nil {
		return ListPage{}, fmt.Errorf("list sessions: at most one of After and Before")
	}
	if q.Sort != SortStarted && q.Sort != SortEnergy {
		return ListPage{}, fmt.Errorf("list sessions: unknown sort %d", q.Sort)
	}

	var args []any
	where := "TRUE"
	if q.Site != "" {
		args = append(args, q.Site)
		where = fmt.Sprintf("s.source = 'mirror' AND c.postal_code = $%d", len(args))
	}
	var total int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sessions s LEFT JOIN chargers c ON c.charger_id = s.charger_id WHERE `+where, args...).Scan(&total); err != nil {
		return ListPage{}, fmt.Errorf("count sessions: %w", err)
	}

	// The query reads descending when the display is, unless Before reverses it.
	desc := q.Desc != (q.Before != nil)
	key, cast := "s.started_at", "timestamptz"
	if q.Sort == SortEnergy {
		key, cast = energyWhSQL, "bigint"
	}
	if cur := boundary(q); cur != nil {
		op := ">"
		if desc {
			op = "<"
		}
		var keyArg any = norm(cur.StartedAt)
		if q.Sort == SortEnergy {
			keyArg = cur.EnergyWh
		}
		args = append(args, keyArg, cur.ID)
		where += fmt.Sprintf(" AND (%s, s.id) %s ($%d::%s, $%d::uuid)", key, op, len(args)-1, cast, len(args))
	}
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	args = append(args, q.Limit+1)
	sql := fmt.Sprintf("%s WHERE %s ORDER BY %s %s, s.id %s LIMIT $%d", listSelectSQL, where, key, dir, dir, len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return ListPage{}, fmt.Errorf("list sessions: %w", err)
	}
	items, err := pgx.CollectRows(rows, scanListRow)
	if err != nil {
		return ListPage{}, fmt.Errorf("list sessions: %w", err)
	}
	more := len(items) > q.Limit
	if more {
		items = items[:q.Limit]
	}
	if q.Before != nil {
		slices.Reverse(items)
	}
	if err := attachReadings(ctx, s.pool, items); err != nil {
		return ListPage{}, fmt.Errorf("list sessions: %w", err)
	}

	page := ListPage{Items: items, Total: total}
	switch {
	case q.Before != nil:
		page.HasNext, page.HasPrev = true, more
	case q.After != nil:
		page.HasPrev, page.HasNext = true, more
	default:
		page.HasNext = more
	}
	return page, nil
}

// boundary returns the query's cursor, or nil.
func boundary(q ListQuery) *ListCursor {
	if q.After != nil {
		return q.After
	}
	return q.Before
}

// scanListRow scans one row of listSelectSQL.
func scanListRow(row pgx.CollectableRow) (SessionWithReadings, error) {
	var (
		item       = SessionWithReadings{Readings: []billing.Reading{}}
		hasCharger bool
		c          ChargerRow
		status     int16
	)
	dest := append(sessionFields(&item.Session), &hasCharger)
	if err := row.Scan(append(dest, chargerFields(&c, &status)...)...); err != nil {
		return SessionWithReadings{}, err
	}
	utcSession(&item.Session)
	if hasCharger {
		finishCharger(&c, status)
		item.Charger = &c
	}
	return item, nil
}

// SiteName returns the name of the site with this postal code (VIEW-5): the
// location name most chargers rows share, taken from the site's own newest
// observation, so a site that has left the current scope still has a name.
// found is false when no chargers row has the postal code.
func (s *Store) SiteName(ctx context.Context, postal string) (name string, found bool, err error) {
	rows, err := s.pool.Query(ctx,
		`SELECT location_name FROM chargers
WHERE postal_code = $1 AND observed_at = (SELECT max(observed_at) FROM chargers WHERE postal_code = $1)`, postal)
	if err != nil {
		return "", false, fmt.Errorf("site name: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", false, fmt.Errorf("site name: %w", err)
	}
	if len(names) == 0 {
		return "", false, nil
	}
	name, _ = locations.SiteNames(names)
	return name, true, nil
}
