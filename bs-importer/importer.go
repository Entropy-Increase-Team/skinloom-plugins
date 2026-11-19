package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	plugin "github.com/Entropy-Increase-Team/skinloom/sdk/go"
)

const (
	TableInvitationCodes    = "invitation_codes"
	TableMojangVerification = "mojang_verifications"
	TableTextureDescription = "textures_description"

	maxBridgeRespBytes = 16 << 20 // 16 MiB per page
	serviceBatchSize   = 200
)

var (
	validPrefixRe = regexp.MustCompile(`^[A-Za-z0-9_]{0,32}$`)
	allExtTables  = []string{TableInvitationCodes, TableMojangVerification, TableTextureDescription}
)

// SourceConfig describes how bs-importer connects to a Blessing Skin database or export bridge.
type SourceConfig struct {
	Mode        string   `json:"mode"`                  // "db" | "bridge"
	Driver      string   `json:"driver,omitempty"`      // "mysql" | "postgres" | "sqlite"
	DSN         string   `json:"dsn,omitempty"`         // database DSN (never persisted to plugin DB)
	TablePrefix string   `json:"tablePrefix,omitempty"` // optional BS table prefix
	BridgeURL   string   `json:"bridgeUrl,omitempty"`   // blessing-skin-export-bridge base URL
	BridgeToken string   `json:"bridgeToken,omitempty"` // bearer token (never persisted)
	Tables      []string `json:"tables,omitempty"`
	DryRun      bool     `json:"dryRun,omitempty"`
}

// TableProbe describes one detected BS extension table.
type TableProbe struct {
	Table        string `json:"table"`
	TargetPlugin string `json:"targetPlugin"`
	Exists       bool   `json:"exists"`
	Rows         int64  `json:"rows"`
}

// TestSourceResult summarizes source connectivity and detected BS plugin tables.
type TestSourceResult struct {
	OK        bool         `json:"ok"`
	Mode      string       `json:"mode"`
	BSVersion string       `json:"bsVersion,omitempty"`
	Tables    []TableProbe `json:"tables"`
}

// TableImportReport summarizes migration outcomes for a single BS extension table.
type TableImportReport struct {
	Table        string   `json:"table"`
	TargetPlugin string   `json:"targetPlugin"`
	Read         int      `json:"read"`
	Imported     int      `json:"imported"`
	Skipped      int      `json:"skipped"`
	Errors       []string `json:"errors,omitempty"`
}

// ImportReport is persisted in {{prefix}}jobs.report_json.
type ImportReport struct {
	DryRun        bool                `json:"dryRun"`
	TotalRead     int                 `json:"totalRead"`
	TotalImported int                 `json:"totalImported"`
	TotalSkipped  int                 `json:"totalSkipped"`
	TotalErrors   int                 `json:"totalErrors"`
	Tables        []TableImportReport `json:"tables"`
}

type InvitationCodeRow struct {
	Code        string `json:"code"`
	GeneratedAt string `json:"generatedAt,omitempty"`
	UsedBy      int64  `json:"usedBy,omitempty"`
	UsedAt      string `json:"usedAt,omitempty"`
}

type MojangVerificationRow struct {
	UserID    int64  `json:"userId"`
	UUID      string `json:"uuid"`
	Verified  bool   `json:"verified"`
	CreatedAt string `json:"createdAt,omitempty"`
}

type TextureDescriptionRow struct {
	TextureID   int64  `json:"textureId"`
	Description string `json:"description"`
}

type serviceImportResult struct {
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors"`
}

type extSource interface {
	Close() error
	Probe(ctx context.Context) (*TestSourceResult, error)
	ReadInvitationCodes(ctx context.Context) ([]InvitationCodeRow, error)
	ReadMojangVerifications(ctx context.Context) ([]MojangVerificationRow, error)
	ReadTextureDescriptions(ctx context.Context) ([]TextureDescriptionRow, error)
}

func targetPluginForTable(table string) string {
	switch table {
	case TableInvitationCodes:
		return "invitation-codes"
	case TableMojangVerification:
		return "mojang-verification"
	case TableTextureDescription:
		return "texture-description"
	default:
		return ""
	}
}

