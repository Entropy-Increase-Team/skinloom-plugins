-- +goose Up
CREATE TABLE IF NOT EXISTS {{prefix}}kv_state (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL,
  updated_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS {{prefix}}sync_outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  action TEXT NOT NULL,
  uuid TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_retry_at BIGINT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS {{prefix}}idx_sync_outbox_retry ON {{prefix}}sync_outbox (next_retry_at);

CREATE TABLE IF NOT EXISTS {{prefix}}webhook_nonces (
  nonce TEXT PRIMARY KEY,
  expires_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS {{prefix}}idx_webhook_nonces_exp ON {{prefix}}webhook_nonces (expires_at);

-- +goose Down
DROP TABLE IF EXISTS {{prefix}}webhook_nonces;
DROP TABLE IF EXISTS {{prefix}}sync_outbox;
DROP TABLE IF EXISTS {{prefix}}kv_state;
