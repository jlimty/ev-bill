-- +goose Up
CREATE TABLE occupancy_samples (
  postal_code text NOT NULL,
  observed_at timestamptz NOT NULL,
  occupied integer NOT NULL CHECK (occupied >= 0),
  available integer NOT NULL CHECK (available >= 0),
  unavailable integer NOT NULL CHECK (unavailable >= 0),
  PRIMARY KEY (postal_code, observed_at)
);
CREATE INDEX occupancy_samples_observed_at ON occupancy_samples (observed_at);
-- +goose Down
DROP TABLE occupancy_samples;
