-- +goose Up
ALTER TABLE sessions ADD COLUMN source text NOT NULL DEFAULT 'api' CHECK (source IN ('api', 'mirror'));
-- +goose Down
ALTER TABLE sessions DROP COLUMN source;
