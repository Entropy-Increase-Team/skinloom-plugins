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

	ServiceBSMigrateDescribe = "bsMigrateDescribe"
	ServiceBSMigrateImport   = "bsMigrateImport"
	dynamicProviderPrefix    = "plugin:"

	maxBridgeRespBytes  = 16 << 20 // 16 MiB per page
	serviceBatchSize    = 200
	maxGenericTableRows = 50000
	maxProviderKeys     = 100
	maxProviderTables   = 10
)

var (
	validPrefixRe     = regexp.MustCompile(`^[A-Za-z0-9_]{0,32}$`)
	safeOptionKeyRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	safeSQLIdentRe    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	legacyBuiltInList = []string{TableInvitationCodes, TableMojangVerification, TableTextureDescription}
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

// MigrationProviderSpec describes a dynamically discovered SkinLoom plugin that
// implements the generic bsMigrateDescribe / bsMigrateImport migration protocol.
type MigrationProviderSpec struct {
	ID             string            `json:"id"`
	TargetPlugin   string            `json:"targetPlugin"`
	Label          map[string]string `json:"label"`
	Description    map[string]string `json:"description,omitempty"`
	OptionKeys     []string          `json:"optionKeys,omitempty"`
	OptionPrefixes []string          `json:"optionPrefixes,omitempty"`
	Tables         []string          `json:"tables,omitempty"`
}

// ProviderSummary is returned by GET /admin/providers for the admin UI checklist.
type ProviderSummary struct {
	ID             string            `json:"id"`
	TargetPlugin   string            `json:"targetPlugin"`
	Dynamic        bool              `json:"dynamic"`
	Label          map[string]string `json:"label,omitempty"`
	Description    map[string]string `json:"description,omitempty"`
	OptionKeys     []string          `json:"optionKeys,omitempty"`
	OptionPrefixes []string          `json:"optionPrefixes,omitempty"`
	Tables         []string          `json:"tables,omitempty"`
}

// GenericMigrationPayload is passed to a target plugin's bsMigrateImport service.
// It contains ONLY the options and tables declared by that plugin in bsMigrateDescribe.
type GenericMigrationPayload struct {
	Options map[string]string           `json:"options,omitempty"`
	Tables  map[string][]map[string]any `json:"tables,omitempty"`
	DryRun  bool                        `json:"dryRun"`
}

// TableProbe describes one detected BS extension table or plugin migration item.
type TableProbe struct {
	Table          string            `json:"table"`
	TargetPlugin   string            `json:"targetPlugin"`
	Dynamic        bool              `json:"dynamic,omitempty"`
	Label          map[string]string `json:"label,omitempty"`
	Description    map[string]string `json:"description,omitempty"`
	OptionKeys     []string          `json:"optionKeys,omitempty"`
	OptionPrefixes []string          `json:"optionPrefixes,omitempty"`
	Tables         []string          `json:"tables,omitempty"`
	Exists         bool              `json:"exists"`
	Rows           int64             `json:"rows"`
}

// TestSourceResult summarizes source connectivity and detected BS extension items.
type TestSourceResult struct {
	OK        bool         `json:"ok"`
	Mode      string       `json:"mode"`
	BSVersion string       `json:"bsVersion,omitempty"`
	Tables    []TableProbe `json:"tables"`
}

// TableImportReport summarizes migration outcomes for a single BS extension table or plugin provider.
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
	Probe(ctx context.Context, providers []MigrationProviderSpec) (*TestSourceResult, error)
	ReadInvitationCodes(ctx context.Context) ([]InvitationCodeRow, error)
	ReadMojangVerifications(ctx context.Context) ([]MojangVerificationRow, error)
	ReadTextureDescriptions(ctx context.Context) ([]TextureDescriptionRow, error)
	ReadOptions(ctx context.Context, keys []string, prefixes []string) (map[string]string, error)
	ReadGenericTable(ctx context.Context, table string) ([]map[string]any, error)
}

