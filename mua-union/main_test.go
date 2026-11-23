package main

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	plugin "github.com/Entropy-Increase-Team/skinloom/sdk/go"
	"github.com/Entropy-Increase-Team/skinloom/sdk/go/plugintest"
)

type mockUnionHub struct {
	t           *testing.T
	srv         *httptest.Server
	hostPriv    *rsa.PrivateKey
	hostPubPEM  string
	oauthPriv   *rsa.PrivateKey
	oauthPubPEM string
	yggPrivPEM  string

	mu              sync.Mutex
	expectedKey     string
	failProfileSync bool
	blacklisted     map[string]bool
	lastSyncMap     map[string]string
	profileOps      []string
	blacklistCalls  []string
}

func newMockUnionHub(t *testing.T, memberKey string) *mockUnionHub {
	t.Helper()
	hostPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host rsa key: %v", err)
	}
	hostPubDER, _ := x509.MarshalPKIXPublicKey(&hostPriv.PublicKey)
	hostPubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: hostPubDER}))

	oauthPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate oauth rsa key: %v", err)
	}
	oauthPubDER, _ := x509.MarshalPKIXPublicKey(&oauthPriv.PublicKey)
	oauthPubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: oauthPubDER}))

	yggPrivPEM, _, err := generateRSA4096KeyPairPEM()
	if err != nil {
		t.Fatalf("generate 4096 ygg key: %v", err)
	}

	m := &mockUnionHub{
		t:           t,
		hostPriv:    hostPriv,
		hostPubPEM:  hostPubPEM,
		oauthPriv:   oauthPriv,
		oauthPubPEM: oauthPubPEM,
		yggPrivPEM:  yggPrivPEM,
		expectedKey: memberKey,
		blacklisted: map[string]bool{"banned@example.com": true},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/union", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":                         12,
			"privateKeyVersion":               5,
			"union_host_signature_public_key": m.hostPubPEM,
			"meta": map[string]string{
				"description":    "MUA 联合认证简介",
				"description_en": "MUA Union Intro",
				"admin_notice":   "欢迎接入 MUA Union",
			},
		})
	})

	requireKey := func(w http.ResponseWriter, r *http.Request) bool {
		m.mu.Lock()
		want := m.expectedKey
		m.mu.Unlock()
		if r.Header.Get("X-Union-Member-Key") != want {
			http.Error(w, "invalid member key", http.StatusUnauthorized)
			return false
		}
		return true
	}

	mux.HandleFunc("/api/union/serverlist", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": 12,
			"servers": []map[string]string{
				{"code": "PKU", "bs_root": "https://skin.pku.example.edu.cn", "name": "PKU Skin"},
				{"code": "THU", "bs_root": "https://skin.thu.example.edu.cn:8443/sub", "name": "THU Skin"},
			},
		})
	})

	mux.HandleFunc("/api/union/privatekey", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"privateKey":        m.yggPrivPEM,
			"privateKeyVersion": 5,
		})
	})

	mux.HandleFunc("/api/union/sync", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		var body struct {
			ProfileList map[string]string `json:"profileList"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.lastSyncMap = body.ProfileList
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/union/profile", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		m.mu.Lock()
		fail := m.failProfileSync
		m.mu.Unlock()
		if fail {
			http.Error(w, "hub overloaded", http.StatusServiceUnavailable)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.profileOps = append(m.profileOps, r.Method+" /profile "+string(raw))
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/union/profile/", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/union/profile/")
		switch {
		case strings.HasPrefix(rest, "detail/"):
			uuid := strings.TrimPrefix(rest, "detail/")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"internal_id":      101,
				"uuid":             uuid,
				"mapped_uuid":      uuid,
				"name":             "Alice",
				"bind_mapped_name": "Alice",
				"bind_status":      0,
				"backend":          "https://skin.pku.example.edu.cn",
				"backend_scopes":   map[string]string{"self": "PKU"},
				"bind": []map[string]any{
					{"internal_id": 102, "uuid": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "bind_status": 1},
				},
			})
		case strings.HasPrefix(rest, "unmapped/byname/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"internal_id": 101, "bind_mapped_name": "Alice", "backend_scopes": map[string]string{"self": "PKU"}},
				{"internal_id": 102, "bind_mapped_name": "Alice", "backend_scopes": map[string]string{"self": "THU"}},
				{"internal_id": 103, "bind_mapped_name": "Alice_SJTU", "backend_scopes": map[string]string{"self": "SJTU"}},
			})
		case rest == "bind":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "BIND-TOKEN-999"})
		case rest == "bindto" || rest == "unbind" || rest == "remapuuid":
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			m.mu.Lock()
			fail := m.failProfileSync
			m.mu.Unlock()
			if fail {
				http.Error(w, "hub overloaded", http.StatusServiceUnavailable)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			m.mu.Lock()
			m.profileOps = append(m.profileOps, r.Method+" /profile/"+rest+" "+string(raw))
			m.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	})

	mux.HandleFunc("/api/union/oauth2/backend", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"publicKey": m.oauthPubPEM,
		})
	})

	mux.HandleFunc("/api/union/code", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		want := m.expectedKey
		m.mu.Unlock()
		if body.Token != want {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "SLM"})
	})

	mux.HandleFunc("/api/union/backend/SLM/security/level", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("3"))
	})

	mux.HandleFunc("/api/union/diagnose", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"healthy":   true,
			"delay":     0.023,
			"timestamp": "2026-11-23T04:30:00Z",
		})
	})

	mux.HandleFunc("/api/union/blacklist/checkallowed", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		email := r.URL.Query().Get("email")
		m.mu.Lock()
		blocked := m.blacklisted[email]
		m.mu.Unlock()
		if blocked {
			_, _ = w.Write([]byte("false"))
			return
		}
		_, _ = w.Write([]byte("true"))
	})

	mux.HandleFunc("/api/union/blacklist/query", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"current_page": 1,
			"last_page":    1,
			"data": []map[string]any{
				{"id": 7, "email": "banned@example.com", "source": "SLM", "reason": "griefing"},
			},
		})
	})

	mux.HandleFunc("/api/union/blacklist/restful", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		m.mu.Lock()
		m.blacklistCalls = append(m.blacklistCalls, r.Method+" /blacklist/restful")
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 8})
	})

	mux.HandleFunc("/api/union/blacklist/restful/", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		m.mu.Lock()
		m.blacklistCalls = append(m.blacklistCalls, r.Method+" "+r.URL.Path)
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/union/blacklist/invalidate/", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		m.mu.Lock()
		m.blacklistCalls = append(m.blacklistCalls, r.Method+" "+r.URL.Path)
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockUnionHub) signHeaders(body []byte, ts int64, nonce string) map[string]string {
	tsStr := strconv.FormatInt(ts, 10)
	h := sha256.New()
	_, _ = h.Write(body)
	_, _ = h.Write([]byte(tsStr))
	_, _ = h.Write([]byte(nonce))
	sig, err := rsa.SignPKCS1v15(rand.Reader, m.hostPriv, crypto.SHA256, h.Sum(nil))
	if err != nil {
		m.t.Fatalf("sign webhook: %v", err)
	}
	return map[string]string{
		"x-message-signature": base64.StdEncoding.EncodeToString(sig),
		"x-message-timestamp": tsStr,
		"x-message-nonce":     nonce,
	}
}

func setupTestPlugin(t *testing.T, hub *mockUnionHub, extraSettings map[string]any) (*MUAUnionPlugin, *plugintest.Harness, *time.Time) {
	t.Helper()
	now := time.Unix(1763900000, 0)
	p := &MUAUnionPlugin{
		httpClient:        hub.srv.Client(),
		allowInsecureTest: true,
		clock:             func() time.Time { return now },
	}
	settings := map[string]any{
		"unionApiRoot":                hub.srv.URL + "/api/union",
		"unionMemberKey":              "member-secret-initial",
		"enableUnionOAuth2":           false,
		"enableLocalBlacklist":        false,
		"allowRemoteKeySync":          false,
		"allowRemoteUUIDRemap":        false,
		"allowRemoteBackendKeyRotate": false,
		"invitationCodesForUnion":     true,
	}
	for k, v := range extraSettings {
		settings[k] = v
	}

	h := plugintest.New(t, p,
		plugintest.WithPluginID("mua-union"),
		plugintest.WithVersion("1.0.0"),
		plugintest.WithSQL(),
		plugintest.WithSettings(settings),
		plugintest.WithPermissions(
			"admin.ui", "native.exec", "storage.sql", "routes.public",
			"yggdrasil.extend", "yggdrasil.key.write", "players.uuid.write",
			"users.read", "players.read", "cron",
		),
		plugintest.WithHost(func(mh *plugintest.MockHost) {
			mh.SitePublic["auth.require_email_verification"] = true
			mh.SitePublic["player.keep_uuid_on_rename"] = true
			mh.SitePublic["yggdrasil.uuid_algorithm"] = "v4"
			mh.Users[1] = &plugin.User{ID: 1, Email: "alice@example.com", Nickname: "AliceNick", Role: "user", Status: "active", EmailVerified: true}
			mh.Users[2] = &plugin.User{ID: 2, Email: "banned@example.com", Nickname: "BadActor", Role: "user", Status: "active", EmailVerified: true}
			mh.Users[3] = &plugin.User{ID: 3, Email: "unverified@example.com", Nickname: "NewUser", Role: "user", Status: "active", EmailVerified: false}
			mh.Users[4] = &plugin.User{ID: 4, Email: "suspended@example.com", Nickname: "Suspended", Role: "user", Status: "banned", EmailVerified: true}
			mh.Players[10] = &plugin.Player{ID: 10, UserID: 1, Name: "Alice", UUID: "11111111111111111111111111111111"}
			mh.Players[20] = &plugin.Player{ID: 20, UserID: 2, Name: "Bob", UUID: "22222222222222222222222222222222"}
		}),
	)
	return p, h, &now
}

func TestMUAUnion_WebhooksVerificationAndHighRiskGates(t *testing.T) {
	ctx := context.Background()
	hub := newMockUnionHub(t, "member-secret-initial")
	_, h, now := setupTestPlugin(t, hub, nil)

	// 1. Public capability discovery (/api/union/member)
	helloResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodGet, Path: "/api/union/member"})
	if err != nil || helloResp.Status != http.StatusOK {
		t.Fatalf("GET /api/union/member failed: status=%v err=%v", helloResp, err)
	}
	var hello struct {
		YggdrasilAPIVersion string   `json:"yggdrasilApiVersion"`
		EnabledFeatures     []string `json:"enabledFeatures"`
	}
	_ = json.Unmarshal(helloResp.RawBody, &hello)
	if !strings.Contains(hello.YggdrasilAPIVersion, "skinloom") {
		t.Fatalf("unexpected yggdrasilApiVersion: %q", hello.YggdrasilAPIVersion)
	}
	featuresJoined := strings.Join(hello.EnabledFeatures, ",")
	if !strings.Contains(featuresJoined, "unionBlacklist") || !strings.Contains(featuresJoined, "emailVerification") || !strings.Contains(featuresJoined, "invitationCodesForUnion") {
		t.Fatalf("missing expected enabledFeatures: %v", hello.EnabledFeatures)
	}

	// 2. UnionHostVerify rejects missing headers, expired timestamp, bad signature, and replayed nonce.
	resp, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/updatelist"})
	if resp.Status != http.StatusForbidden {
		t.Fatalf("expected 403 for unsigned webhook, got %d", resp.Status)
	}
	expiredHdrs := hub.signHeaders(nil, now.Unix()-15, "nonce-expired")
	resp, _ = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/updatelist", Headers: expiredHdrs})
	if resp.Status != http.StatusForbidden {
		t.Fatalf("expected 403 for expired timestamp, got %d", resp.Status)
	}
	badSigHdrs := hub.signHeaders([]byte("tampered"), now.Unix(), "nonce-bad")
	resp, _ = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/updatelist", Body: []byte("{}"), Headers: badSigHdrs})
	if resp.Status != http.StatusForbidden {
		t.Fatalf("expected 403 for mismatched body signature, got %d", resp.Status)
	}

	// 3. Valid /updatelist updates cached server list & Yggdrasil skinDomains immediately.
	validHdrs := hub.signHeaders([]byte("{}"), now.Unix(), "nonce-list-1")
	resp, err = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/updatelist", Body: []byte("{}"), Headers: validHdrs})
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("valid /updatelist failed: status=%d err=%v body=%s", resp.Status, err, string(resp.RawBody))
	}
	// Replay of exact same nonce must fail with 403.
	replayResp, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/updatelist", Body: []byte("{}"), Headers: validHdrs})
	if replayResp.Status != http.StatusForbidden {
		t.Fatalf("expected 403 for replayed nonce, got %d", replayResp.Status)
	}

	metaExt, err := h.TriggerYggdrasilMeta(ctx)
	if err != nil {
		t.Fatalf("TriggerYggdrasilMeta failed: %v", err)
	}
	joinedDomains := strings.Join(metaExt.SkinDomains, ",")
	if !strings.Contains(joinedDomains, "skin.pku.example.edu.cn") || !strings.Contains(joinedDomains, "skin.thu.example.edu.cn") {
		t.Fatalf("expected Union member skinDomains in Yggdrasil meta, got %v", metaExt.SkinDomains)
	}

	// 4. High-risk webhooks refused (403) while opt-in settings are false.
	for _, tc := range []struct {
		path string
		body []byte
	}{
		{"/api/union/member/updateprivatekey", []byte("{}")},
		{"/api/union/member/remapuuid", []byte(`{"remapped_uuid":{"11111111111111111111111111111111":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`)},
		{"/api/union/member/updatebackendkey", []byte(`{"key":"member-secret-rotated"}`)},
	} {
		hdrs := hub.signHeaders(tc.body, now.Unix(), "nonce-gate-"+tc.path)
		r, err := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: tc.path, Body: tc.body, Headers: hdrs})
		if err != nil || r.Status != http.StatusForbidden {
			t.Fatalf("expected 403 for %s when opt-in disabled, got status=%d err=%v", tc.path, r.Status, err)
		}
	}

	// 5. Enable opt-in high-risk toggles and verify each high-risk webhook succeeds + writes audit logs.
	if err := h.UpdateSettings(ctx, map[string]any{
		"unionApiRoot":                hub.srv.URL + "/api/union",
		"unionMemberKey":              "member-secret-initial",
		"allowRemoteKeySync":          true,
		"allowRemoteUUIDRemap":        true,
		"allowRemoteBackendKeyRotate": true,
	}); err != nil {
		t.Fatalf("UpdateSettings failed: %v", err)
	}

	// 5a. updateprivatekey
	hdrs := hub.signHeaders([]byte("{}"), now.Unix(), "nonce-key-ok")
	resp, err = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/updateprivatekey", Body: []byte("{}"), Headers: hdrs})
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("updateprivatekey failed: status=%d err=%v body=%s", resp.Status, err, string(resp.RawBody))
	}
	if h.Host().YggdrasilPrivKey == "" {
		t.Fatal("expected host YggdrasilPrivKey to be updated")
	}

	// 5b. remapuuid
	remapBody := []byte(`{"remapped_uuid":{"11111111111111111111111111111111":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`)
	hdrs = hub.signHeaders(remapBody, now.Unix(), "nonce-remap-ok")
	resp, err = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/remapuuid", Body: remapBody, Headers: hdrs})
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("remapuuid failed: status=%d err=%v body=%s", resp.Status, err, string(resp.RawBody))
	}
	if h.Host().Players[10].UUID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("expected player 10 UUID remapped, got %q", h.Host().Players[10].UUID)
	}

	// 5c. updatebackendkey rotates runtime key used on subsequent outbound calls
	rotBody := []byte(`{"key":"member-secret-rotated"}`)
	hdrs = hub.signHeaders(rotBody, now.Unix(), "nonce-rot-ok")
	resp, err = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/updatebackendkey", Body: rotBody, Headers: hdrs})
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("updatebackendkey failed: status=%d err=%v", resp.Status, err)
	}
	hub.mu.Lock()
	hub.expectedKey = "member-secret-rotated"
	hub.mu.Unlock()

	// 6. /sync uses rotated key and sends uuid->name map
	hdrs = hub.signHeaders([]byte("{}"), now.Unix(), "nonce-sync-ok")
	resp, err = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/sync", Body: []byte("{}"), Headers: hdrs})
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("sync failed: status=%d err=%v body=%s", resp.Status, err, string(resp.RawBody))
	}
	hub.mu.Lock()
	syncedName := hub.lastSyncMap["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]
	hub.mu.Unlock()
	if syncedName != "Alice" {
		t.Fatalf("expected synced profile map to contain remapped Alice, got %q", syncedName)
	}

	// 7. /queryemail returns 200 for existing player and 204 for unknown player
	hdrs = hub.signHeaders(nil, now.Unix(), "nonce-qe-1")
	resp, _ = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodGet, Path: "/api/union/member/queryemail", Query: map[string]string{"username": "Alice"}, Headers: hdrs})
	if resp.Status != http.StatusOK || !strings.Contains(string(resp.RawBody), "alice@example.com") {
		t.Fatalf("queryemail existing failed: status=%d body=%s", resp.Status, string(resp.RawBody))
	}
	hdrs = hub.signHeaders(nil, now.Unix(), "nonce-qe-2")
	resp, _ = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodGet, Path: "/api/union/member/queryemail", Query: map[string]string{"username": "Ghost"}, Headers: hdrs})
	if resp.Status != http.StatusNoContent {
		t.Fatalf("queryemail missing expected 204, got %d", resp.Status)
	}

	// 8. /diagnose echoes nonce + float timestamp; /updateplugin refuses with 501
	diagBody := []byte(`{"nonce":"diag-123"}`)
	hdrs = hub.signHeaders(diagBody, now.Unix(), "nonce-diag-1")
	resp, _ = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/diagnose", Body: diagBody, Headers: hdrs})
	if resp.Status != http.StatusOK || !strings.Contains(string(resp.RawBody), "diag-123") {
		t.Fatalf("diagnose failed: status=%d body=%s", resp.Status, string(resp.RawBody))
	}
	hdrs = hub.signHeaders([]byte("{}"), now.Unix(), "nonce-upd-501")
	resp, _ = h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/api/union/member/updateplugin", Body: []byte("{}"), Headers: hdrs})
	if resp.Status != http.StatusNotImplemented {
		t.Fatalf("expected 501 for /updateplugin RCE refusal, got %d", resp.Status)
	}
	if len(h.Host().Audits) < 3 {
		t.Fatalf("expected audit logs for key sync, uuid remap, and member key rotate; got %d", len(h.Host().Audits))
	}
}

func TestMUAUnion_OAuth2ChunkedEncryptAndVerify(t *testing.T) {
	ctx := context.Background()
	hub := newMockUnionHub(t, "member-secret-initial")
	_, h, _ := setupTestPlugin(t, hub, map[string]any{"enableUnionOAuth2": true})

	// 1. Fetch site's auto-generated OAuth2 RSA-4096 signing public key
	pubResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodGet, Path: "/api/union/member/oauth2"})
	if err != nil || pubResp.Status != http.StatusOK {
		t.Fatalf("GET /api/union/member/oauth2 failed: status=%v err=%v", pubResp, err)
	}
	var pubOut struct {
		SignaturePublicKey string `json:"signaturePublicKey"`
	}
	_ = json.Unmarshal(pubResp.RawBody, &pubOut)
	sitePubKey, err := parseRSAPublicKeyPEM(pubOut.SignaturePublicKey)
	if err != nil || sitePubKey.N.BitLen() < 4096 {
		t.Fatalf("expected valid 4096-bit RSA OAuth2 public key, err=%v", err)
	}

	// 2. Unauthenticated grant redirects to /auth/login?next=...
	anonResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodGet,
		Path:   "/api/union/member/oauth2/grant",
		Query:  map[string]string{"state": "xyz123"},
	})
	if err != nil || anonResp.Status != http.StatusFound || !strings.HasPrefix(anonResp.Headers["Location"], "/auth/login?next=") {
		t.Fatalf("expected 302 login redirect for unauthenticated grant, got status=%d loc=%q err=%v", anonResp.Status, anonResp.Headers["Location"], err)
	}

	// 3. Authenticated grant redirects to Hub /oauth2/continue with verifiable chunk-encrypted userInfoToken
	grantResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodGet,
		Path:   "/api/union/member/oauth2/grant",
		Query:  map[string]string{"state": "xyz123", "client_id": "mua-forum"},
		User:   &plugin.Viewer{Authenticated: true, UserID: 1, Email: "alice@example.com", Nickname: "AliceNick", Role: "user"},
	})
	if err != nil || grantResp.Status != http.StatusFound {
		t.Fatalf("authenticated grant failed: status=%d err=%v body=%s", grantResp.Status, err, string(grantResp.RawBody))
	}
	locURL, err := url.Parse(grantResp.Headers["Location"])
	if err != nil || !strings.HasSuffix(locURL.Path, "/api/union/oauth2/continue") {
		t.Fatalf("unexpected redirect location: %q", grantResp.Headers["Location"])
	}
	if locURL.Query().Get("state") != "xyz123" || locURL.Query().Get("client_id") != "mua-forum" {
		t.Fatalf("original query params were not preserved: %s", locURL.RawQuery)
	}

	// Decrypt chunked PKCS#1 v1.5 token using Hub's private key
	plain, err := decryptChunkedPKCS1v15(locURL.Query().Get("userInfoToken"), hub.oauthPriv)
	if err != nil {
		t.Fatalf("decryptChunkedPKCS1v15 failed: %v", err)
	}
	var token struct {
		UserInfo  string `json:"userInfo"`
		MAC       string `json:"mac"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(plain, &token); err != nil {
		t.Fatalf("unmarshal decrypted token: %v", err)
	}

	// Verify HMAC-SHA256(userInfo, memberKey)
	mac := hmac.New(sha256.New, []byte("member-secret-initial"))
	_, _ = mac.Write([]byte(token.UserInfo))
	if hex.EncodeToString(mac.Sum(nil)) != token.MAC {
		t.Fatal("HMAC-SHA256 mismatch on userInfoToken")
	}

	// Verify RSA-SHA256(userInfo + "." + mac, sitePubKey)
	sigBytes, _ := base64.StdEncoding.DecodeString(token.Signature)
	digest := sha256.Sum256([]byte(token.UserInfo + "." + token.MAC))
	if err := rsa.VerifyPKCS1v15(sitePubKey, crypto.SHA256, digest[:], sigBytes); err != nil {
		t.Fatalf("RSA-SHA256 signature verification failed: %v", err)
	}

	// Verify userInfo fields
	uiRaw, _ := base64.StdEncoding.DecodeString(token.UserInfo)
	var ui map[string]any
	_ = json.Unmarshal(uiRaw, &ui)
	if ui["email"] != "alice@example.com" || ui["nickname"] != "AliceNick" {
		t.Fatalf("unexpected userInfo payload: %+v", ui)
	}
	if !strings.Contains(grantResp.Headers["Cache-Control"], "no-store") || grantResp.Headers["Referrer-Policy"] != "no-referrer" {
		t.Fatalf("expected no-store and no-referrer on OAuth2 grant redirect, got %v", grantResp.Headers)
	}

	// 4. H-2: Unverified user (on SL3 require_email_verification=true site) and banned user must be denied OAuth2 grant.
	unverResp, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodGet,
		Path:   "/api/union/member/oauth2/grant",
		Query:  map[string]string{"state": "s1"},
		User:   &plugin.Viewer{Authenticated: true, UserID: 3, Email: "unverified@example.com", Role: "user"},
	})
	if unverResp.Status != http.StatusForbidden {
		t.Fatalf("H-2 regression: expected 403 for unverified user on OAuth2 grant, got %d %s", unverResp.Status, string(unverResp.RawBody))
	}
	bannedResp, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodGet,
		Path:   "/api/union/member/oauth2/grant",
		Query:  map[string]string{"state": "s2"},
		User:   &plugin.Viewer{Authenticated: true, UserID: 4, Email: "suspended@example.com", Role: "user"},
	})
	if bannedResp.Status != http.StatusForbidden {
		t.Fatalf("H-2 regression: expected 403 for banned user on OAuth2 grant, got %d %s", bannedResp.Status, string(bannedResp.RawBody))
	}
}

func TestMUAUnion_UserIDORAndOutboxRetryAndBlacklistGuard(t *testing.T) {
	ctx := context.Background()
	hub := newMockUnionHub(t, "member-secret-initial")
	_, h, now := setupTestPlugin(t, hub, map[string]any{"enableLocalBlacklist": true})

	alice := &plugin.Viewer{Authenticated: true, UserID: 1, Email: "alice@example.com", Role: "user"}
	admin := &plugin.Viewer{Authenticated: true, UserID: 1, Email: "alice@example.com", Role: "admin"}
	superadmin := &plugin.Viewer{Authenticated: true, UserID: 1, Email: "alice@example.com", Role: "superadmin"}

	// 1. User overview filters out self (101) & already-bound (102) from dupName, leaving only 103
	ovResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodGet, Path: "/user/overview", User: alice})
	if err != nil || ovResp.Status != http.StatusOK {
		t.Fatalf("GET /user/overview failed: status=%v err=%v", ovResp, err)
	}
	var ov struct {
		SecurityLevel int                    `json:"securityLevel"`
		Profiles      []unionProfileOverview `json:"profiles"`
	}
	_ = json.Unmarshal(ovResp.RawBody, &ov)
	if ov.SecurityLevel != 3 || len(ov.Profiles) != 1 || len(ov.Profiles[0].DupName) != 1 {
		t.Fatalf("unexpected overview response: %+v", ov)
	}

	// 2. IDOR protection: Alice can bind her own UUID, but gets 403 trying to bind/unbind/remap Bob's UUID!
	ownBind, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{
		Method: http.MethodPost, Path: "/user/bind", User: alice,
		Body: []byte(`{"uuid":"11111111111111111111111111111111"}`),
	})
	if ownBind.Status != http.StatusOK || !strings.Contains(string(ownBind.RawBody), "BIND-TOKEN-999") {
		t.Fatalf("own bind failed: status=%d body=%s", ownBind.Status, string(ownBind.RawBody))
	}
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/user/bind", `{"uuid":"22222222222222222222222222222222"}`},
		{"/user/bindto", `{"uuid":"22222222222222222222222222222222","token":"abc"}`},
		{"/user/unbind", `{"uuid":"22222222222222222222222222222222"}`},
		{"/user/remapuuid", `{"me":"22222222222222222222222222222222","target":"33333333333333333333333333333333"}`},
	} {
		idorResp, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: tc.path, User: alice, Body: []byte(tc.body)})
		if idorResp.Status != http.StatusForbidden {
			t.Fatalf("IDOR check failed for %s: expected 403, got %d", tc.path, idorResp.Status)
		}
	}

	// 3. Outbound player lifecycle sync + outbox retry via Cron when Hub transiently fails
	hub.mu.Lock()
	hub.failProfileSync = true
	hub.mu.Unlock()
	if err := h.TriggerEvent(ctx, "player.created", playerEventPayload{
		PlayerID: 10, UserID: 1, Name: "Alice", UUID: "11111111111111111111111111111111",
	}); err != nil {
		t.Fatalf("TriggerEvent player.created failed: %v", err)
	}
	items, _ := listAllOutbox(ctx, h.DB(), 10)
	if len(items) != 1 || items[0].Action != "create" {
		t.Fatalf("expected 1 queued outbox item, got %+v", items)
	}

	// Recover Hub and advance clock past next_retry_at, then trigger cron
	hub.mu.Lock()
	hub.failProfileSync = false
	hub.mu.Unlock()
	*now = now.Add(2 * time.Minute)
	if err := h.TriggerCron(ctx, "mua-union-maintenance"); err != nil {
		t.Fatalf("TriggerCron failed: %v", err)
	}
	itemsAfter, _ := listAllOutbox(ctx, h.DB(), 10)
	if len(itemsAfter) != 0 {
		t.Fatalf("expected outbox to drain after cron retry, still has %d items", len(itemsAfter))
	}

	// H-7: Outbox causal coalescing while Hub is unreachable:
	// create -> rename collapses to a single create with updated name;
	// create -> delete drops create and enqueues delete so Hub never resurrects deleted profile.
	hub.mu.Lock()
	hub.failProfileSync = true
	hub.mu.Unlock()
	_ = h.TriggerEvent(ctx, "player.created", playerEventPayload{
		PlayerID: 30, UserID: 1, Name: "OldCoalesce", UUID: "33333333333333333333333333333333",
	})
	_ = h.TriggerEvent(ctx, "player.renamed", playerEventPayload{
		PlayerID: 30, UserID: 1, Name: "NewCoalesce", UUID: "33333333333333333333333333333333",
	})
	coalItems, _ := listAllOutbox(ctx, h.DB(), 10)
	if len(coalItems) != 1 || coalItems[0].Action != "create" || coalItems[0].Name != "NewCoalesce" {
		t.Fatalf("H-7 regression: expected create+rename coalesced into 1 create(NewCoalesce), got %+v", coalItems)
	}
	_ = h.TriggerEvent(ctx, "player.deleted", playerEventPayload{
		PlayerID: 30, UserID: 1, Name: "NewCoalesce", UUID: "33333333333333333333333333333333",
	})
	coalItems2, _ := listAllOutbox(ctx, h.DB(), 10)
	if len(coalItems2) != 1 || coalItems2[0].Action != "delete" {
		t.Fatalf("H-7 regression: expected create+delete coalesced into 1 delete, got %+v", coalItems2)
	}
	hub.mu.Lock()
	hub.failProfileSync = false
	hub.mu.Unlock()
	*now = now.Add(2 * time.Minute)
	_ = h.TriggerCron(ctx, "mua-union-maintenance")

	// 4. Blacklist Guard blocks banned user (ID 2) and permits clean user (ID 1)
	if err := h.TriggerGuard(ctx, "yggdrasil.join", yggdrasilJoinPayload{UserID: 1, PlayerID: 10, UUID: "11111111111111111111111111111111"}); err != nil {
		t.Fatalf("clean user unexpectedly rejected: %v", err)
	}
	banErr := h.TriggerGuard(ctx, "yggdrasil.join", yggdrasilJoinPayload{UserID: 2, PlayerID: 20, UUID: "22222222222222222222222222222222"})
	if _, isRej := plugin.IsRejection(banErr); !isRej {
		t.Fatalf("expected blacklisted user join to be rejected, got err=%v", banErr)
	}

	// 5. Admin status never leaks secret memberKey or private keys; blacklist CRUD proxies cleanly
	statusResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodGet, Path: "/admin/status", User: admin})
	if err != nil || statusResp.Status != http.StatusOK {
		t.Fatalf("GET /admin/status failed: status=%v err=%v", statusResp, err)
	}
	if strings.Contains(string(statusResp.RawBody), "member-secret-initial") || strings.Contains(string(statusResp.RawBody), "PRIVATE KEY") {
		t.Fatal("admin status leaked secret credential or private key!")
	}
	diagResp, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/admin/actions/diagnose", User: admin})
	if diagResp.Status != http.StatusOK || !strings.Contains(string(diagResp.RawBody), `"healthy":true`) {
		t.Fatalf("admin diagnose failed: status=%d body=%s", diagResp.Status, string(diagResp.RawBody))
	}
	blList, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodGet, Path: "/admin/blacklist", User: admin})
	if blList.Status != http.StatusOK || !strings.Contains(string(blList.RawBody), "banned@example.com") {
		t.Fatalf("admin blacklist list failed: status=%d body=%s", blList.Status, string(blList.RawBody))
	}
	// H-3: Regular admin must be denied (403) on high-risk crypto & blacklist mutations; superadmin succeeds.
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/admin/actions/updateprivatekey", `{}`},
		{http.MethodPost, "/admin/actions/regenerate-oauth2-key", `{}`},
		{http.MethodPost, "/admin/blacklist", `{"email":"bad2@example.com","reason":"spam"}`},
		{http.MethodPut, "/admin/blacklist/7/invalidate", `{}`},
		{http.MethodDelete, "/admin/blacklist/7", ``},
	} {
		denied, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: tc.method, Path: tc.path, User: admin, Body: []byte(tc.body)})
		if denied.Status != http.StatusForbidden {
			t.Fatalf("H-3 regression: expected regular admin 403 on %s %s, got %d", tc.method, tc.path, denied.Status)
		}
	}
	r1, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPost, Path: "/admin/blacklist", User: superadmin, Body: []byte(`{"email":"bad2@example.com","reason":"spam"}`)})
	r2, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodPut, Path: "/admin/blacklist/7/invalidate", User: superadmin})
	r3, _ := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodDelete, Path: "/admin/blacklist/7", User: superadmin})
	if r1.Status != http.StatusOK || r2.Status != http.StatusOK || r3.Status != http.StatusOK {
		t.Fatalf("superadmin blacklist mutations failed: %d %d %d", r1.Status, r2.Status, r3.Status)
	}
	hub.mu.Lock()
	blCalls := len(hub.blacklistCalls)
	hub.mu.Unlock()
	if blCalls != 3 {
		t.Fatalf("expected 3 blacklist mutating calls proxied, got %d", blCalls)
	}
}

