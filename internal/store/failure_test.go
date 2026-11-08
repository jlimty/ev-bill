package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
	"github.com/jlimty/ev-bill/internal/testdb"
)

// API-2: an unknown failure (database down, cancelled, schema broken) must come
// back as a plain error, never a RuleError or ErrNotFound, so the API maps it to
// 500 "internal". Errors are never swallowed.

type storeOp struct {
	name string
	run  func(ctx context.Context, s *store.Store, id string) error
}

func storeOps() []storeOp {
	r := billing.Reading{At: at(10, 5, 0, 0), MeterWh: 10}
	return []storeOp{
		{"Ping", func(c context.Context, s *store.Store, _ string) error { return s.Ping(c) }},
		{"CreateSession", func(c context.Context, s *store.Store, _ string) error {
			_, err := s.CreateSession(c, sessions.SourceAPI, "CH-FAIL", at(10, 0, 0, 0), 0, now)
			return err
		}},
		{"AddReading", func(c context.Context, s *store.Store, id string) error {
			_, _, _, err := s.AddReading(c, sessions.SourceAPI, id, r, now)
			return err
		}},
		{"StopSession", func(c context.Context, s *store.Store, id string) error {
			_, _, err := s.StopSession(c, sessions.SourceAPI, id, r, now)
			return err
		}},
		{"GetSession", func(c context.Context, s *store.Store, id string) error {
			_, _, err := s.GetSession(c, id)
			return err
		}},
		{"ListRecent", func(c context.Context, s *store.Store, _ string) error {
			_, err := s.ListRecent(c, 5)
			return err
		}},
	}
}

func requirePlainError(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: want an error, got nil", name)
		return
	}
	var re *sessions.RuleError
	if errors.As(err, &re) || errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("%s: err = %v, want a plain (non-rule, non-not-found) error", name, err)
	}
}

// liveSession creates a session on a healthy store and returns its id.
func liveSession(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return mustCreate(t, store.New(pool), "CH-1", at(10, 0, 0, 0), 0).ID
}

func TestStoreFailures_API2_CancelledContext(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	id := liveSession(t, pool)
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := store.New(pool)
	for _, op := range storeOps() {
		requirePlainError(t, op.name, op.run(cctx, s, id))
	}
	requirePlainError(t, "Migrate", store.Migrate(cctx, pool))
}

func TestStoreFailures_API2_ClosedPool(t *testing.T) {
	t.Parallel()
	pool := testdb.New(t)
	id := liveSession(t, pool)
	// A second pool from the same config, closed; the test pool stays open so
	// testdb's cleanup still works.
	closed, err := pgxpool.NewWithConfig(ctx(), pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	s := store.New(closed)
	for _, op := range storeOps() {
		requirePlainError(t, op.name, op.run(ctx(), s, id))
	}
	requirePlainError(t, "Migrate", store.Migrate(ctx(), closed))
}

// With a table missing, later steps (lock, load readings, list readings) fail
// even though the connection works.
func TestStoreFailures_API2_BrokenSchema(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		drop  string
		check []string // ops that must fail
	}{
		{"no readings table", "meter_readings", []string{"AddReading", "StopSession", "GetSession", "ListRecent"}},
		{"no sessions table", "sessions CASCADE", []string{"CreateSession", "AddReading", "StopSession", "GetSession", "ListRecent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testdb.New(t)
			id := liveSession(t, pool)
			// A second reading so ListRecent has a session to load readings for.
			if _, _, _, err := store.New(pool).AddReading(ctx(), sessions.SourceAPI, id, billing.Reading{At: at(10, 1, 0, 0), MeterWh: 1}, now); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx(), "DROP TABLE "+tc.drop); err != nil {
				t.Fatal(err)
			}
			s := store.New(pool)
			want := map[string]bool{}
			for _, n := range tc.check {
				want[n] = true
			}
			for _, op := range storeOps() {
				if want[op.name] {
					requirePlainError(t, op.name, op.run(ctx(), s, id))
				}
			}
		})
	}
}

func TestListRecent_RejectsBadLimit(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	requirePlainError(t, "ListRecent(-1)", func() error { _, err := s.ListRecent(ctx(), -1); return err }())
}
