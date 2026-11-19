-- +goose Up
CREATE TABLE IF NOT EXISTS {{prefix}}jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  mode TEXT NOT NULL,
  dry_run INTEGER NOT NULL DEFAULT 0,
  tables_json TEXT NOT NULL,
  status TEXT NOT NULL,
  imported_count INTEGER NOT NULL DEFAULT 0,
  skipped_count INTEGER NOT NULL DEFAULT 0,
  error_count INTEGER NOT NULL DEFAULT 0,
  report_json TEXT NOT NULL DEFAULT '{}',
  created_by BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  finished_at TIMESTAMP
);

-- +goose Down
DROP TABLE IF EXISTS {{prefix}}jobs;
