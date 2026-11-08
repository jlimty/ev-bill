-- +goose Up
CREATE TABLE chargers (
  charger_id text PRIMARY KEY,
  location_name text NOT NULL, address text NOT NULL, postal_code text NOT NULL,
  operator text NOT NULL, position text NOT NULL, plug_type text NOT NULL,
  current_type text NOT NULL CHECK (current_type IN ('AC', 'DC', '')),
  power_w bigint NOT NULL CHECK (power_w > 0),
  price text NOT NULL, price_type text NOT NULL,
  status smallint NOT NULL, held boolean NOT NULL DEFAULT false,
  observed_at timestamptz NOT NULL
);
CREATE INDEX sessions_open_mirror ON sessions (charger_id) WHERE stopped_at IS NULL AND source = 'mirror';
-- +goose Down
DROP INDEX sessions_open_mirror;
DROP TABLE chargers;