func normalizeRequestedTables(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return append([]string(nil), allExtTables...), nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.TrimSpace(t)
		if targetPluginForTable(t) == "" {
			return nil, fmt.Errorf("unsupported extension table %q", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

func openSource(ctx context.Context, cfg SourceConfig) (extSource, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Mode)) {
	case "db":
		return openDBSource(ctx, cfg)
	case "bridge":
		return openBridgeSource(cfg)
	default:
		return nil, fmt.Errorf("unsupported source mode %q (expected \"db\" or \"bridge\")", cfg.Mode)
	}
}

// --- Direct Database Source (MySQL / PostgreSQL / SQLite) ---

type dbSource struct {
	db      *sql.DB
	dialect string
	prefix  string
}

func openDBSource(ctx context.Context, cfg SourceConfig) (*dbSource, error) {
	prefix := strings.TrimSpace(cfg.TablePrefix)
	if !validPrefixRe.MatchString(prefix) {
		return nil, fmt.Errorf("invalid tablePrefix %q (only letters, digits, underscores allowed)", prefix)
	}
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, errors.New("database dsn is required")
	}
	var dialect, sqlDriver string
	switch strings.ToLower(strings.TrimSpace(cfg.Driver)) {
	case "sqlite", "sqlite3":
		dialect, sqlDriver = "sqlite", "sqlite"
	case "mysql", "mariadb":
		dialect, sqlDriver = "mysql", "mysql"
	case "postgres", "postgresql", "pg":
		dialect, sqlDriver = "postgres", "pgx"
	default:
		return nil, fmt.Errorf("unsupported driver %q (expected mysql, postgres, or sqlite)", cfg.Driver)
	}
	pool, err := sql.Open(sqlDriver, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open source %s: %w", dialect, err)
	}
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(1)
	pingCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("connect source %s: %w", dialect, err)
	}
	return &dbSource{db: pool, dialect: dialect, prefix: prefix}, nil
}

func (s *dbSource) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *dbSource) tableName(base string) string {
	return s.prefix + base
}

func (s *dbSource) quoteIdent(name string) string {
	if s.dialect == "mysql" {
		return "`" + name + "`"
	}
	return `"` + name + `"`
}

func (s *dbSource) tableExists(ctx context.Context, fullName string) (bool, error) {
	var count int
	var err error
	switch s.dialect {
	case "sqlite":
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, fullName).Scan(&count)
	case "mysql":
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`, fullName).Scan(&count)
	case "postgres":
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1`, fullName).Scan(&count)
	}
	return count > 0, err
}

func (s *dbSource) Probe(ctx context.Context) (*TestSourceResult, error) {
	probes := make([]TableProbe, 0, len(allExtTables))
	for _, base := range allExtTables {
		full := s.tableName(base)
		exists, err := s.tableExists(ctx, full)
		if err != nil {
			return nil, fmt.Errorf("probe table %s: %w", full, err)
		}
		var rows int64
		if exists {
			if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+s.quoteIdent(full)).Scan(&rows); err != nil {
				return nil, fmt.Errorf("count table %s: %w", full, err)
			}
		}
		probes = append(probes, TableProbe{
			Table:        base,
			TargetPlugin: targetPluginForTable(base),
			Exists:       exists,
			Rows:         rows,
		})
	}
	return &TestSourceResult{OK: true, Mode: "db", Tables: probes}, nil
}