func TestSSRFIPGuard(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "172.16.5.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "0.0.0.1",
		"192.0.0.1", "192.0.2.1", "192.88.99.1", "198.18.0.245", "240.0.0.1",
		"::1", "fe80::1", "fc00::1", "100::1", "64:ff9b::7f00:1", "2001:db8::1", "2002:7f00:1::1",
	}
	for _, s := range blocked {
		if isPublicGlobalIP(net.ParseIP(s)) {
			t.Fatalf("expected non-public IP %s to be blocked", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if !isPublicGlobalIP(net.ParseIP(s)) {
			t.Fatalf("expected public IP %s to be allowed", s)
		}
	}
}

func TestMUAUnion_ImportConfigFromBSImporter(t *testing.T) {
	ctx := context.Background()
	hub := newMockUnionHub(t, "imported-union-secret")
	// Start with an unconfigured fresh mua-union plugin (blank member key, default URL, oauth2 disabled).
	_, h, _ := setupTestPlugin(t, hub, map[string]any{
		"unionApiRoot":                defaultUnionAPIRoot,
		"unionMemberKey":              "",
		"enableUnionOAuth2":           false,
		"enableLocalBlacklist":        false,
		"invitationCodesForUnion":     false,
		"allowRemoteKeySync":          false,
		"allowRemoteUUIDRemap":        false,
		"allowRemoteBackendKeyRotate": false,
	})

	// 1. Reject callers other than bs-importer on both describe and import.
	if _, err := h.CallService(ctx, "bsMigrateDescribe", "evil-plugin"); err == nil {
		t.Fatal("expected bsMigrateDescribe to reject non-bs-importer caller")
	}
	if _, err := h.CallService(ctx, "bsMigrateImport", "evil-plugin", genericMigrationPayload{Options: map[string]string{"union_member_key": "hijack"}}); err == nil {
		t.Fatal("expected bsMigrateImport to reject non-bs-importer caller")
	}

	// 2. bsMigrateDescribe returns localized label and declared optionKeys.
	descRaw, err := h.CallService(ctx, "bsMigrateDescribe", "bs-importer")
	if err != nil || !strings.Contains(string(descRaw), "union_member_key") || !strings.Contains(string(descRaw), "union_oauth2_sig_private_key") {
		t.Fatalf("bsMigrateDescribe failed: err=%v raw=%s", err, string(descRaw))
	}

	// 3. Import legacy MUA Union options + OAuth2 RSA keypair + server list from bs-importer.
	legacyPriv, legacyPub, err := generateRSA4096KeyPairPEM()
	if err != nil {
		t.Fatalf("generate legacy oauth2 keypair: %v", err)
	}
	payload := genericMigrationPayload{
		Options: map[string]string{
			"union_api_root":                     hub.srv.URL + "/api/union",
			"union_member_key":                   "imported-union-secret",
			"union_server_list":                  `[{"code":"PKU","bs_root":"https://skin.pku.example.edu.cn"},{"code":"SJTU","bs_root":"https://skin.sjtu.example.edu.cn"}]`,
			"union_server_list_version":          "42",
			"union_private_key_version":          "9",
			"union_enable_oauth2":                "true",
			"union_oauth2_sig_private_key":       legacyPriv,
			"union_oauth2_sig_public_key":        legacyPub,
			"union_use_blacklist_locally":        "true",
			"invitation_codes_for_union_enabled": "true",
		},
	}

	raw, err := h.CallService(ctx, "bsMigrateImport", "bs-importer", payload)
	if err != nil {
		t.Fatalf("bsMigrateImport failed: %v", err)
	}
	var res struct {
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode bsMigrateImport result: %v", err)
	}
	if res.Imported != 9 || res.Skipped != 0 || len(res.Errors) != 0 {
		t.Fatalf("unexpected first import result: %+v", res)
	}

	// Verify host plugin settings persisted & high-risk remote callbacks remained false.
	if h.Host().Settings["unionMemberKey"] != "imported-union-secret" ||
		h.Host().Settings["unionApiRoot"] != hub.srv.URL+"/api/union" ||
		h.Host().Settings["enableUnionOAuth2"] != true ||
		h.Host().Settings["enableLocalBlacklist"] != true ||
		h.Host().Settings["invitationCodesForUnion"] != true {
		t.Fatalf("host settings were not patched as expected: %+v", h.Host().Settings)
	}
	if h.Host().Settings["allowRemoteKeySync"] == true ||
		h.Host().Settings["allowRemoteUUIDRemap"] == true ||
		h.Host().Settings["allowRemoteBackendKeyRotate"] == true {
		t.Fatal("bsMigrateImport must never auto-enable high-risk remote webhook toggles")
	}

	// Verify Yggdrasil skinDomains immediately includes imported member sites.
	metaExt, err := h.TriggerYggdrasilMeta(ctx)
	if err != nil {
		t.Fatalf("TriggerYggdrasilMeta failed: %v", err)
	}
	joined := strings.Join(metaExt.SkinDomains, ",")
	if !strings.Contains(joined, "skin.pku.example.edu.cn") || !strings.Contains(joined, "skin.sjtu.example.edu.cn") {
		t.Fatalf("expected imported member domains in Yggdrasil meta, got %v", metaExt.SkinDomains)
	}

	// Verify /api/union/member reflects imported versions & features.
	helloResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodGet, Path: "/api/union/member"})
	if err != nil || helloResp.Status != http.StatusOK {
		t.Fatalf("GET /api/union/member failed: %v %v", helloResp, err)
	}
	if !strings.Contains(string(helloResp.RawBody), `"serverListVersion":42`) ||
		!strings.Contains(string(helloResp.RawBody), `"privateKeyVersion":9`) ||
		!strings.Contains(string(helloResp.RawBody), `"unionOAuth2"`) {
		t.Fatalf("unexpected /api/union/member body after import: %s", string(helloResp.RawBody))
	}

	// Verify /api/union/member/oauth2 serves the exact imported public key (zero key loss!).
	oauthResp, err := h.DoHTTP(ctx, &plugin.HTTPRequest{Method: http.MethodGet, Path: "/api/union/member/oauth2"})
	if err != nil || oauthResp.Status != http.StatusOK {
		t.Fatalf("GET /api/union/member/oauth2 failed: %v %v", oauthResp, err)
	}
	var pubOut struct {
		SignaturePublicKey string `json:"signaturePublicKey"`
	}
	_ = json.Unmarshal(oauthResp.RawBody, &pubOut)
	if publicKeySHA256Fingerprint(pubOut.SignaturePublicKey) != publicKeySHA256Fingerprint(legacyPub) {
		t.Fatal("imported Union OAuth2 public key fingerprint does not match legacy key")
	}

	// 4. Re-running bsMigrateImport is 100% idempotent (0 imported, 9 skipped, 0 errors).
	raw2, err := h.CallService(ctx, "bsMigrateImport", "bs-importer", payload)
	if err != nil {
		t.Fatalf("second bsMigrateImport failed: %v", err)
	}
	var res2 struct {
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	_ = json.Unmarshal(raw2, &res2)
	if res2.Imported != 0 || res2.Skipped != 9 || len(res2.Errors) != 0 {
		t.Fatalf("expected idempotent second run (0 imported, 9 skipped), got %+v", res2)
	}

	// 5. M-6: dryRun=true writes nothing, and false flags do not disable already-enabled features.
	dryResRaw, err := h.CallService(ctx, "bsMigrateImport", "bs-importer", genericMigrationPayload{
		DryRun: true,
		Options: map[string]string{
			"union_api_root":      "https://evil.example.com/api/union",
			"union_enable_oauth2": "false",
		},
	})
	if err != nil {
		t.Fatalf("dryRun bsMigrateImport failed: %v", err)
	}
	if strings.Contains(string(dryResRaw), "error") && h.Host().Settings["unionApiRoot"] == "https://evil.example.com/api/union" {
		t.Fatal("M-6 regression: dryRun mutated host settings")
	}
	if h.Host().Settings["enableUnionOAuth2"] != true {
		t.Fatal("M-6 regression: false option flipped already-enabled enableUnionOAuth2 to false")
	}
	_, _ = h.CallService(ctx, "bsMigrateImport", "bs-importer", genericMigrationPayload{
		DryRun:  false,
		Options: map[string]string{"union_enable_oauth2": "false"},
	})
	if h.Host().Settings["enableUnionOAuth2"] != true {
		t.Fatal("M-6 regression: live import with union_enable_oauth2=false disabled pre-enabled feature")
	}
}
