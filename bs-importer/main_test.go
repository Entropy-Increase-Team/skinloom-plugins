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
		`INSERT INTO ` + prefix + `invitation_codes VALUES (1, 'LEGACY-001', '2025-02-01 10:00:00', 0, NULL)`,
		`INSERT INTO ` + prefix + `invitation_codes VALUES (2, 'LEGACY-002', '2025-02-02 10:00:00', 5, '2025-02-03 12:00:00')`,
		`INSERT INTO ` + prefix + `mojang_verifications VALUES (1, 5, '069a79f444e94726a5befca90e38aaf5', 1, '2025-02-01 09:00:00', '2025-02-01 09:00:00')`,
		`INSERT INTO ` + prefix + `textures_description VALUES (1, 42, 'Classic Steve remix')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	return dbPath
}

func TestBSImporter_DirectSQLiteDryRunImportAndSSE(t *testing.T) {
	ctx := context.Background()
	bsPath := seedLegacyBSSQLite(t, "bs_")

	called := map[string]int{}
	h := plugintest.New(t, &BSImporterPlugin{},
		plugintest.WithPluginID("bs-importer"),
		plugintest.WithSQL(),
		plugintest.WithPermissions("admin.ui", "native.exec", "storage.sql", "http.stream", "users.read", "textures.read"),
		plugintest.WithHost(func(mh *plugintest.MockHost) {
			mh.ServiceHandler = func(_ context.Context, target, svc string, args []json.RawMessage) (any, error) {
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
	if !probe.OK || len(probe.Tables) != 3 || probe.Tables[0].Rows != 2 || probe.Tables[1].Rows != 1 || probe.Tables[2].Rows != 1 {
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

	// 4. Real run dispatches all 3 tables to target plugins and persists job in isolated SQL DB
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
		called["texture-description.importDescriptions"] != 1 {
		t.Fatalf("unexpected dispatched counts: %+v", called)
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
	if len(listOut.Items) != 2 || listOut.Items[0].ImportedCount != 4 {
		t.Fatalf("unexpected jobs list: %+v", listOut.Items)
	}
}

func TestBSImporter_HTTPBridgeImport(t *testing.T) {
	ctx := context.Background()
	const bridgeToken = "bridge-secret-token"

	bridgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bridgeToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/manifest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"bsVersion": "6.0.2",
				"extensions": map[string]any{
					"invitation-codes":    map[string]any{"table": "invitation_codes", "rows": 1},
					"mojang-verification": map[string]any{"table": "mojang_verifications", "rows": 1},
					"texture-description": map[string]any{"table": "textures_description", "rows": 1},
				},
			})
		case "/rows":
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
			mh.ServiceHandler = func(_ context.Context, _, _ string, args []json.RawMessage) (any, error) {
				var items []map[string]any
				_ = json.Unmarshal(args[0], &items)
				dispatched += len(items)
				return map[string]any{"imported": len(items), "skipped": 0, "errors": []string{}}, nil
			}
		}),
	)

	superadmin := &plugin.Viewer{Authenticated: true, UserID: 1, Role: "superadmin"}
	body, _ := json.Marshal(SourceConfig{
		Mode:        "bridge",
		BridgeURL:   bridgeSrv.URL,
		BridgeToken: bridgeToken,
		DryRun:      false,
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
	if dispatched != 3 {
		t.Fatalf("expected 3 bridged rows imported across 3 target plugins, got %d", dispatched)
	}
}