func targetPluginForLegacyTable(table string) string {
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

var deniedGenericTables = map[string]bool{
	"users":                  true,
	"players":                true,
	"textures":               true,
	"user_closet":            true,
	"options":                true,
	"uuid":                   true,
	"reports":                true,
	"notifications":          true,
	"language_lines":         true,
	"migrations":             true,
	"password_resets":        true,
	"personal_access_tokens": true,
	"sessions":               true,
	"jobs":                   true,
	"failed_jobs":            true,
	"scopes":                 true,
	"_prisma_migrations":     true,
	TableInvitationCodes:     true,
	TableMojangVerification:  true,
	TableTextureDescription:  true,
}

func isDeniedGenericTable(table string) bool {
	low := strings.ToLower(strings.TrimSpace(table))
	if low == "" || deniedGenericTables[low] {
		return true
	}
	return strings.HasPrefix(low, "oauth_") || strings.HasPrefix(low, "ygg")
}

func isDeniedOptionKey(key string) bool {
	low := strings.ToLower(strings.TrimSpace(key))
	if low == "" {
		return true
	}
	if low == "ygg_private_key" || low == "app_key" {
		return true
	}
	for _, p := range []string{"skinloom_bridge_", "mail_", "smtp_", "oauth_", "jwt_"} {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

func isAllowedOptionPrefix(prefix string) bool {
	if len(prefix) < 4 {
		return false
	}
	last := prefix[len(prefix)-1]
	if last != '_' && last != '.' && last != '-' {
		return false
	}
	low := strings.ToLower(prefix)
	for _, denied := range []string{"skinloom_bridge_", "mail_", "smtp_", "ygg_", "oauth_", "jwt_"} {
		if strings.HasPrefix(low, denied) || strings.HasPrefix(denied, low) {
			return false
		}
	}
	return true
}

// discoverMigrationProviders queries the host service bus for all active plugins
// that opted into bs-importer and expose the generic bsMigrateDescribe service.
func discoverMigrationProviders(ctx context.Context, pctx *plugin.Context) []MigrationProviderSpec {
	if pctx == nil || pctx.Services == nil {
		return nil
	}
	ids, err := pctx.Services.Discover(ctx, ServiceBSMigrateDescribe)
	if err != nil || len(ids) == 0 {
		return nil
	}
	out := make([]MigrationProviderSpec, 0, len(ids))
	for _, pid := range ids {
		descCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		raw, callErr := pctx.Services.Call(descCtx, pid, ServiceBSMigrateDescribe)
		cancel()
		if callErr != nil || len(raw) == 0 {
			continue
		}
		var desc struct {
			Label          map[string]string `json:"label"`
			Description    map[string]string `json:"description"`
			OptionKeys     []string          `json:"optionKeys"`
			OptionPrefixes []string          `json:"optionPrefixes"`
			Tables         []string          `json:"tables"`
		}
		if err := json.Unmarshal(raw, &desc); err != nil {
			continue
		}
		rawKeys := sanitizeStringSlice(desc.OptionKeys, safeOptionKeyRe, maxProviderKeys)
		cleanKeys := make([]string, 0, len(rawKeys))
		for _, k := range rawKeys {
			if !isDeniedOptionKey(k) {
				cleanKeys = append(cleanKeys, k)
			}
		}
		rawPrefixes := sanitizeStringSlice(desc.OptionPrefixes, safeOptionKeyRe, maxProviderKeys)
		cleanPrefixes := make([]string, 0, len(rawPrefixes))
		for _, p := range rawPrefixes {
			if isAllowedOptionPrefix(p) {
				cleanPrefixes = append(cleanPrefixes, p)
			}
		}
		rawTables := sanitizeStringSlice(desc.Tables, safeSQLIdentRe, maxProviderTables)
		cleanTables := make([]string, 0, len(rawTables))
		for _, tbl := range rawTables {
			if !isDeniedGenericTable(tbl) {
				cleanTables = append(cleanTables, tbl)
			}
		}
		if len(cleanKeys) == 0 && len(cleanPrefixes) == 0 && len(cleanTables) == 0 {
			continue
		}
		label := desc.Label
		if len(label) == 0 {
			label = map[string]string{"en": pid, "zh-CN": pid}
		}
		out = append(out, MigrationProviderSpec{
			ID:             dynamicProviderPrefix + pid,
			TargetPlugin:   pid,
			Label:          label,
			Description:    desc.Description,
			OptionKeys:     cleanKeys,
			OptionPrefixes: cleanPrefixes,
			Tables:         cleanTables,
		})
	}
	return out
}

func sanitizeStringSlice(in []string, re *regexp.Regexp, maxItems int) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, min(len(in), maxItems))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" || seen[s] || !re.MatchString(s) {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= maxItems {
			break
		}
	}
	return out
}

