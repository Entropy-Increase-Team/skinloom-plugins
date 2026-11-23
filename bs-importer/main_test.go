package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	plugin "github.com/Entropy-Increase-Team/skinloom/sdk/go"
	"github.com/Entropy-Increase-Team/skinloom/sdk/go/plugintest"
	_ "modernc.org/sqlite"
)

func seedLegacyBSSQLite(t *testing.T, prefix string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "legacy-bs.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy bs sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stmts := []string{
		`CREATE TABLE ` + prefix + `invitation_codes (id INTEGER PRIMARY KEY, code TEXT, generated_at TEXT, used_by INTEGER, used_at TEXT)`,
		`CREATE TABLE ` + prefix + `mojang_verifications (id INTEGER PRIMARY KEY, user_id INTEGER, uuid TEXT, verified INTEGER, created_at TEXT, updated_at TEXT)`,
		`CREATE TABLE ` + prefix + `textures_description (id INTEGER PRIMARY KEY, tid INTEGER, description TEXT)`,
		`CREATE TABLE ` + prefix + `options (id INTEGER PRIMARY KEY, option_name TEXT, option_value TEXT)`,
		`CREATE TABLE ` + prefix + `custom_ext_items (id INTEGER PRIMARY KEY, item_key TEXT, item_val TEXT)`,
		`INSERT INTO ` + prefix + `invitation_codes VALUES (1, 'LEGACY-001', '2025-02-01 10:00:00', 0, NULL)`,
		`INSERT INTO ` + prefix + `invitation_codes VALUES (2, 'LEGACY-002', '2025-02-02 10:00:00', 5, '2025-02-03 12:00:00')`,
		`INSERT INTO ` + prefix + `mojang_verifications VALUES (1, 5, '069a79f444e94726a5befca90e38aaf5', 1, '2025-02-01 09:00:00', '2025-02-01 09:00:00')`,
		`INSERT INTO ` + prefix + `textures_description VALUES (1, 42, 'Classic Steve remix')`,
		`INSERT INTO ` + prefix + `options VALUES (1, 'ext_api_root', 'https://ext.example.com/api')`,
		`INSERT INTO ` + prefix + `options VALUES (2, 'ext_flag_enabled', 'true')`,
		`INSERT INTO ` + prefix + `options VALUES (3, 'unrelated_secret', 'must-not-leak')`,
		`INSERT INTO ` + prefix + `custom_ext_items VALUES (1, 'alpha', '100')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	return dbPath
}

func TestBSImporter_DirectSQLiteDynamicDiscoveryDryRunAndSSE(t *testing.T) {
	ctx := context.Background()
	bsPath := seedLegacyBSSQLite(t, "bs_")

	called := map[string]int{}
	var receivedGeneric GenericMigrationPayload
	h := plugintest.New(t, &BSImporterPlugin{},
		plugintest.WithPluginID("bs-importer"),
		plugintest.WithSQL(),
		plugintest.WithPermissions("admin.ui", "native.exec", "storage.sql", "http.stream", "users.read", "textures.read"),
		plugintest.WithHost(func(mh *plugintest.MockHost) {
			mh.DiscoverHandler = func(_ context.Context, svcName string) ([]string, error) {
				if svcName == ServiceBSMigrateDescribe {
					return []string{"custom-ext"}, nil
				}
				return nil, nil
			}
			mh.ServiceHandler = func(_ context.Context, target, svc string, args []json.RawMessage) (any, error) {
				if target == "custom-ext" && svc == ServiceBSMigrateDescribe {
					return map[string]any{
						"label":          map[string]string{"zh-CN": "自定义扩展配置", "en": "Custom Extension"},
						"description":    map[string]string{"zh-CN": "示例通用迁移提供方", "en": "Generic migration provider"},
						"optionKeys":     []string{"ext_api_root"},
						"optionPrefixes": []string{"ext_flag_"},
						"tables":         []string{"custom_ext_items"},
					}, nil
				}
				if target == "custom-ext" && svc == ServiceBSMigrateImport {
					if len(args) > 0 {
						_ = json.Unmarshal(args[0], &receivedGeneric)
					}
					n := len(receivedGeneric.Options) + len(receivedGeneric.Tables["custom_ext_items"])
					called[target+"."+svc] += n
					return map[string]any{"imported": n, "skipped": 0, "errors": []string{}}, nil
				}
				var items []map[string]any
				if len(args) > 0 {
					_ = json.Unmarshal(args[0], &items)
				}
				called[target+"."+svc] += len(items)
				return map[string]any{"imported": len(items), "skipped": 0, "errors": []string{}}, nil
			}
		}),
	)

	superadmin := &plugin.Viewer{Authenticated: true, UserID: 1, Role: "superadmin"}

	// 0. GET /admin/providers dynamically includes discovered plugin provider
	provResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodGet, Path: "/admin/providers", User: superadmin})
	if err != nil || provResp.Status != http.StatusOK || !strings.Contains(string(provResp.RawBody), "plugin:custom-ext") {
		t.Fatalf("GET /admin/providers failed: status=%v err=%v body=%s", provResp, err, string(provResp.RawBody))
	}

	// 1. Reject unsafe table prefix (SQL injection protection)
	badPrefixBody, _ := json.Marshal(SourceConfig{
		Mode:        "db",
		Driver:      "sqlite",
		DSN:         bsPath,
		TablePrefix: "bs_; DROP TABLE users;--",
	})
	badResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodPost,
		Path:   "/admin/test",
		Body:   badPrefixBody,
		User:   superadmin,
	})
	if _, isRej := plugin.IsRejection(err); !isRej && (err != nil || badResp.Status < 400) {
		t.Fatalf("expected rejection for malicious tablePrefix, got resp=%+v err=%v", badResp, err)
	}

	// 2. Test valid SQLite source with prefix "bs_"
	testBody, _ := json.Marshal(SourceConfig{
		Mode:        "db",
		Driver:      "sqlite",
		DSN:         bsPath,
		TablePrefix: "bs_",
	})
	probeResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodPost,
		Path:   "/admin/test",
		Body:   testBody,
		User:   superadmin,
	})
	if err != nil || probeResp.Status != http.StatusOK {
		t.Fatalf("POST /admin/test: status=%d err=%v body=%s", probeResp.Status, err, string(probeResp.RawBody))
	}
	var probe TestSourceResult
	if err := json.Unmarshal(probeResp.RawBody, &probe); err != nil {
		t.Fatalf("decode probe: %v", err)
	}
	if !probe.OK || len(probe.Tables) != 4 || probe.Tables[0].Rows != 2 || probe.Tables[1].Rows != 1 || probe.Tables[2].Rows != 1 || probe.Tables[3].Rows != 3 {
		t.Fatalf("unexpected probe result: %+v", probe)
	}

	// 3. Dry-run reads rows without calling target plugin write services
	dryBody, _ := json.Marshal(SourceConfig{
		Mode:        "db",
		Driver:      "sqlite",
		DSN:         bsPath,
		TablePrefix: "bs_",
		DryRun:      true,
	})
	dryResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodPost,
		Path:   "/admin/run",
		Body:   dryBody,
		User:   superadmin,
	})
	if err != nil || dryResp.Status != http.StatusOK {
		t.Fatalf("dry-run failed: status=%d err=%v", dryResp.Status, err)
	}
	if len(called) != 0 {
		t.Fatalf("expected zero service writes during dry-run, got %+v", called)
	}

	// 4. Real run dispatches 3 legacy tables + dynamic provider (and NEVER leaks undeclared options!)
	runBody, _ := json.Marshal(SourceConfig{
		Mode:        "db",
		Driver:      "sqlite",
		DSN:         bsPath,
		TablePrefix: "bs_",
		DryRun:      false,
	})
	runResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodPost,
		Path:   "/admin/run",
		Body:   runBody,
		User:   superadmin,
		Locale: "zh-CN",
	})
	if err != nil || runResp.Status != http.StatusOK {
		t.Fatalf("real import failed: status=%d err=%v body=%s", runResp.Status, err, string(runResp.RawBody))
	}
	if called["invitation-codes.importCodes"] != 2 ||
		called["mojang-verification.importVerifications"] != 1 ||
		called["texture-description.importDescriptions"] != 1 ||
		called["custom-ext.bsMigrateImport"] != 3 {
		t.Fatalf("unexpected dispatched counts: %+v", called)
	}
	if _, leaked := receivedGeneric.Options["unrelated_secret"]; leaked {
		t.Fatal("least-privilege violation: undeclared BS option leaked to plugin provider")
	}
	if receivedGeneric.Options["ext_api_root"] != "https://ext.example.com/api" ||
		receivedGeneric.Options["ext_flag_enabled"] != "true" ||
		len(receivedGeneric.Tables["custom_ext_items"]) != 1 {
		t.Fatalf("unexpected generic migration payload: %+v", receivedGeneric)
	}

	// 5. Stream endpoint replays localized Chinese job events cleanly as SSE
	sseReq := httptest.NewRequest(http.MethodGet, "/admin/jobs/2/stream", nil)
	sseReq.Header.Set("X-SkinLoom-User-ID", "1")
	sseReq.Header.Set("X-SkinLoom-User-Role", "superadmin")
	sseReq.Header.Set("X-SkinLoom-Locale", "zh-CN")
	rec := h.DoStreamHTTP(sseReq)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: done") || !strings.Contains(rec.Body.String(), "任务 #2") || !strings.Contains(rec.Body.String(), "直连数据库") {
		t.Fatalf("unexpected SSE stream response: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 6. Jobs history lists both dry-run and real run
	jobsResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodGet,
		Path:   "/admin/jobs",
		User:   superadmin,
	})
	if err != nil || jobsResp.Status != http.StatusOK {
		t.Fatalf("GET /admin/jobs: status=%d err=%v", jobsResp.Status, err)
	}
	var listOut struct {
		Items []JobSummary `json:"items"`
	}
	if err := json.Unmarshal(jobsResp.RawBody, &listOut); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(listOut.Items) != 2 || listOut.Items[0].ImportedCount != 7 {
		t.Fatalf("unexpected jobs list: %+v", listOut.Items)
	}
}

func TestBSImporter_HTTPBridgeImportWithDynamicProvider(t *testing.T) {
	ctx := context.Background()
	const bridgeToken = "bridge-secret-token"

	bridgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bridgeToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/plugin/skinloom-bridge/manifest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"bsVersion": "6.0.2",
				"options": map[string]string{
					"ext_api_root":     "https://bridge.example.com/api",
					"ext_flag_enabled": "true",
					"unrelated_secret": "must-not-leak",
				},
				"extensions": map[string]any{
					"invitation-codes":    map[string]any{"table": "invitation_codes", "rows": 1},
					"mojang-verification": map[string]any{"table": "mojang_verifications", "rows": 1},
					"texture-description": map[string]any{"table": "textures_description", "rows": 1},
				},
			})
		case "/plugin/skinloom-bridge/rows":
			switch r.URL.Query().Get("table") {
			case TableInvitationCodes:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"rows":    []InvitationCodeRow{{Code: "BRIDGE-001", UsedBy: 0}},
					"hasMore": false,
				})
			case TableMojangVerification:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"rows":    []MojangVerificationRow{{UserID: 7, UUID: "069a79f444e94726a5befca90e38aaf5", Verified: true}},
					"hasMore": false,
				})
			case TableTextureDescription:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"rows":    []TextureDescriptionRow{{TextureID: 12, Description: "From bridge"}},
					"hasMore": false,
				})
			default:
				http.Error(w, "invalid table", http.StatusBadRequest)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer bridgeSrv.Close()

	dispatched := 0
	h := plugintest.New(t, &BSImporterPlugin{},
		plugintest.WithPluginID("bs-importer"),
		plugintest.WithSQL(),
		plugintest.WithHost(func(mh *plugintest.MockHost) {
			mh.DiscoverHandler = func(_ context.Context, svcName string) ([]string, error) {
				if svcName == ServiceBSMigrateDescribe {
					return []string{"custom-ext"}, nil
				}
				return nil, nil
			}
			mh.ServiceHandler = func(_ context.Context, target, svc string, args []json.RawMessage) (any, error) {
				if target == "custom-ext" && svc == ServiceBSMigrateDescribe {
					return map[string]any{
						"label":      map[string]string{"en": "Custom Extension"},
						"optionKeys": []string{"ext_api_root", "ext_flag_enabled"},
					}, nil
				}
				if target == "custom-ext" && svc == ServiceBSMigrateImport {
					var p GenericMigrationPayload
					_ = json.Unmarshal(args[0], &p)
					if _, leaked := p.Options["unrelated_secret"]; leaked {
						t.Error("undeclared option leaked via bridge")
					}
					dispatched += len(p.Options)
					return map[string]any{"imported": len(p.Options), "skipped": 0, "errors": []string{}}, nil
				}
				var items []map[string]any
				_ = json.Unmarshal(args[0], &items)
				dispatched += len(items)
				return map[string]any{"imported": len(items), "skipped": 0, "errors": []string{}}, nil
			}
		}),
	)

	superadmin := &plugin.Viewer{Authenticated: true, UserID: 1, Role: "superadmin"}
	credJSON, _ := json.Marshal(map[string]string{"url": bridgeSrv.URL, "token": bridgeToken})
	body, _ := json.Marshal(SourceConfig{
		Mode:      "bridge",
		BridgeURL: string(credJSON),
		DryRun:    false,
	})
	resp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodPost,
		Path:   "/admin/run",
		Body:   body,
		User:   superadmin,
	})
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("bridge import failed: status=%d err=%v body=%s", resp.Status, err, string(resp.RawBody))
	}
	if dispatched != 5 {
		t.Fatalf("expected 5 bridged items imported across 3 legacy + 1 dynamic provider, got %d", dispatched)
	}
}

func TestBSImporter_SecurityRegressions(t *testing.T) {
	ctx := context.Background()

	// 1. M-7: parseSQLBool defaults nil/unknown/0 to false and handles typed ints/bytes/strings.
	for _, falsy := range []any{nil, false, int64(0), int32(0), uint64(0), float64(0), []byte("0"), "false", "0", ""} {
		if parseSQLBool(falsy) {
			t.Fatalf("M-7 regression: parseSQLBool(%#v) = true, want false", falsy)
		}
	}
	for _, truthy := range []any{true, int64(1), int32(1), uint64(1), float64(1), []byte("1"), "1", "true", "TRUE", "t"} {
		if !parseSQLBool(truthy) {
			t.Fatalf("M-7 regression: parseSQLBool(%#v) = false, want true", truthy)
		}
	}

	// 2. M-4: Malformed Bridge JSON bundle must NOT echo the secret token in error messages.
	_, err := openBridgeSource(SourceConfig{
		Mode:      "bridge",
		BridgeURL: `{"url":"https://skin.example.com","token":"SUPER-SECRET-TOKEN-999"`,
	})
	if err == nil {
		t.Fatal("expected error for malformed JSON bundle")
	}
	if strings.Contains(err.Error(), "SUPER-SECRET-TOKEN-999") {
		t.Fatalf("M-4 regression: error echoed secret token: %v", err)
	}
	if scrubbed := scrubDSN("mysql", "root:TOPSECRET_PW@tcp(db.local:3306)/bs"); strings.Contains(scrubbed, "TOPSECRET_PW") {
		t.Fatalf("M-4 regression: scrubDSN leaked password: %q", scrubbed)
	}

	// 3. C-1 & LIKE ESCAPE: Malicious provider requesting core tables, broad prefixes, or sensitive option keys
	// must be blocked, and underscore in prefix "ext_" must NOT match "extX_secret".
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "bs_sec.sqlite")
	srcDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer srcDB.Close()
	for _, q := range []string{
		`CREATE TABLE users (uid INTEGER PRIMARY KEY, email TEXT, password TEXT);`,
		`INSERT INTO users (uid, email, password) VALUES (1, 'admin@example.com', '$2y$10$hash');`,
		`CREATE TABLE oauth_clients (id INTEGER PRIMARY KEY, secret TEXT);`,
		`INSERT INTO oauth_clients (id, secret) VALUES (1, 'client-secret');`,
		`CREATE TABLE options (option_name TEXT PRIMARY KEY, option_value TEXT);`,
		`INSERT INTO options (option_name, option_value) VALUES ('ygg_private_key', '-----BEGIN PRIVATE KEY-----');`,
		`INSERT INTO options (option_name, option_value) VALUES ('oauth_client_secret', 'top-secret');`,
		`INSERT INTO options (option_name, option_value) VALUES ('ext_allowed_opt', 'ok');`,
		`INSERT INTO options (option_name, option_value) VALUES ('extX_wildcard_leak', 'must-not-match');`,
		`CREATE TABLE custom_plugin_data (id INTEGER PRIMARY KEY, val TEXT);`,
		`INSERT INTO custom_plugin_data (id, val) VALUES (1, 'safe-val');`,
	} {
		if _, err := srcDB.Exec(q); err != nil {
			t.Fatalf("seed %s: %v", q, err)
		}
	}

	var received GenericMigrationPayload
	h := plugintest.New(t, &BSImporterPlugin{},
		plugintest.WithPluginID("bs-importer"),
		plugintest.WithSQL(),
		plugintest.WithHost(func(mh *plugintest.MockHost) {
			mh.DiscoverHandler = func(_ context.Context, svcName string) ([]string, error) {
				if svcName == ServiceBSMigrateDescribe {
					return []string{"greedy-plugin"}, nil
				}
				return nil, nil
			}
			mh.ServiceHandler = func(_ context.Context, target, svc string, args []json.RawMessage) (any, error) {
				if target == "greedy-plugin" && svc == ServiceBSMigrateDescribe {
					return map[string]any{
						"label":          map[string]string{"en": "Greedy"},
						"optionKeys":     []string{"ygg_private_key", "oauth_client_secret", "ext_allowed_opt"},
						"optionPrefixes": []string{"a", "u_", "ext_"},
						"tables":         []string{"users", "oauth_clients", "options", "custom_plugin_data"},
					}, nil
				}
				if target == "greedy-plugin" && svc == ServiceBSMigrateImport {
					_ = json.Unmarshal(args[0], &received)
					return map[string]any{"imported": len(received.Options), "skipped": 0, "errors": []string{}}, nil
				}
				return map[string]any{"imported": 0, "skipped": 0, "errors": []string{}}, nil
			}
		}),
	)

	superadmin := &plugin.Viewer{Authenticated: true, UserID: 1, Role: "superadmin"}
	body, _ := json.Marshal(SourceConfig{
		Mode:   "db",
		Driver: "sqlite",
		DSN:    dbPath,
		DryRun: false,
	})
	resp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodPost,
		Path:   "/admin/run",
		Body:   body,
		User:   superadmin,
	})
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("run failed: status=%d body=%s", resp.Status, string(resp.RawBody))
	}
	if _, leaked := received.Options["ygg_private_key"]; leaked {
		t.Fatal("C-1 regression: ygg_private_key leaked to dynamic provider")
	}
	if _, leaked := received.Options["oauth_client_secret"]; leaked {
		t.Fatal("C-1 regression: oauth_client_secret leaked to dynamic provider")
	}
	if _, leaked := received.Options["extX_wildcard_leak"]; leaked {
		t.Fatal("LIKE ESCAPE regression: unescaped '_' in prefix ext_ matched extX_wildcard_leak")
	}
	if received.Options["ext_allowed_opt"] != "ok" {
		t.Fatalf("expected allowed option ext_allowed_opt=ok, got %+v", received.Options)
	}
	for _, forbiddenTable := range []string{"users", "oauth_clients", "options"} {
		if _, leaked := received.Tables[forbiddenTable]; leaked {
			t.Fatalf("C-1 regression: core sensitive table %q leaked to dynamic provider", forbiddenTable)
		}
	}
	if len(received.Tables["custom_plugin_data"]) != 1 {
		t.Fatalf("expected non-core custom_plugin_data allowed, got %+v", received.Tables)
	}
}
