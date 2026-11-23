package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	plugin "github.com/Entropy-Increase-Team/skinloom/sdk/go"
)

var unionWebhookHeaders = []string{
	"X-Message-Signature",
	"X-Message-Timestamp",
	"X-Message-Nonce",
}

func (p *MUAUnionPlugin) registerPublicUnionRoutes(ctx *plugin.Context) {
	// 1. Public member capability discovery (CORS: *).
	ctx.HTTP.Get("/api/union/member", p.handleMemberHello,
		plugin.WithAuth(plugin.AuthPublic),
		plugin.WithOpenCORS(),
	)

	// 2. RSA-SHA256 verified webhooks from MUA Union Hub.
	webhookOpts := []plugin.RouteOption{
		plugin.WithAuth(plugin.AuthPublic),
		plugin.WithWebhookNoCSRF(),
		plugin.WithForwardHeaders(unionWebhookHeaders...),
	}

	ctx.HTTP.Post("/api/union/member/updatelist", p.withUnionHostVerify(p.handleWebhookUpdateList), webhookOpts...)
	ctx.HTTP.Post("/api/union/member/updateprivatekey", p.withUnionHostVerify(p.handleWebhookUpdatePrivateKey), webhookOpts...)
	ctx.HTTP.Post("/api/union/member/updatebackendkey", p.withUnionHostVerify(p.handleWebhookUpdateBackendKey), webhookOpts...)
	ctx.HTTP.Post("/api/union/member/sync", p.withUnionHostVerify(p.handleWebhookSync), webhookOpts...)
	ctx.HTTP.Post("/api/union/member/remapuuid", p.withUnionHostVerify(p.handleWebhookRemapUUID), webhookOpts...)
	ctx.HTTP.Get("/api/union/member/queryemail", p.withUnionHostVerify(p.handleWebhookQueryEmail), webhookOpts...)
	ctx.HTTP.Post("/api/union/member/diagnose", p.withUnionHostVerify(p.handleWebhookDiagnose), webhookOpts...)
	ctx.HTTP.Post("/api/union/member/updateplugin", p.withUnionHostVerify(p.handleWebhookUpdatePluginRefused), webhookOpts...)
}

func (p *MUAUnionPlugin) withUnionHostVerify(next plugin.HTTPHandler) plugin.HTTPHandler {
	return func(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
		if err := p.verifyUnionHostRequest(reqCtx, req); err != nil {
			p.ctx.Log.Warn(fmt.Sprintf("UnionHostVerify rejected %s %s: %v", req.Method, req.Path, err))
			return plugin.JSON(http.StatusForbidden, map[string]any{
				"error":        "ForbiddenOperationException",
				"errorMessage": "Union host verification failure.",
			}), nil
		}
		return next(reqCtx, req)
	}
}

func (p *MUAUnionPlugin) verifyUnionHostRequest(ctx context.Context, req *plugin.HTTPRequest) error {
	sig := strings.TrimSpace(req.Headers["x-message-signature"])
	tsRaw := strings.TrimSpace(req.Headers["x-message-timestamp"])
	nonce := strings.TrimSpace(req.Headers["x-message-nonce"])
	if sig == "" || tsRaw == "" || nonce == "" {
		return errors.New("missing X-Message-Signature, X-Message-Timestamp, or X-Message-Nonce")
	}
	if len(nonce) > 180 {
		return errors.New("nonce exceeds maximum length")
	}

	tsFloat, err := strconv.ParseFloat(tsRaw, 64)
	if err != nil || math.IsNaN(tsFloat) || math.IsInf(tsFloat, 0) || tsFloat < 0 || tsFloat > 4e12 {
		return errors.New("invalid timestamp format")
	}
	now := p.nowFunc().Unix()
	tsSec := int64(tsFloat)
	// Exact MUA Union timestamp window: [now - 10, now + 30].
	if tsSec < now-10 || tsSec > now+30 {
		return fmt.Errorf("timestamp %d outside allowed window [%d, %d]", tsSec, now-10, now+30)
	}

	// Verify RSA-SHA256 signature with cached Hub public key; on failure, refresh Hub metadata once and retry.
	_, pubKey, err := p.fetchHubMeta(ctx, false)
	if err != nil {
		return fmt.Errorf("load hub public key: %w", err)
	}
	if err := verifyUnionHostSignature(req.Body, tsRaw, nonce, sig, pubKey); err != nil {
		_, freshPub, refreshErr := p.fetchHubMeta(ctx, true)
		if refreshErr != nil || verifyUnionHostSignature(req.Body, tsRaw, nonce, sig, freshPub) != nil {
			return errors.New("invalid RSA-SHA256 signature")
		}
	}

	// Replay protection: cache nonce for 60 seconds in isolated SQL table.
	accepted, err := recordNonceIfNew(ctx, p.ctx.DB(), nonce, now, now+60)
	if err != nil {
		return fmt.Errorf("record nonce: %w", err)
	}
	if !accepted {
		return errors.New("replayed nonce")
	}
	return nil
}

