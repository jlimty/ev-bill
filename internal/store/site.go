package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SiteActivity is what a site's figures are built from (LOC-6, LOC-8).
type SiteActivity struct {
	Sessions    []SessionWithReadings // NAV-2's set, oldest first (started_at, id)
	FirstSample time.Time             // earliest stored occupancy sample, UTC; zero when none
}

// siteSessionsSQL selects the mirror sessions at the chargers of the given
// postal codes ($1), with their chargers row, oldest first. Its filter is
// NAV-2's site filter, so a card counts exactly what its drill-down lists
// (LOC-6), including chargers no longer in the current scope.
const siteSessionsSQL = listSelectSQL + `
WHERE s.source = 'mirror' AND c.postal_code = ANY($1::text[])
ORDER BY s.started_at, s.id`

// firstSamplesSQL is each given site's earliest stored occupancy sample (LOC-8).
const firstSamplesSQL = `SELECT postal_code, min(observed_at)
FROM occupancy_samples WHERE postal_code = ANY($1::text[])
GROUP BY postal_code`

// SiteActivity returns, for every postal code asked for, the site's mirror
// sessions with their readings and its earliest stored sample (LOC-6, LOC-8).
// A postal code with nothing stored maps to an empty SiteActivity. The two
// queries are separate statements, so a poll landing in between can add a
// session or sample that the other did not see; both are bounded by
// retention (MIR-9), not by a window.
func (s *Store) SiteActivity(ctx context.Context, postals []string) (map[string]SiteActivity, error) {
	out := make(map[string]SiteActivity, len(postals))
	for _, p := range postals {
		out[p] = SiteActivity{}
	}
	rows, err := s.pool.Query(ctx, siteSessionsSQL, postals)
	if err != nil {
		return nil, fmt.Errorf("site activity: %w", err)
	}
	items, err := pgx.CollectRows(rows, scanListRow)
	if err != nil {
		return nil, fmt.Errorf("site activity: %w", err)
	}
	if err := attachReadings(ctx, s.pool, items); err != nil {
		return nil, fmt.Errorf("site activity: %w", err)
	}
	for _, it := range items {
		// The WHERE clause matched c.postal_code, so every row has a charger.
		a := out[it.Charger.PostalCode]
		a.Sessions = append(a.Sessions, it)
		out[it.Charger.PostalCode] = a
	}

	rows, err = s.pool.Query(ctx, firstSamplesSQL, postals)
	if err != nil {
		return nil, fmt.Errorf("site activity: %w", err)
	}
	var (
		postal string
		first  time.Time
	)
	_, err = pgx.ForEachRow(rows, []any{&postal, &first}, func() error {
		a := out[postal]
		a.FirstSample = first.UTC()
		out[postal] = a
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("site activity: %w", err)
	}
	return out, nil
}
