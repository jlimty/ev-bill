-- +goose Up
CREATE TABLE sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  charger_id text NOT NULL,
  started_at timestamptz NOT NULL,
  meter_start_wh bigint NOT NULL CHECK (meter_start_wh >= 0),
  stopped_at timestamptz,
  meter_stop_wh bigint CHECK (meter_stop_wh >= 0),
  -- A stop is always both fields or neither (SES-3).
  CONSTRAINT sessions_stop_fields_together CHECK ((stopped_at IS NULL) = (meter_stop_wh IS NULL))
);
CREATE UNIQUE INDEX sessions_one_active_per_charger ON sessions (charger_id) WHERE stopped_at IS NULL;
CREATE INDEX sessions_started_at ON sessions (started_at DESC);
CREATE TABLE meter_readings (
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  at timestamptz NOT NULL,
  meter_wh bigint NOT NULL CHECK (meter_wh >= 0),
  PRIMARY KEY (session_id, at)
);

-- +goose Down
DROP TABLE meter_readings;
DROP TABLE sessions;