func (s *dbSource) ReadInvitationCodes(ctx context.Context) ([]InvitationCodeRow, error) {
	full := s.tableName(TableInvitationCodes)
	exists, err := s.tableExists(ctx, full)
	if err != nil || !exists {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT code, generated_at, used_by, used_at FROM "+s.quoteIdent(full)+" ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []InvitationCodeRow
	for rows.Next() {
		var code string
		var genAt, usedAt sql.NullString
		var usedBy sql.NullInt64
		if err := rows.Scan(&code, &genAt, &usedBy, &usedAt); err != nil {
			return nil, err
		}
		out = append(out, InvitationCodeRow{
			Code:        strings.TrimSpace(code),
			GeneratedAt: normalizeTimestampString(genAt.String),
			UsedBy:      usedBy.Int64,
			UsedAt:      normalizeTimestampString(usedAt.String),
		})
	}
	return out, rows.Err()
}

func (s *dbSource) ReadMojangVerifications(ctx context.Context) ([]MojangVerificationRow, error) {
	full := s.tableName(TableMojangVerification)
	exists, err := s.tableExists(ctx, full)
	if err != nil || !exists {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT user_id, uuid, verified, created_at FROM "+s.quoteIdent(full)+" ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MojangVerificationRow
	for rows.Next() {
		var uid int64
		var uuid string
		var verifiedRaw any
		var createdAt sql.NullString
		if err := rows.Scan(&uid, &uuid, &verifiedRaw, &createdAt); err != nil {
			return nil, err
		}
		out = append(out, MojangVerificationRow{
			UserID:    uid,
			UUID:      strings.TrimSpace(uuid),
			Verified:  parseSQLBool(verifiedRaw),
			CreatedAt: normalizeTimestampString(createdAt.String),
		})
	}
	return out, rows.Err()
}

func (s *dbSource) ReadTextureDescriptions(ctx context.Context) ([]TextureDescriptionRow, error) {
	full := s.tableName(TableTextureDescription)
	exists, err := s.tableExists(ctx, full)
	if err != nil || !exists {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT tid, description FROM "+s.quoteIdent(full)+" ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TextureDescriptionRow
	for rows.Next() {
		var tid int64
		var desc sql.NullString
		if err := rows.Scan(&tid, &desc); err != nil {
			return nil, err
		}
		out = append(out, TextureDescriptionRow{
			TextureID:   tid,
			Description: strings.TrimSpace(desc.String),
		})
	}
	return out, rows.Err()
}

func parseSQLBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case []byte:
		s := strings.TrimSpace(strings.ToLower(string(x)))
		return s == "1" || s == "true" || s == "t"
	case string:
		s := strings.TrimSpace(strings.ToLower(x))
		return s == "1" || s == "true" || s == "t"
	default:
		return true
	}
}

func normalizeTimestampString(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05-07:00",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

// --- Export Bridge HTTP Source (blessing-skin-export-bridge) ---

type bridgeSource struct {
	baseURL string
	token   string
	client  *http.Client
}

func isPrivateOrLoopbackHost(host string) bool {
	h := strings.Trim(strings.ToLower(host), "[]")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

func openBridgeSource(cfg SourceConfig) (*bridgeSource, error) {
	rawURL := strings.TrimRight(strings.TrimSpace(cfg.BridgeURL), "/")
	if rawURL == "" {
		return nil, errors.New("bridgeUrl is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid bridgeUrl %q", rawURL)
	}
	if parsed.Scheme == "http" && !isPrivateOrLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("HTTPS is required for non-local bridge endpoints")
	}
	if strings.TrimSpace(cfg.BridgeToken) == "" {
		return nil, errors.New("bridgeToken is required")
	}
	return &bridgeSource{
		baseURL: rawURL,
		token:   strings.TrimSpace(cfg.BridgeToken),
		client: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return errors.New("too many redirects")
				}
				if req.URL.Scheme == "http" && !isPrivateOrLoopbackHost(req.URL.Hostname()) {
					return errors.New("refusing redirect to insecure HTTP endpoint")
				}
				return nil
			},
		},
	}, nil
}

func (b *bridgeSource) Close() error { return nil }

func (b *bridgeSource) getJSON(ctx context.Context, relPath string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+relPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBridgeRespBytes+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > maxBridgeRespBytes {
		return errors.New("bridge response exceeded size cap")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, dst)
}

func (b *bridgeSource) Probe(ctx context.Context) (*TestSourceResult, error) {
	var manifest struct {
		BSVersion  string `json:"bsVersion"`
		Extensions map[string]struct {
			Table string `json:"table"`
			Rows  int64  `json:"rows"`
		} `json:"extensions"`
	}
	if err := b.getJSON(ctx, "/manifest", &manifest); err != nil {
		return nil, err
	}
	byTable := make(map[string]int64, len(manifest.Extensions))
	for _, ext := range manifest.Extensions {
		byTable[ext.Table] = ext.Rows
	}
	probes := make([]TableProbe, 0, len(allExtTables))
	for _, base := range allExtTables {
		rows, exists := byTable[base]
		probes = append(probes, TableProbe{
			Table:        base,
			TargetPlugin: targetPluginForTable(base),
			Exists:       exists,
			Rows:         rows,
		})
	}
	return &TestSourceResult{
		OK:        true,
		Mode:      "bridge",
		BSVersion: manifest.BSVersion,
		Tables:    probes,
	}, nil
}

