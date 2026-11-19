-- +goose Up
CREATE TABLE IF NOT EXISTS {{prefix}}jobs (
  id BIGSERIAL PRIMARY KEY,
  mode TEXT NOT NULL,
  dry_run BOOLEAN NOT NULL DEFAULT FALSE,
  tables_json TEXT NOT NULL,
  status TEXT NOT NULL,
  imported_count BIGINT NOT NULL DEFAULT 0,
  skipped_count BIGINT NOT NULL DEFAULT 0,
  error_count BIGINT NOT NULL DEFAULT 0,
  report_json TEXT NOT NULL DEFAULT '{}',
  created_by BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
  finished_at TIMESTAMPTZ
);

-- +goose Down
DROP TABLE IF EXISTS {{prefix}}jobs;
