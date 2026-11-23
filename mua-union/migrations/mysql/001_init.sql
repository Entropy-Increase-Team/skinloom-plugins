-- +goose Up
CREATE TABLE IF NOT EXISTS {{prefix}}kv_state (
  k VARCHAR(128) PRIMARY KEY,
  v LONGTEXT NOT NULL,
  updated_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS {{prefix}}sync_outbox (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  action VARCHAR(32) NOT NULL,
  uuid VARCHAR(64) NOT NULL,
  name VARCHAR(128) NOT NULL DEFAULT '',
  attempts INT NOT NULL DEFAULT 0,
  next_retry_at BIGINT NOT NULL,
  last_error TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  INDEX {{prefix}}idx_sync_outbox_retry (next_retry_at)
);

CREATE TABLE IF NOT EXISTS {{prefix}}webhook_nonces (
  nonce VARCHAR(191) PRIMARY KEY,
  expires_at BIGINT NOT NULL,
  INDEX {{prefix}}idx_webhook_nonces_exp (expires_at)
);

-- +goose Down
DROP TABLE IF EXISTS {{prefix}}webhook_nonces;
DROP TABLE IF EXISTS {{prefix}}sync_outbox;
DROP TABLE IF EXISTS {{prefix}}kv_state;
