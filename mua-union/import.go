package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	plugin "github.com/Entropy-Increase-Team/skinloom/sdk/go"
)

// muaUnionLegacyOptionKeys lists the Blessing Skin options table keys owned by
// the legacy MUA edition of yggdrasil-connect. Declared via bsMigrateDescribe so
// bs-importer fetches ONLY these keys (least privilege) without hardcoding MUA.
var muaUnionLegacyOptionKeys = []string{
	"union_api_root",
	"union_member_key",
	"union_server_list",
	"union_server_list_version",
	"union_private_key_version",
	"union_enable_oauth2",
	"union_oauth2_sig_private_key",
	"union_oauth2_sig_public_key",
	"union_use_blacklist_locally",
	"invitation_codes_for_union_enabled",
}

// genericMigrationPayload matches bs-importer's universal bsMigrateImport wire contract.
type genericMigrationPayload struct {
	Options map[string]string           `json:"options"`
	Tables  map[string][]map[string]any `json:"tables,omitempty"`
	DryRun  bool                        `json:"dryRun"`
}

func (p *MUAUnionPlugin) registerImportService(ctx *plugin.Context) {
	ctx.Services.Expose("bsMigrateDescribe", p.handleBSMigrateDescribe)
	ctx.Services.Expose("bsMigrateImport", p.handleBSMigrateImport)
}

func (p *MUAUnionPlugin) handleBSMigrateDescribe(_ context.Context, callerPluginID string, _ []json.RawMessage) (any, error) {
	if callerPluginID != "bs-importer" {
		return nil, fmt.Errorf("mua-union.bsMigrateDescribe may only be invoked by bs-importer (got %q)", callerPluginID)
	}
	return map[string]any{
		"label": map[string]string{
			"zh-CN": "MUA Union 配置与密钥（options: union_*）",
			"en":    "MUA Union Config & Keys (options: union_*)",
		},
		"description": map[string]string{
			"zh-CN": "迁移旧站 MUA Union 成员站密钥、API 地址、成员站列表、版本号、功能开关与 4096 位 Union OAuth2 RSA 签名密钥对（不自动开启高危远端回调）",
			"en":    "Migrate legacy MUA Union member key, API root, server list, version markers, feature flags, and Union OAuth2 RSA signing keypair (high-risk remote webhooks remain off)",
		},
		"optionKeys": muaUnionLegacyOptionKeys,
	}, nil
}