func normalizeRequestedTargets(raw []string, providers []MigrationProviderSpec) ([]string, map[string]MigrationProviderSpec, error) {
	provByID := make(map[string]MigrationProviderSpec, len(providers))
	for _, p := range providers {
		provByID[p.ID] = p
	}
	if len(raw) == 0 {
		all := append([]string(nil), legacyBuiltInList...)
		for _, p := range providers {
			all = append(all, p.ID)
		}
		return all, provByID, nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.TrimSpace(t)
		_, isDyn := provByID[t]
		if targetPluginForLegacyTable(t) == "" && !isDyn {
			return nil, nil, fmt.Errorf("unsupported extension migration target %q", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, provByID, nil
}

func countMeaningfulOptions(opts map[string]string) int {
	n := 0
	for _, v := range opts {
		s := strings.TrimSpace(v)
		if s != "" && s != "{}" && s != "[]" && s != "null" {
			n++
		}
	}
	return n
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
		return nil, fmt.Errorf("open source %s: %s", dialect, scrubDSN(err.Error(), cfg.DSN))
	}
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(1)
	pingCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("connect source %s: %s", dialect, scrubDSN(err.Error(), cfg.DSN))
	}
	return &dbSource{db: pool, dialect: dialect, prefix: prefix}, nil
}

func scrubDSN(msg, dsn string) string {
	if dsn != "" && strings.Contains(msg, dsn) {
		msg = strings.ReplaceAll(msg, dsn, "[REDACTED_DSN]")
	}
	return msg
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

func (s *dbSource) placeholder(idx int) string {
	if s.dialect == "postgres" {
		return fmt.Sprintf("$%d", idx)
	}
	return "?"
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

func (s *dbSource) Probe(ctx context.Context, providers []MigrationProviderSpec) (*TestSourceResult, error) {
	probes := make([]TableProbe, 0, len(legacyBuiltInList)+len(providers))
	for _, base := range legacyBuiltInList {
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
			TargetPlugin: targetPluginForLegacyTable(base),
			Exists:       exists,
			Rows:         rows,
		})
	}
	for _, prov := range providers {
		var count int64
		if len(prov.OptionKeys) > 0 || len(prov.OptionPrefixes) > 0 {
			opts, err := s.ReadOptions(ctx, prov.OptionKeys, prov.OptionPrefixes)
			if err != nil {
				return nil, fmt.Errorf("probe options for %s: %w", prov.TargetPlugin, err)
			}
			count += int64(countMeaningfulOptions(opts))
		}
		for _, tbl := range prov.Tables {
			full := s.tableName(tbl)
			exists, err := s.tableExists(ctx, full)
			if err != nil {
				return nil, fmt.Errorf("probe table %s for %s: %w", full, prov.TargetPlugin, err)
			}
			if exists {
				var rows int64
				if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+s.quoteIdent(full)).Scan(&rows); err != nil {
					return nil, fmt.Errorf("count table %s: %w", full, err)
				}
				count += rows
			}
		}
		probes = append(probes, TableProbe{
			Table:          prov.ID,
			TargetPlugin:   prov.TargetPlugin,
			Dynamic:        true,
			Label:          prov.Label,
			Description:    prov.Description,
			OptionKeys:     prov.OptionKeys,
			OptionPrefixes: prov.OptionPrefixes,
			Tables:         prov.Tables,
			Exists:         count > 0,
			Rows:           count,
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

func escapeSQLLike(prefix string) string {
	r := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return r.Replace(prefix) + "%"
}

func (s *dbSource) ReadOptions(ctx context.Context, keys []string, prefixes []string) (map[string]string, error) {
	out := map[string]string{}
	if len(keys) == 0 && len(prefixes) == 0 {
		return out, nil
	}
	full := s.tableName("options")
	exists, err := s.tableExists(ctx, full)
	if err != nil || !exists {
		return out, err
	}
	var clauses []string
	var args []any
	argIdx := 1
	if len(keys) > 0 {
		phs := make([]string, len(keys))
		for i, k := range keys {
			phs[i] = s.placeholder(argIdx)
			argIdx++
			args = append(args, k)
		}
		clauses = append(clauses, "option_name IN ("+strings.Join(phs, ",")+")")
	}
	for _, p := range prefixes {
		clauses = append(clauses, fmt.Sprintf("option_name LIKE %s ESCAPE '!'", s.placeholder(argIdx)))
		argIdx++
		args = append(args, escapeSQLLike(p))
	}
	q := fmt.Sprintf("SELECT option_name, option_value FROM %s WHERE %s", s.quoteIdent(full), strings.Join(clauses, " OR "))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var val sql.NullString
		if err := rows.Scan(&name, &val); err != nil {
			return nil, err
		}
		cleanName := strings.TrimSpace(name)
		if isDeniedOptionKey(cleanName) {
			continue
		}
		out[cleanName] = val.String
	}
	return out, rows.Err()
}

func (s *dbSource) ReadGenericTable(ctx context.Context, table string) ([]map[string]any, error) {
	if !safeSQLIdentRe.MatchString(table) || isDeniedGenericTable(table) {
		return nil, fmt.Errorf("table %q is not permitted for generic plugin migration", table)
	}
	full := s.tableName(table)
	exists, err := s.tableExists(ctx, full)
	if err != nil || !exists {
		return nil, err
	}
	q := fmt.Sprintf("SELECT * FROM %s LIMIT %d", s.quoteIdent(full), maxGenericTableRows+1)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		if len(out) >= maxGenericTableRows {
			return nil, fmt.Errorf("table %q exceeds maximum supported row cap (%d)", table, maxGenericTableRows)
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rowMap := make(map[string]any, len(cols))
		for i, col := range cols {
			if b, ok := vals[i].([]byte); ok {
				rowMap[col] = string(b)
			} else {
				rowMap[col] = vals[i]
			}
		}
		out = append(out, rowMap)
	}
	return out, rows.Err()
}

func parseSQLBool(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int:
		return x != 0
	case int8:
		return x != 0
	case int16:
		return x != 0
	case int32:
		return x != 0
	case int64:
		return x != 0
	case uint:
		return x != 0
	case uint8:
		return x != 0
	case uint16:
		return x != 0
	case uint32:
		return x != 0
	case uint64:
		return x != 0
	case float32:
		return x != 0
	case float64:
		return x != 0
	case []byte:
		s := strings.TrimSpace(strings.ToLower(string(x)))
		return s == "1" || s == "true" || s == "t"
	case string:
		s := strings.TrimSpace(strings.ToLower(x))
		return s == "1" || s == "true" || s == "t"
	default:
		return false
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

type bridgeManifestResp struct {
	BSVersion  string            `json:"bsVersion"`
	Options    map[string]string `json:"options"`
	Extensions map[string]struct {
		Table string `json:"table"`
		Rows  int64  `json:"rows"`
	} `json:"extensions"`
}

type bridgeSource struct {
	baseURL        string
	token          string
	client         *http.Client
	cachedManifest *bridgeManifestResp
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

func parseBridgeCredentials(raw string) (bridgeURL, token string, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		var bundle struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(trimmed), &bundle); err == nil && strings.TrimSpace(bundle.URL) != "" {
			return strings.TrimSpace(bundle.URL), strings.TrimSpace(bundle.Token), true
		}
	}
	return "", "", false
}

func openBridgeSource(cfg SourceConfig) (*bridgeSource, error) {
	rawURL := strings.TrimSpace(cfg.BridgeURL)
	rawToken := strings.TrimSpace(cfg.BridgeToken)
	if parsedURL, parsedTok, ok := parseBridgeCredentials(rawURL); ok {
		rawURL = parsedURL
		if rawToken == "" {
			rawToken = parsedTok
		}
	}
	if rawURL == "" {
		return nil, errors.New("bridgeUrl is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("invalid bridgeUrl: expected an https:// URL or a valid JSON credential bundle")
	}
	if parsed.User != nil {
		return nil, errors.New("bridgeUrl must not contain embedded credentials")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	isLocalTarget := isPrivateOrLoopbackHost(parsed.Hostname())
	if parsed.Scheme == "http" && !isLocalTarget {
		return nil, errors.New("HTTPS is required for non-local bridge endpoints")
	}
	if rawToken == "" {
		return nil, errors.New("bridgeToken is required")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	for _, suffix := range []string{
		"/plugin/skinloom-bridge/manifest",
		"/plugin/skinloom-bridge",
		"/skinloom-bridge/manifest",
		"/skinloom-bridge",
	} {
		if strings.HasSuffix(parsed.Path, suffix) {
			parsed.Path = strings.TrimRight(strings.TrimSuffix(parsed.Path, suffix), "/")
			break
		}
	}
	parsed.Path += "/plugin/skinloom-bridge"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        8,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 8 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IP addresses resolved for %q", host)
			}
			for _, rec := range ips {
				ip := rec.IP
				if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
					return nil, fmt.Errorf("blocked link-local/unspecified bridge IP %s", ip.String())
				}
				if !isLocalTarget && (ip.IsLoopback() || ip.IsPrivate()) {
					return nil, fmt.Errorf("blocked private/loopback IP %s for public bridge host %q", ip.String(), host)
				}
			}
			var lastErr error
			for _, rec := range ips {
				conn, dErr := dialer.DialContext(ctx, network, net.JoinHostPort(rec.IP.String(), port))
				if dErr == nil {
					return conn, nil
				}
				lastErr = dErr
			}
			return nil, lastErr
		},
	}
	return &bridgeSource{
		baseURL: parsed.String(),
		token:   rawToken,
		client: &http.Client{
			Timeout:   20 * time.Second,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (b *bridgeSource) Close() error { return nil }

func truncateErrorBody(body []byte, maxLen int) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > maxLen {
		return s[:maxLen] + "...(truncated)"
	}
	return s
}

func (b *bridgeSource) getJSON(ctx context.Context, relPath string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+relPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "SkinLoom-Importer/1")
	req.Header.Set("X-SkinLoom-Bridge-Client", "1")
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
		return fmt.Errorf("bridge HTTP %d: %s", resp.StatusCode, truncateErrorBody(body, 512))
	}
	return json.Unmarshal(body, dst)
}

func (b *bridgeSource) fetchManifest(ctx context.Context) (*bridgeManifestResp, error) {
	if b.cachedManifest != nil {
		return b.cachedManifest, nil
	}
	var m bridgeManifestResp
	if err := b.getJSON(ctx, "/manifest", &m); err != nil {
		return nil, err
	}
	b.cachedManifest = &m
	return &m, nil
}

func filterOptionsMap(all map[string]string, keys []string, prefixes []string) map[string]string {
	out := map[string]string{}
	if len(all) == 0 || (len(keys) == 0 && len(prefixes) == 0) {
		return out
	}
	keySet := make(map[string]bool, len(keys))
	for _, k := range keys {
		keySet[k] = true
	}
	for name, val := range all {
		if isDeniedOptionKey(name) {
			continue
		}
		if keySet[name] {
			out[name] = val
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				out[name] = val
				break
			}
		}
	}
	return out
}

func (b *bridgeSource) Probe(ctx context.Context, providers []MigrationProviderSpec) (*TestSourceResult, error) {
	manifest, err := b.fetchManifest(ctx)
	if err != nil {
		return nil, err
	}
	byTable := make(map[string]int64, len(manifest.Extensions))
	for _, ext := range manifest.Extensions {
		byTable[ext.Table] = ext.Rows
	}
	probes := make([]TableProbe, 0, len(legacyBuiltInList)+len(providers))
	for _, base := range legacyBuiltInList {
		rows, exists := byTable[base]
		probes = append(probes, TableProbe{
			Table:        base,
			TargetPlugin: targetPluginForLegacyTable(base),
			Exists:       exists,
			Rows:         rows,
		})
	}
	for _, prov := range providers {
		matchedOpts := filterOptionsMap(manifest.Options, prov.OptionKeys, prov.OptionPrefixes)
		count := int64(countMeaningfulOptions(matchedOpts))
		for _, tbl := range prov.Tables {
			if rows, ok := byTable[tbl]; ok && rows > 0 {
				count += rows
			}
		}
		probes = append(probes, TableProbe{
			Table:          prov.ID,
			TargetPlugin:   prov.TargetPlugin,
			Dynamic:        true,
			Label:          prov.Label,
			Description:    prov.Description,
			OptionKeys:     prov.OptionKeys,
			OptionPrefixes: prov.OptionPrefixes,
			Tables:         prov.Tables,
			Exists:         count > 0,
			Rows:           count,
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
	const maxPages = 1000
	for page := 0; page < maxPages; page++ {
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
			return nil
		}
		if resp.NextCursor == cursor {
			return fmt.Errorf("bridge pagination stalled on table %q at cursor %q", table, cursor)
		}
		cursor = resp.NextCursor
	}
	return fmt.Errorf("bridge pagination exceeded %d pages on table %q", maxPages, table)
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

func (b *bridgeSource) ReadOptions(ctx context.Context, keys []string, prefixes []string) (map[string]string, error) {
	manifest, err := b.fetchManifest(ctx)
	if err != nil {
		return nil, err
	}
	return filterOptionsMap(manifest.Options, keys, prefixes), nil
}

func (b *bridgeSource) ReadGenericTable(ctx context.Context, table string) ([]map[string]any, error) {
	if !safeSQLIdentRe.MatchString(table) || isDeniedGenericTable(table) {
		return nil, fmt.Errorf("table %q is not permitted for generic plugin migration", table)
	}
	manifest, err := b.fetchManifest(ctx)
	if err != nil {
		return nil, err
	}
	supported := false
	for _, ext := range manifest.Extensions {
		if ext.Table == table {
			supported = true
			break
		}
	}
	if !supported {
		return nil, nil
	}
	var out []map[string]any
	err = b.fetchPaginated(ctx, table, func(raw json.RawMessage) error {
		var batch []map[string]any
		if err := json.Unmarshal(raw, &batch); err != nil {
			return err
		}
		if len(out)+len(batch) > maxGenericTableRows {
			return fmt.Errorf("table %q exceeds maximum supported row cap (%d)", table, maxGenericTableRows)
		}
		out = append(out, batch...)
		return nil
	})
	return out, err
}

// --- Batch & Generic Provider Dispatchers ---

type translateFunc func(key string, params map[string]any, zhFallback, enFallback string) string

func dispatchBatches[T any](
	ctx context.Context,
	pctx *plugin.Context,
	targetPlugin string,
	serviceName string,
	items []T,
	dryRun bool,
	emit func(msg string),
	tr translateFunc,
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
		emit(tr("logDryRunTable", map[string]any{
			"plugin":  targetPlugin,
			"count":   len(items),
			"service": serviceName,
		},
			fmt.Sprintf("[模拟演练] %s：已读取 %d 条记录，等待调用 %s.%s", targetPlugin, len(items), targetPlugin, serviceName),
			fmt.Sprintf("[dry-run] %s: read %d row(s) ready for %s.%s", targetPlugin, len(items), targetPlugin, serviceName),
		))
		return rep
	}
	for start := 0; start < len(items); start += serviceBatchSize {
		end := min(start+serviceBatchSize, len(items))
		batch := items[start:end]
		raw, err := pctx.Services.Call(ctx, targetPlugin, serviceName, batch)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s.%s (batch %d-%d): %v", targetPlugin, serviceName, start, end, err))
			emit(tr("logTableError", map[string]any{
				"plugin":  targetPlugin,
				"service": serviceName,
				"error":   err.Error(),
			},
				fmt.Sprintf("[错误] %s.%s 调用失败：%v", targetPlugin, serviceName, err),
				fmt.Sprintf("[error] %s.%s failed: %v", targetPlugin, serviceName, err),
			))
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
		emit(tr("logTableProgress", map[string]any{
			"plugin":   targetPlugin,
			"done":     end,
			"total":    len(items),
			"imported": rep.Imported,
			"skipped":  rep.Skipped,
		},
			fmt.Sprintf("[进度] %s：已处理 %d/%d（成功 %d，跳过 %d）", targetPlugin, end, len(items), rep.Imported, rep.Skipped),
			fmt.Sprintf("[progress] %s: %d/%d processed (imported=%d, skipped=%d)", targetPlugin, end, len(items), rep.Imported, rep.Skipped),
		))
	}
	return rep
}

func dispatchGenericProvider(
	ctx context.Context,
	pctx *plugin.Context,
	src extSource,
	spec MigrationProviderSpec,
	dryRun bool,
	emit func(msg string),
	tr translateFunc,
) TableImportReport {
	rep := TableImportReport{
		Table:        spec.ID,
		TargetPlugin: spec.TargetPlugin,
	}
	payload := GenericMigrationPayload{DryRun: dryRun}
	readCount := 0

	if len(spec.OptionKeys) > 0 || len(spec.OptionPrefixes) > 0 {
		opts, err := src.ReadOptions(ctx, spec.OptionKeys, spec.OptionPrefixes)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("read options for %s: %v", spec.TargetPlugin, err))
			return rep
		}
		payload.Options = opts
		readCount += countMeaningfulOptions(opts)
	}
	if len(spec.Tables) > 0 {
		payload.Tables = make(map[string][]map[string]any, len(spec.Tables))
		for _, tbl := range spec.Tables {
			rows, err := src.ReadGenericTable(ctx, tbl)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("read table %s for %s: %v", tbl, spec.TargetPlugin, err))
				return rep
			}
			if len(rows) > 0 {
				payload.Tables[tbl] = rows
				readCount += len(rows)
			}
		}
	}
	rep.Read = readCount
	if readCount == 0 {
		return rep
	}
	if dryRun {
		rep.Skipped = readCount
		emit(tr("logDryRunTable", map[string]any{
			"plugin":  spec.TargetPlugin,
			"count":   readCount,
			"service": ServiceBSMigrateImport,
		},
			fmt.Sprintf("[模拟演练] %s：已读取 %d 项数据，等待调用 %s.%s", spec.TargetPlugin, readCount, spec.TargetPlugin, ServiceBSMigrateImport),
			fmt.Sprintf("[dry-run] %s: read %d item(s) ready for %s.%s", spec.TargetPlugin, readCount, spec.TargetPlugin, ServiceBSMigrateImport),
		))
		return rep
	}
	raw, err := pctx.Services.Call(ctx, spec.TargetPlugin, ServiceBSMigrateImport, payload)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("%s.%s: %v", spec.TargetPlugin, ServiceBSMigrateImport, err))
		emit(tr("logTableError", map[string]any{
			"plugin":  spec.TargetPlugin,
			"service": ServiceBSMigrateImport,
			"error":   err.Error(),
		},
			fmt.Sprintf("[错误] %s.%s 调用失败：%v", spec.TargetPlugin, ServiceBSMigrateImport, err),
			fmt.Sprintf("[error] %s.%s failed: %v", spec.TargetPlugin, ServiceBSMigrateImport, err),
		))
		return rep
	}
	var res serviceImportResult
	if err := json.Unmarshal(raw, &res); err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("decode %s.%s response: %v", spec.TargetPlugin, ServiceBSMigrateImport, err))
		return rep
	}
	rep.Imported = res.Imported
	rep.Skipped = res.Skipped
	rep.Errors = append(rep.Errors, res.Errors...)
	emit(tr("logTableProgress", map[string]any{
		"plugin":   spec.TargetPlugin,
		"done":     readCount,
		"total":    readCount,
		"imported": rep.Imported,
		"skipped":  rep.Skipped,
	},
		fmt.Sprintf("[进度] %s：已处理 %d/%d（成功 %d，跳过 %d）", spec.TargetPlugin, readCount, readCount, rep.Imported, rep.Skipped),
		fmt.Sprintf("[progress] %s: %d/%d processed (imported=%d, skipped=%d)", spec.TargetPlugin, readCount, readCount, rep.Imported, rep.Skipped),
	))
	return rep
}