func (p *MUAUnionPlugin) handleMemberHello(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	features := []string{"unionBlacklist"}
	if pubSite, err := p.ctx.Site.Public(reqCtx); err == nil {
		if reqEmail, ok := pubSite["auth.require_email_verification"].(bool); ok && reqEmail {
			features = append(features, "emailVerification")
		}
	}
	if p.ctx.Settings.GetBool("invitationCodesForUnion") {
		features = append(features, "invitationCodesForUnion")
	}
	if p.ctx.Settings.GetBool("enableUnionOAuth2") {
		features = append(features, "unionOAuth2")
	}

	serverVer, _ := p.getInt64KV(reqCtx, kvServerListVersion)
	keyVer, _ := p.getInt64KV(reqCtx, kvPrivateKeyVersion)

	return plugin.JSON(http.StatusOK, map[string]any{
		"yggdrasilApiVersion": pluginWireVersion,
		"serverListVersion":   serverVer,
		"privateKeyVersion":   keyVer,
		"enabledFeatures":     features,
	}), nil
}

func (p *MUAUnionPlugin) handleWebhookUpdateList(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	list, err := p.syncServerList(reqCtx)
	if err != nil {
		p.ctx.Log.Error(fmt.Sprintf("Union updatelist failed: %v", err))
		return plugin.JSON(http.StatusBadGateway, map[string]any{"error": err.Error()}), nil
	}
	return plugin.JSON(http.StatusOK, map[string]any{
		"ok":      true,
		"version": list.Version,
		"count":   len(list.Servers),
	}), nil
}

func (p *MUAUnionPlugin) handleWebhookUpdatePrivateKey(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	if !p.ctx.Settings.GetBool("allowRemoteKeySync") {
		p.ctx.Log.Warn("Refused /api/union/member/updateprivatekey: allowRemoteKeySync is disabled")
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Remote Yggdrasil signing key sync is disabled in mua-union plugin settings.",
		}), nil
	}
	ver, err := p.syncPrivateKey(reqCtx, "webhook", nil)
	if err != nil {
		p.ctx.Log.Error(fmt.Sprintf("Union updateprivatekey failed: %v", err))
		return plugin.JSON(http.StatusBadGateway, map[string]any{"error": err.Error()}), nil
	}
	return plugin.JSON(http.StatusOK, map[string]any{
		"ok":                true,
		"privateKeyVersion": ver,
	}), nil
}

func (p *MUAUnionPlugin) handleWebhookUpdateBackendKey(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	if !p.ctx.Settings.GetBool("allowRemoteBackendKeyRotate") {
		p.ctx.Log.Warn("Refused /api/union/member/updatebackendkey: allowRemoteBackendKeyRotate is disabled")
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Remote member key rotation is disabled in mua-union plugin settings.",
		}), nil
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := req.DecodeJSON(&body); err != nil || strings.TrimSpace(body.Key) == "" {
		return plugin.JSON(http.StatusBadRequest, map[string]any{
			"error": "missing key field",
		}), nil
	}
	trimmed := strings.TrimSpace(body.Key)
	// Persist directly to plugin settings via Settings.Patch so Admin UI, HA pods,
	// and future admin key-clears stay 100% authoritative; clean up legacy KV keys.
	if _, err := p.ctx.Settings.Patch(reqCtx, map[string]any{"unionMemberKey": trimmed}); err != nil {
		p.ctx.Log.Error(fmt.Sprintf("Persist rotated unionMemberKey via Settings.Patch failed: %v", err))
		return plugin.JSON(http.StatusInternalServerError, map[string]any{"error": "failed to store rotated key"}), nil
	}
	_ = deleteKV(reqCtx, p.ctx.DB(), kvRuntimeMemberKey)
	_ = setKV(reqCtx, p.ctx.DB(), kvLastSettingKey, trimmed)
	_ = p.ctx.Audit.Record(reqCtx, "member_key.rotated", "plugin", "mua-union", map[string]any{
		"source": "webhook",
	})
	p.ctx.Log.Info("Rotated MUA Union member key via verified webhook")
	return plugin.JSON(http.StatusOK, map[string]any{"ok": true}), nil
}

func (p *MUAUnionPlugin) handleWebhookSync(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	count, err := p.triggerFullPlayerSync(reqCtx)
	if err != nil {
		p.ctx.Log.Error(fmt.Sprintf("Union full sync failed: %v", err))
		return plugin.JSON(http.StatusBadGateway, map[string]any{"error": err.Error()}), nil
	}
	return plugin.JSON(http.StatusOK, map[string]any{
		"ok":    true,
		"count": count,
	}), nil
}