func (b *bridgeSource) fetchPaginated(ctx context.Context, table string, onBatch func(raw json.RawMessage) error) error {
	cursor := ""
	for page := 0; page < 1000; page++ {
		q := url.Values{}
		q.Set("table", table)
		q.Set("limit", "500")
		if cursor != "" {
			q.Set("after", cursor)
		}
		var resp struct {
			Rows       json.RawMessage `json:"rows"`
			NextCursor string          `json:"nextCursor"`
			HasMore    bool            `json:"hasMore"`
		}
		if err := b.getJSON(ctx, "/rows?"+q.Encode(), &resp); err != nil {
			return err
		}
		if len(resp.Rows) > 0 {
			if err := onBatch(resp.Rows); err != nil {
				return err
			}
		}
		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	return nil
}

func (b *bridgeSource) ReadInvitationCodes(ctx context.Context) ([]InvitationCodeRow, error) {
	var out []InvitationCodeRow
	err := b.fetchPaginated(ctx, TableInvitationCodes, func(raw json.RawMessage) error {
		var batch []InvitationCodeRow
		if err := json.Unmarshal(raw, &batch); err != nil {
			return err
		}
		out = append(out, batch...)
		return nil
	})
	return out, err
}

func (b *bridgeSource) ReadMojangVerifications(ctx context.Context) ([]MojangVerificationRow, error) {
	var out []MojangVerificationRow
	err := b.fetchPaginated(ctx, TableMojangVerification, func(raw json.RawMessage) error {
		var batch []MojangVerificationRow
		if err := json.Unmarshal(raw, &batch); err != nil {
			return err
		}
		out = append(out, batch...)
		return nil
	})
	return out, err
}

func (b *bridgeSource) ReadTextureDescriptions(ctx context.Context) ([]TextureDescriptionRow, error) {
	var out []TextureDescriptionRow
	err := b.fetchPaginated(ctx, TableTextureDescription, func(raw json.RawMessage) error {
		var batch []TextureDescriptionRow
		if err := json.Unmarshal(raw, &batch); err != nil {
			return err
		}
		out = append(out, batch...)
		return nil
	})
	return out, err
}

// --- Batch Service Dispatcher ---

func dispatchBatches[T any](
	ctx context.Context,
	pctx *plugin.Context,
	targetPlugin string,
	serviceName string,
	items []T,
	dryRun bool,
	emit func(msg string),
) TableImportReport {
	rep := TableImportReport{
		TargetPlugin: targetPlugin,
		Read:         len(items),
	}
	if len(items) == 0 {
		return rep
	}
	if dryRun {
		rep.Skipped = len(items)
		emit(fmt.Sprintf("[dry-run] %s: read %d row(s) ready for %s.%s", targetPlugin, len(items), targetPlugin, serviceName))
		return rep
	}
	for start := 0; start < len(items); start += serviceBatchSize {
		end := min(start+serviceBatchSize, len(items))
		batch := items[start:end]
		raw, err := pctx.Services.Call(ctx, targetPlugin, serviceName, batch)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s.%s (batch %d-%d): %v", targetPlugin, serviceName, start, end, err))
			emit(fmt.Sprintf("[error] %s.%s failed: %v", targetPlugin, serviceName, err))
			return rep
		}
		var res serviceImportResult
		if err := json.Unmarshal(raw, &res); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("decode %s.%s response: %v", targetPlugin, serviceName, err))
			return rep
		}
		rep.Imported += res.Imported
		rep.Skipped += res.Skipped
		rep.Errors = append(rep.Errors, res.Errors...)
		emit(fmt.Sprintf("[progress] %s: %d/%d processed (imported=%d, skipped=%d)", targetPlugin, end, len(items), rep.Imported, rep.Skipped))
	}
	return rep
}