func (p *MUAUnionPlugin) handleBSMigrateImport(ctx context.Context, callerPluginID string, args []json.RawMessage) (any, error) {
	if callerPluginID != "bs-importer" {
		return nil, fmt.Errorf("mua-union.bsMigrateImport may only be invoked by bs-importer (got %q)", callerPluginID)
	}
	if len(args) == 0 || len(args[0]) == 0 {
		return map[string]any{"imported": 0, "skipped": 0, "errors": []string{"missing migration payload"}}, nil
	}

	var payload genericMigrationPayload
	if err := json.Unmarshal(args[0], &payload); err != nil {
		return map[string]any{"imported": 0, "skipped": 0, "errors": []string{"invalid bsMigrateImport payload: " + err.Error()}}, nil
	}
	opts := payload.Options
	if len(opts) == 0 {
		return map[string]any{"imported": 0, "skipped": 0, "errors": []string{}}, nil
	}

	imported := 0
	skipped := 0
	errs := []string{}
	settingsPatch := map[string]any{}

	// 1. union_api_root -> unionApiRoot
	if rawRoot := strings.TrimRight(strings.TrimSpace(opts["union_api_root"]), "/"); rawRoot != "" {
		u, err := url.Parse(rawRoot)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "https" && !(p.allowInsecureTest && u.Scheme == "http")) {
			errs = append(errs, "union_api_root must be a valid https:// URL without credentials or query")
		} else {
			cur := strings.TrimRight(strings.TrimSpace(p.ctx.Settings.GetString("unionApiRoot")), "/")
			if cur == rawRoot || (cur != "" && cur != defaultUnionAPIRoot) {
				skipped++
			} else {
				settingsPatch["unionApiRoot"] = rawRoot
				imported++
			}
		}
	}

	// 2. union_member_key -> unionMemberKey (never overwrite an already-configured key; never logged)
	importedKey := ""
	if key := strings.TrimSpace(opts["union_member_key"]); key != "" {
		if p.effectiveMemberKey(ctx) != "" {
			skipped++
		} else {
			settingsPatch["unionMemberKey"] = key
			importedKey = key
			imported++
		}
	}

	// 3. Non-elevated feature flags (high-risk remote webhook toggles remain disabled by default;
	// never silently turn OFF a flag an admin already enabled on SkinLoom).
	importBoolOpt := func(optKey, settingKey string) {
		raw, ok := opts[optKey]
		if !ok || strings.TrimSpace(raw) == "" {
			return
		}
		val := parseLegacyPHPBool(raw)
		cur := p.ctx.Settings.GetBool(settingKey)
		if cur == val || (cur && !val) {
			skipped++
			return
		}
		settingsPatch[settingKey] = val
		imported++
	}
	importBoolOpt("union_enable_oauth2", "enableUnionOAuth2")
	importBoolOpt("union_use_blacklist_locally", "enableLocalBlacklist")
	importBoolOpt("invitation_codes_for_union_enabled", "invitationCodesForUnion")

	// 4. Member server list + version (persist BEFORE Settings.Patch so LiveRefresh /
	// concurrent requests see complete KV state immediately).
	if slRaw := strings.TrimSpace(opts["union_server_list"]); slRaw != "" && slRaw != "{}" && slRaw != "[]" && slRaw != "null" {
		if !json.Valid([]byte(slRaw)) {
			errs = append(errs, "union_server_list is not valid JSON")
		} else {
			servers := parseServerEntries([]byte(slRaw))
			if len(servers) > 0 {
				existing := p.loadCachedServers(ctx)
				if len(existing) > 0 {
					skipped++
				} else if payload.DryRun {
					imported++
				} else {
					raw, mErr := json.Marshal(servers)
					if mErr != nil {
						errs = append(errs, "marshal server list: "+mErr.Error())
					} else if err := setKV(ctx, p.ctx.DB(), kvServerList, string(raw)); err != nil {
						errs = append(errs, "store server list: "+err.Error())
					} else {
						p.setCachedServers(servers)
						imported++
					}
				}
			}
		}
	}
	if verRaw := strings.TrimSpace(opts["union_server_list_version"]); verRaw != "" {
		v, err := strconv.ParseInt(verRaw, 10, 64)
		if err != nil || v < 0 {
			errs = append(errs, fmt.Sprintf("union_server_list_version %q is not a valid integer", verRaw))
		} else if v > 0 {
			curVer, _ := p.getInt64KV(ctx, kvServerListVersion)
			if curVer > 0 {
				skipped++
			} else if payload.DryRun {
				imported++
			} else if err := setKV(ctx, p.ctx.DB(), kvServerListVersion, strconv.FormatInt(v, 10)); err != nil {
				errs = append(errs, "store server list version: "+err.Error())
			} else {
				imported++
			}
		}
	}

	// 5. Private key version marker (the Yggdrasil signing key itself is migrated by SkinLoom Core importer)
	if verRaw := strings.TrimSpace(opts["union_private_key_version"]); verRaw != "" {
		v, err := strconv.ParseInt(verRaw, 10, 64)
		if err != nil || v < 0 {
			errs = append(errs, fmt.Sprintf("union_private_key_version %q is not a valid integer", verRaw))
		} else if v > 0 {
			curVer, _ := p.getInt64KV(ctx, kvPrivateKeyVersion)
			if curVer > 0 {
				skipped++
			} else if payload.DryRun {
				imported++
			} else if err := setKV(ctx, p.ctx.DB(), kvPrivateKeyVersion, strconv.FormatInt(v, 10)); err != nil {
				errs = append(errs, "store private key version: "+err.Error())
			} else {
				imported++
			}
		}
	}

	// 6. Union OAuth2 RSA signing keypair — persisted atomically BEFORE enabling
	// enableUnionOAuth2 via Settings.Patch so concurrent /admin/status or /oauth2
	// requests never race to auto-generate a fresh keypair first.
	if privRaw := strings.TrimSpace(opts["union_oauth2_sig_private_key"]); privRaw != "" {
		privPEM, pubPEM, err := canonicalizeOAuth2KeyPair(privRaw, opts["union_oauth2_sig_public_key"])
		if err != nil {
			errs = append(errs, "union_oauth2_sig_private_key: "+err.Error())
		} else {
			_, hasExisting, _ := getKV(ctx, p.ctx.DB(), kvOAuth2PrivKey)
			if hasExisting {
				skipped++
			} else if payload.DryRun {
				imported++
			} else {
				privObj, _ := parseRSAPrivateKeyPEM(privPEM)
				inserted, iErr := insertKVIfAbsent(ctx, p.ctx.DB(), kvOAuth2PrivKey, privPEM, p.nowFunc().Unix())
				if iErr != nil {
					errs = append(errs, "store oauth2 private key: "+iErr.Error())
				} else if !inserted {
					skipped++
				} else {
					_ = deleteKV(ctx, p.ctx.DB(), kvOAuth2PubKey)
					p.oauth2KeyMu.Lock()
					p.oauth2Priv = privObj
					p.oauth2PubPEM = pubPEM
					p.oauth2KeyExp = p.nowFunc().Add(haCacheTTL)
					p.oauth2KeyMu.Unlock()
					imported++
				}
			}
		}
	}

	// 7. Persist validated plugin settings into SkinLoom host DB (after KV state is ready).
	if len(settingsPatch) > 0 && !payload.DryRun {
		if _, err := p.ctx.Settings.Patch(ctx, settingsPatch); err != nil {
			errs = append(errs, fmt.Sprintf("persist plugin settings: %v", err))
			imported -= len(settingsPatch)
			if imported < 0 {
				imported = 0
			}
		} else {
			p.metaMu.Lock()
			p.cachedMeta = nil
			p.cachedHostPub = nil
			p.cachedMetaExp = time.Time{}
			p.oauth2BackendPub = nil
			p.oauth2BackendPubExp = time.Time{}
			p.secLevelExp = time.Time{}
			p.metaMu.Unlock()
			if importedKey != "" {
				_ = deleteKV(ctx, p.ctx.DB(), kvRuntimeMemberKey)
				_ = setKV(ctx, p.ctx.DB(), kvLastSettingKey, importedKey)
			}
		}
	}

	if !payload.DryRun {
		_ = p.ctx.Audit.Record(ctx, "config.imported", "plugin", "mua-union", map[string]any{
			"caller":   callerPluginID,
			"imported": imported,
			"skipped":  skipped,
			"errors":   len(errs),
		})
		p.ctx.Log.Info(fmt.Sprintf("Imported MUA Union config from %s: imported=%d skipped=%d errors=%d", callerPluginID, imported, skipped, len(errs)))
	}

	return map[string]any{
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	}, nil
}

func parseLegacyPHPBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "(false)", "null", "(null)", "no", "off":
		return false
	default:
		return true
	}
}

func canonicalizeOAuth2KeyPair(privPEMIn, pubPEMIn string) (privPEM string, pubPEM string, err error) {
	priv, err := parseRSAPrivateKeyPEM(privPEMIn)
	if err != nil {
		return "", "", err
	}
	if err := priv.Validate(); err != nil {
		return "", "", fmt.Errorf("invalid RSA private key: %w", err)
	}
	if priv.N.BitLen() < 2048 {
		return "", "", fmt.Errorf("RSA key too weak (%d bits, minimum 2048)", priv.N.BitLen())
	}
	if trimmedPub := strings.TrimSpace(pubPEMIn); trimmedPub != "" {
		pub, pErr := parseRSAPublicKeyPEM(trimmedPub)
		if pErr != nil {
			return "", "", fmt.Errorf("invalid union_oauth2_sig_public_key: %w", pErr)
		}
		if !pub.Equal(&priv.PublicKey) {
			return "", "", errors.New("union_oauth2_sig_public_key does not match private key")
		}
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", "", err
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return privPEM, pubPEM, nil
}