func (p *MUAUnionPlugin) handleWebhookRemapUUID(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	if !p.ctx.Settings.GetBool("allowRemoteUUIDRemap") {
		p.ctx.Log.Warn("Refused /api/union/member/remapuuid: allowRemoteUUIDRemap is disabled")
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Remote player UUID remapping is disabled in mua-union plugin settings.",
		}), nil
	}
	var body struct {
		RemappedUUID map[string]string `json:"remapped_uuid"`
	}
	if err := req.DecodeJSON(&body); err != nil {
		return plugin.JSON(http.StatusBadRequest, map[string]any{"error": "invalid JSON payload"}), nil
	}
	if len(body.RemappedUUID) > 500 {
		return plugin.JSON(http.StatusBadRequest, map[string]any{"error": "batch exceeds maximum of 500 UUID remaps"}), nil
	}
	// Order pairs deterministically and process chain tails (entries whose target is
	// not another entry's source) first so chained remaps (A->B, B->C) do not collide.
	cleanPairs := make(map[string]string, len(body.RemappedUUID))
	for oldUUID, newUUID := range body.RemappedUUID {
		oldClean := strings.TrimSpace(oldUUID)
		newClean := strings.TrimSpace(newUUID)
		if oldClean != "" && newClean != "" {
			cleanPairs[oldClean] = newClean
		}
	}
	orderedKeys := make([]string, 0, len(cleanPairs))
	for k := range cleanPairs {
		orderedKeys = append(orderedKeys, k)
	}
	sort.Strings(orderedKeys)
	sortedOrder := make([]string, 0, len(orderedKeys))
	remaining := make(map[string]bool, len(orderedKeys))
	for _, k := range orderedKeys {
		remaining[k] = true
	}
	for len(remaining) > 0 {
		progressed := false
		for _, k := range orderedKeys {
			if !remaining[k] {
				continue
			}
			if !remaining[cleanPairs[k]] {
				sortedOrder = append(sortedOrder, k)
				delete(remaining, k)
				progressed = true
			}
		}
		if !progressed {
			for _, k := range orderedKeys {
				if remaining[k] {
					sortedOrder = append(sortedOrder, k)
					delete(remaining, k)
				}
			}
		}
	}

	remapped := 0
	var failures []map[string]string
	for _, oldClean := range sortedOrder {
		newClean := cleanPairs[oldClean]
		if _, err := p.ctx.Players.RemapUUID(reqCtx, oldClean, newClean); err != nil {
			p.ctx.Log.Warn(fmt.Sprintf("RemapUUID %s -> %s failed: %v", oldClean, newClean, err))
			failures = append(failures, map[string]string{"from": oldClean, "to": newClean, "error": err.Error()})
			continue
		}
		remapped++
	}
	_ = p.ctx.Audit.Record(reqCtx, "players.uuid_remapped", "plugin", "mua-union", map[string]any{
		"source":   "webhook",
		"remapped": remapped,
		"failed":   len(failures),
	})
	return plugin.JSON(http.StatusOK, map[string]any{
		"ok":       len(failures) == 0,
		"remapped": remapped,
		"failures": failures,
	}), nil
}

func (p *MUAUnionPlugin) handleWebhookQueryEmail(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	username := strings.TrimSpace(req.Query["username"])
	if username == "" {
		return plugin.NoContent(), nil
	}
	pl, err := p.ctx.Players.FindByName(reqCtx, username)
	if err != nil || pl == nil {
		return plugin.NoContent(), nil
	}
	u, err := p.ctx.Users.Get(reqCtx, pl.UserID)
	if err != nil || u == nil {
		return plugin.NoContent(), nil
	}
	return plugin.JSON(http.StatusOK, map[string]string{
		"email": u.Email,
	}), nil
}

func (p *MUAUnionPlugin) handleWebhookDiagnose(_ context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	var body struct {
		Nonce string `json:"nonce"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	return plugin.JSON(http.StatusOK, map[string]any{
		"nonce":     body.Nonce,
		"timestamp": float64(p.nowFunc().UnixMicro()) / 1e6,
	}), nil
}

func (p *MUAUnionPlugin) handleWebhookUpdatePluginRefused(_ context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	p.ctx.Log.Warn("Refused /api/union/member/updateplugin: SkinLoom plugins are updated exclusively via the Ed25519-signed plugin marketplace")
	return plugin.JSON(http.StatusNotImplemented, map[string]any{
		"error":        "NotImplemented",
		"errorMessage": "Remote ZIP self-update is intentionally disabled in SkinLoom mua-union. Upgrade plugins via the SkinLoom Ed25519-signed plugin marketplace.",
	}), nil
}
