package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Entropy-Increase-Team/skinloom/sdk/go/plugindb"
)

const (
	kvServerList        = "server_list"
	kvServerListVersion = "server_list_version"
	kvPrivateKeyVersion = "private_key_version"
	kvRuntimeMemberKey  = "runtime_member_key"
	kvLastSettingKey    = "last_setting_member_key"
	kvOAuth2PrivKey     = "oauth2_sig_private_key"
	kvOAuth2PubKey      = "oauth2_sig_public_key"
)

type outboxItem struct {
	ID          int64  `json:"id"`
	Action      string `json:"action"` // create | rename | delete
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Attempts    int    `json:"attempts"`
	NextRetryAt int64  `json:"nextRetryAt"`
	LastError   string `json:"lastError"`
	CreatedAt   int64  `json:"createdAt"`
}

func getKV(ctx context.Context, db *plugindb.DB, key string) (string, bool, error) {
	if db == nil {
		return "", false, errors.New("database unavailable")
	}
	tbl := db.Table("kv_state")
	q := db.Rebind(fmt.Sprintf("SELECT v FROM %s WHERE k = ?", tbl))
	var val string
	err := db.SQL().QueryRowContext(ctx, q, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

func setKV(ctx context.Context, db *plugindb.DB, key, value string) error {
	if db == nil {
		return errors.New("database unavailable")
	}
	tbl := db.Table("kv_state")
	now := time.Now().Unix()
	var q string
	if db.Dialect() == "mysql" {
		q = fmt.Sprintf(
			"INSERT INTO %s (k, v, updated_at) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE v = VALUES(v), updated_at = VALUES(updated_at)",
			tbl,
		)
	} else {
		q = db.Rebind(fmt.Sprintf(
			"INSERT INTO %s (k, v, updated_at) VALUES (?, ?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v, updated_at = excluded.updated_at",
			tbl,
		))
	}
	_, err := db.SQL().ExecContext(ctx, q, key, value, now)
	return err
}

// insertKVIfAbsent atomically inserts (key, value) only when key does not already exist.
// Returns true if this caller inserted the row, false if an existing row won the race.
func insertKVIfAbsent(ctx context.Context, db *plugindb.DB, key, value string, now int64) (bool, error) {
	if db == nil {
		return false, errors.New("database unavailable")
	}
	if now <= 0 {
		now = time.Now().Unix()
	}
	tbl := db.Table("kv_state")
	var q string
	switch db.Dialect() {
	case "mysql":
		q = fmt.Sprintf("INSERT IGNORE INTO %s (k, v, updated_at) VALUES (?, ?, ?)", tbl)
	case "postgres":
		q = db.Rebind(fmt.Sprintf("INSERT INTO %s (k, v, updated_at) VALUES (?, ?, ?) ON CONFLICT (k) DO NOTHING", tbl))
	default:
		q = fmt.Sprintf("INSERT OR IGNORE INTO %s (k, v, updated_at) VALUES (?, ?, ?)", tbl)
	}
	res, err := db.SQL().ExecContext(ctx, q, key, value, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func deleteKV(ctx context.Context, db *plugindb.DB, key string) error {
	if db == nil {
		return errors.New("database unavailable")
	}
	tbl := db.Table("kv_state")
	q := db.Rebind(fmt.Sprintf("DELETE FROM %s WHERE k = ?", tbl))
	_, err := db.SQL().ExecContext(ctx, q, key)
	return err
}

// recordNonceIfNew atomically stores nonce with expiresAt if not currently active.
// Returns true if the nonce was accepted (not replayed), false if already seen.
func recordNonceIfNew(ctx context.Context, db *plugindb.DB, nonce string, now, expiresAt int64) (bool, error) {
	if db == nil {
		return false, errors.New("database unavailable")
	}
	tbl := db.Table("webhook_nonces")
	// Clear an expired row for this exact nonce if present so it does not block forever.
	delQ := db.Rebind(fmt.Sprintf("DELETE FROM %s WHERE nonce = ? AND expires_at <= ?", tbl))
	if _, err := db.SQL().ExecContext(ctx, delQ, nonce, now); err != nil {
		return false, err
	}

	var insQ string
	switch db.Dialect() {
	case "mysql":
		insQ = fmt.Sprintf("INSERT IGNORE INTO %s (nonce, expires_at) VALUES (?, ?)", tbl)
	case "postgres":
		insQ = db.Rebind(fmt.Sprintf("INSERT INTO %s (nonce, expires_at) VALUES (?, ?) ON CONFLICT (nonce) DO NOTHING", tbl))
	default:
		insQ = fmt.Sprintf("INSERT OR IGNORE INTO %s (nonce, expires_at) VALUES (?, ?)", tbl)
	}
	res, err := db.SQL().ExecContext(ctx, insQ, nonce, expiresAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func pruneExpiredNonces(ctx context.Context, db *plugindb.DB, now int64) error {
	if db == nil {
		return nil
	}
	tbl := db.Table("webhook_nonces")
	q := db.Rebind(fmt.Sprintf("DELETE FROM %s WHERE expires_at <= ?", tbl))
	_, err := db.SQL().ExecContext(ctx, q, now)
	return err
}

func hasPendingOutboxForUUID(ctx context.Context, db *plugindb.DB, uuid string) (bool, error) {
	if db == nil {
		return false, nil
	}
	tbl := db.Table("sync_outbox")
	q := db.Rebind(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE uuid = ? AND attempts < 10", tbl))
	var cnt int
	if err := db.SQL().QueryRowContext(ctx, q, uuid).Scan(&cnt); err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// enqueueOutboxCoalesced queues a player lifecycle sync while preserving strict
// per-UUID causal order:
//   - delete supersedes any pending create/rename rows for the same UUID;
//   - rename updates an existing pending create/rename row's latest name in place;
//   - otherwise inserts a new retry entry.
func enqueueOutboxCoalesced(ctx context.Context, db *plugindb.DB, action, uuid, name, lastErr string, now int64) error {
	if db == nil {
		return errors.New("database unavailable")
	}
	tbl := db.Table("sync_outbox")
	if now <= 0 {
		now = time.Now().Unix()
	}
	nextRetry := now + 60 // first retry after 1 minute
	errText := truncateStr(lastErr, 500)

	switch action {
	case "delete":
		delQ := db.Rebind(fmt.Sprintf("DELETE FROM %s WHERE uuid = ? AND action IN ('create', 'rename')", tbl))
		_, _ = db.SQL().ExecContext(ctx, delQ, uuid)
	case "rename":
		updQ := db.Rebind(fmt.Sprintf(
			"UPDATE %s SET name = ?, last_error = ?, next_retry_at = ? WHERE uuid = ? AND action IN ('create', 'rename') AND attempts < 10",
			tbl,
		))
		if res, err := db.SQL().ExecContext(ctx, updQ, name, errText, nextRetry, uuid); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				return nil
			}
		}
	}

	q := db.Rebind(fmt.Sprintf(
		"INSERT INTO %s (action, uuid, name, attempts, next_retry_at, last_error, created_at) VALUES (?, ?, ?, 1, ?, ?, ?)",
		tbl,
	))
	_, err := db.SQL().ExecContext(ctx, q, action, uuid, name, nextRetry, errText, now)
	return err
}

// claimOutboxItem atomically advances next_retry_at to leaseUntil so concurrent
// cron/admin workers and HA pods do not deliver the same outbox row twice.
func claimOutboxItem(ctx context.Context, db *plugindb.DB, id, now, leaseUntil int64) (bool, error) {
	if db == nil {
		return false, nil
	}
	tbl := db.Table("sync_outbox")
	q := db.Rebind(fmt.Sprintf(
		"UPDATE %s SET next_retry_at = ? WHERE id = ? AND next_retry_at <= ? AND attempts < 10",
		tbl,
	))
	res, err := db.SQL().ExecContext(ctx, q, leaseUntil, id, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func pruneStaleOutbox(ctx context.Context, db *plugindb.DB, cutoffUnix int64) error {
	if db == nil {
		return nil
	}
	tbl := db.Table("sync_outbox")
	q := db.Rebind(fmt.Sprintf("DELETE FROM %s WHERE attempts >= 10 AND created_at <= ?", tbl))
	_, err := db.SQL().ExecContext(ctx, q, cutoffUnix)
	return err
}

func listDueOutbox(ctx context.Context, db *plugindb.DB, now int64, limit int) ([]outboxItem, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	tbl := db.Table("sync_outbox")
	q := db.Rebind(fmt.Sprintf(
		"SELECT id, action, uuid, name, attempts, next_retry_at, last_error, created_at FROM %s WHERE next_retry_at <= ? AND attempts < 10 ORDER BY id ASC LIMIT ?",
		tbl,
	))
	return queryOutboxRows(ctx, db, q, now, limit)
}

func listAllOutbox(ctx context.Context, db *plugindb.DB, limit int) ([]outboxItem, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	tbl := db.Table("sync_outbox")
	q := db.Rebind(fmt.Sprintf(
		"SELECT id, action, uuid, name, attempts, next_retry_at, last_error, created_at FROM %s ORDER BY id DESC LIMIT ?",
		tbl,
	))
	return queryOutboxRows(ctx, db, q, limit)
}

func queryOutboxRows(ctx context.Context, db *plugindb.DB, q string, args ...any) ([]outboxItem, error) {
	rows, err := db.SQL().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []outboxItem
	for rows.Next() {
		var it outboxItem
		if err := rows.Scan(&it.ID, &it.Action, &it.UUID, &it.Name, &it.Attempts, &it.NextRetryAt, &it.LastError, &it.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func markOutboxAttemptFailed(ctx context.Context, db *plugindb.DB, id int64, nextAttempts int, nextRetryAt int64, lastErr string) error {
	if db == nil {
		return nil
	}
	tbl := db.Table("sync_outbox")
	q := db.Rebind(fmt.Sprintf(
		"UPDATE %s SET attempts = ?, next_retry_at = ?, last_error = ? WHERE id = ?",
		tbl,
	))
	_, err := db.SQL().ExecContext(ctx, q, nextAttempts, nextRetryAt, truncateStr(lastErr, 500), id)
	return err
}

func deleteOutbox(ctx context.Context, db *plugindb.DB, id int64) (bool, error) {
	if db == nil {
		return false, nil
	}
	tbl := db.Table("sync_outbox")
	q := db.Rebind(fmt.Sprintf("DELETE FROM %s WHERE id = ?", tbl))
	res, err := db.SQL().ExecContext(ctx, q, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func resetAllOutboxDue(ctx context.Context, db *plugindb.DB, now int64) error {
	if db == nil {
		return nil
	}
	tbl := db.Table("sync_outbox")
	q := db.Rebind(fmt.Sprintf("UPDATE %s SET attempts = 0, next_retry_at = ?", tbl))
	_, err := db.SQL().ExecContext(ctx, q, now)
	return err
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
