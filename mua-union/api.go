package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	plugin "github.com/Entropy-Increase-Team/skinloom/sdk/go"
)

func apiErrJSON(status int, msg string) *plugin.HTTPResponse {
	return plugin.JSON(status, map[string]any{
		"error":  msg,
		"detail": msg,
	})
}

func actorAuditMeta(req *plugin.HTTPRequest, extra map[string]any) map[string]any {
	meta := map[string]any{}
	for k, v := range extra {
		meta[k] = v
	}
	if req != nil && req.User != nil {
		if req.User.UserID > 0 {
			meta["actorId"] = req.User.UserID
		}
		if req.User.Email != "" {
			meta["actorEmail"] = req.User.Email
		}
		if req.User.Role != "" {
			meta["actorRole"] = req.User.Role
		}
	}
	return meta
}

func superadminOnly(next plugin.HTTPHandler) plugin.HTTPHandler {
	return func(ctx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
		if req == nil || req.User == nil || !req.User.Authenticated || req.User.Role != "superadmin" {
			return apiErrJSON(http.StatusForbidden, "superadmin privileges required"), nil
		}
		return next(ctx, req)
	}
}

func (p *MUAUnionPlugin) registerPluginAPIRoutes(ctx *plugin.Context) {
	// User-facing MUA Union profile binding routes (/api/plugins/mua-union/user/*).
	ctx.HTTP.Get("/user/overview", p.handleUserOverview, plugin.WithAuth(plugin.AuthUser))
	ctx.HTTP.Post("/user/bind", p.handleUserBind, plugin.WithAuth(plugin.AuthUser))
	ctx.HTTP.Post("/user/bindto", p.handleUserBindTo, plugin.WithAuth(plugin.AuthUser))
	ctx.HTTP.Post("/user/unbind", p.handleUserUnbind, plugin.WithAuth(plugin.AuthUser))
	ctx.HTTP.Post("/user/remapuuid", p.handleUserRemapUUID, plugin.WithAuth(plugin.AuthUser))

	// Admin console & blacklist routes (/api/plugins/mua-union/admin/*).
	// High-risk cryptographic/federation mutations require superadmin (matching SkinLoom native plugin boundary).
	ctx.HTTP.Get("/admin/status", p.handleAdminStatus, plugin.WithAuth(plugin.AuthAdmin))
	ctx.HTTP.Post("/admin/actions/updatelist", superadminOnly(p.handleAdminUpdateList), plugin.WithAuth(plugin.AuthSuperadmin))
	ctx.HTTP.Post("/admin/actions/updateprivatekey", superadminOnly(p.handleAdminUpdatePrivateKey), plugin.WithAuth(plugin.AuthSuperadmin))
	ctx.HTTP.Post("/admin/actions/sync", p.handleAdminSync, plugin.WithAuth(plugin.AuthAdmin))
	ctx.HTTP.Post("/admin/actions/diagnose", p.handleAdminDiagnose, plugin.WithAuth(plugin.AuthAdmin))
	ctx.HTTP.Post("/admin/actions/regenerate-oauth2-key", superadminOnly(p.handleAdminRegenerateOAuth2Key), plugin.WithAuth(plugin.AuthSuperadmin))
	ctx.HTTP.Post("/admin/actions/retry-outbox", p.handleAdminRetryOutbox, plugin.WithAuth(plugin.AuthAdmin))
	ctx.HTTP.Delete("/admin/outbox/:id", p.handleAdminDeleteOutbox, plugin.WithAuth(plugin.AuthAdmin))

	ctx.HTTP.Get("/admin/blacklist", p.handleAdminBlacklistList, plugin.WithAuth(plugin.AuthAdmin))
	ctx.HTTP.Post("/admin/blacklist", superadminOnly(p.handleAdminBlacklistCreate), plugin.WithAuth(plugin.AuthSuperadmin))
	ctx.HTTP.Put("/admin/blacklist/:id/invalidate", superadminOnly(p.handleAdminBlacklistInvalidate), plugin.WithAuth(plugin.AuthSuperadmin))
	ctx.HTTP.Delete("/admin/blacklist/:id", superadminOnly(p.handleAdminBlacklistDelete), plugin.WithAuth(plugin.AuthSuperadmin))
}

type unionProfileOverview struct {
	PlayerID int64            `json:"playerId"`
	Name     string           `json:"name"`
	UUID     string           `json:"uuid"`
	Self     map[string]any   `json:"self,omitempty"`
	DupName  []map[string]any `json:"dupName"`
	Error    string           `json:"error,omitempty"`
}

func (p *MUAUnionPlugin) handleUserOverview(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	if req.User == nil || req.User.UserID <= 0 {
		return apiErrJSON(http.StatusUnauthorized, "authentication required"), nil
	}

	configured := p.effectiveMemberKey(reqCtx) != ""
	servers := p.loadCachedServers(reqCtx)
	intro := strings.TrimSpace(p.ctx.Settings.GetString("unionIntroMarkdown"))

	var hubDesc string
	if meta, _, err := p.fetchHubMeta(reqCtx, false); err == nil && meta != nil {
		if isChineseLocale(req.Locale) && strings.TrimSpace(meta.Meta.Description) != "" {
			hubDesc = meta.Meta.Description
		} else if strings.TrimSpace(meta.Meta.DescriptionEN) != "" {
			hubDesc = meta.Meta.DescriptionEN
		} else {
			hubDesc = meta.Meta.Description
		}
	}
	if intro == "" {
		intro = hubDesc
	}

	var securityLevel *int
	if configured {
		if lvl, err := p.fetchSecurityLevel(reqCtx); err == nil && lvl >= 0 {
			securityLevel = &lvl
		}
	}

	players, err := p.ctx.Players.List(reqCtx, req.User.UserID)
	if err != nil {
		return apiErrJSON(http.StatusInternalServerError, "failed to load players"), nil
	}

	profiles := make([]unionProfileOverview, len(players))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, pl := range players {
		profiles[i] = unionProfileOverview{
			PlayerID: pl.ID,
			Name:     pl.Name,
			UUID:     pl.UUID,
			DupName:  []map[string]any{},
		}
		if !configured {
			continue
		}
		wg.Add(1)
		go func(idx int, item plugin.Player) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			detailRes, dErr := p.doHubRequest(reqCtx, http.MethodGet, "/profile/detail/"+url.PathEscape(item.UUID), nil, nil, true, 6*time.Second)
			if dErr != nil {
				p.ctx.Log.Warn(fmt.Sprintf("Union profile detail failed for %s: %v", item.UUID, dErr))
				profiles[idx].Error = "Union hub temporarily unavailable"
				return
			}
			if detailRes.Status >= 200 && detailRes.Status < 300 {
				var selfObj map[string]any
				if err := json.Unmarshal(detailRes.Body, &selfObj); err == nil {
					profiles[idx].Self = selfObj
				}
			} else {
				profiles[idx].Error = fmt.Sprintf("HTTP %d", detailRes.Status)
			}

			dupRes, dupErr := p.doHubRequest(reqCtx, http.MethodGet, "/profile/unmapped/byname/"+url.PathEscape(item.Name), nil, nil, true, 6*time.Second)
			if dupErr == nil && dupRes.Status >= 200 && dupRes.Status < 300 {
				var allDups []map[string]any
				if err := json.Unmarshal(dupRes.Body, &allDups); err == nil {
					profiles[idx].DupName = filterDuplicateProfiles(allDups, profiles[idx].Self)
				}
			}
		}(i, pl)
	}
	wg.Wait()

	return plugin.JSON(http.StatusOK, map[string]any{
		"configured":    configured,
		"introMarkdown": intro,
		"securityLevel": securityLevel,
		"profiles":      profiles,
		"servers":       servers,
	}), nil
}

// filterDuplicateProfiles mirrors UnionProfileController::render:
// removes self.internal_id and every internal_id already bound in self.bind.
func filterDuplicateProfiles(candidates []map[string]any, selfObj map[string]any) []map[string]any {
	exclude := map[string]struct{}{}
	if selfObj != nil {
		if id, ok := selfObj["internal_id"]; ok && id != nil {
			exclude[fmt.Sprint(id)] = struct{}{}
		}
		if binds, ok := selfObj["bind"].([]any); ok {
			for _, raw := range binds {
				if m, ok := raw.(map[string]any); ok {
					if id, ok := m["internal_id"]; ok && id != nil {
						exclude[fmt.Sprint(id)] = struct{}{}
					}
				}
			}
		}
	}
	out := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		id := fmt.Sprint(c["internal_id"])
		if _, skip := exclude[id]; skip {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (p *MUAUnionPlugin) verifyUserOwnsPlayerUUID(ctx context.Context, userID int64, targetUUID string) (bool, error) {
	norm := normalizeUUIDHex(targetUUID)
	if norm == "" {
		return false, nil
	}
	players, err := p.ctx.Players.List(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, pl := range players {
		if normalizeUUIDHex(pl.UUID) == norm {
			return true, nil
		}
	}
	return false, nil
}

func isValidUUIDHex(norm string) bool {
	if len(norm) != 32 {
		return false
	}
	_, err := hex.DecodeString(norm)
	return err == nil
}

func (p *MUAUnionPlugin) handleUserBind(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	if req.User == nil || req.User.UserID <= 0 {
		return apiErrJSON(http.StatusUnauthorized, "authentication required"), nil
	}
	var body struct {
		UUID string `json:"uuid"`
	}
	if err := req.DecodeJSON(&body); err != nil || strings.TrimSpace(body.UUID) == "" {
		return apiErrJSON(http.StatusBadRequest, "uuid is required"), nil
	}
	owned, err := p.verifyUserOwnsPlayerUUID(reqCtx, req.User.UserID, body.UUID)
	if err != nil {
		return apiErrJSON(http.StatusInternalServerError, "ownership check failed"), nil
	}
	if !owned {
		return apiErrJSON(http.StatusForbidden, "player does not belong to current user"), nil
	}
	res, err := p.doHubRequest(reqCtx, http.MethodPost, "/profile/bind", nil, map[string]string{
		"uuid": normalizeUUIDHex(body.UUID),
	}, true, 6*time.Second)
	if err != nil {
		p.ctx.Log.Warn(fmt.Sprintf("Union /profile/bind failed: %v", err))
		return apiErrJSON(http.StatusBadGateway, "failed to reach MUA Union Hub"), nil
	}
	return forwardHubJSONResponse(res), nil
}

func (p *MUAUnionPlugin) handleUserBindTo(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	if req.User == nil || req.User.UserID <= 0 {
		return apiErrJSON(http.StatusUnauthorized, "authentication required"), nil
	}
	var body struct {
		UUID  string `json:"uuid"`
		Token string `json:"token"`
	}
	if err := req.DecodeJSON(&body); err != nil || strings.TrimSpace(body.UUID) == "" || strings.TrimSpace(body.Token) == "" {
		return apiErrJSON(http.StatusBadRequest, "uuid and token are required"), nil
	}
	owned, err := p.verifyUserOwnsPlayerUUID(reqCtx, req.User.UserID, body.UUID)
	if err != nil {
		return apiErrJSON(http.StatusInternalServerError, "ownership check failed"), nil
	}
	if !owned {
		return apiErrJSON(http.StatusForbidden, "player does not belong to current user"), nil
	}
	res, err := p.doHubRequest(reqCtx, http.MethodPost, "/profile/bindto", nil, map[string]string{
		"uuid":  normalizeUUIDHex(body.UUID),
		"token": strings.TrimSpace(body.Token),
	}, true, 6*time.Second)
	if err != nil {
		p.ctx.Log.Warn(fmt.Sprintf("Union /profile/bindto failed: %v", err))
		return apiErrJSON(http.StatusBadGateway, "failed to reach MUA Union Hub"), nil
	}
	return forwardHubJSONResponse(res), nil
}

func (p *MUAUnionPlugin) handleUserUnbind(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	if req.User == nil || req.User.UserID <= 0 {
		return apiErrJSON(http.StatusUnauthorized, "authentication required"), nil
	}
	var body struct {
		UUID string `json:"uuid"`
	}
	if err := req.DecodeJSON(&body); err != nil || strings.TrimSpace(body.UUID) == "" {
		return apiErrJSON(http.StatusBadRequest, "uuid is required"), nil
	}
	owned, err := p.verifyUserOwnsPlayerUUID(reqCtx, req.User.UserID, body.UUID)
	if err != nil {
		return apiErrJSON(http.StatusInternalServerError, "ownership check failed"), nil
	}
	if !owned {
		return apiErrJSON(http.StatusForbidden, "player does not belong to current user"), nil
	}
	res, err := p.doHubRequest(reqCtx, http.MethodPost, "/profile/unbind", nil, map[string]string{
		"uuid": normalizeUUIDHex(body.UUID),
	}, true, 6*time.Second)
	if err != nil {
		p.ctx.Log.Warn(fmt.Sprintf("Union /profile/unbind failed: %v", err))
		return apiErrJSON(http.StatusBadGateway, "failed to reach MUA Union Hub"), nil
	}
	return forwardHubJSONResponse(res), nil
}

func (p *MUAUnionPlugin) handleUserRemapUUID(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	if req.User == nil || req.User.UserID <= 0 {
		return apiErrJSON(http.StatusUnauthorized, "authentication required"), nil
	}
	var body struct {
		Me     string `json:"me"`
		Target string `json:"target"`
	}
	if err := req.DecodeJSON(&body); err != nil || strings.TrimSpace(body.Me) == "" || strings.TrimSpace(body.Target) == "" {
		return apiErrJSON(http.StatusBadRequest, "me and target are required"), nil
	}
	meNorm := normalizeUUIDHex(body.Me)
	targetNorm := normalizeUUIDHex(body.Target)
	if !isValidUUIDHex(meNorm) || !isValidUUIDHex(targetNorm) {
		return apiErrJSON(http.StatusBadRequest, "me and target must be valid 32-hex UUIDs"), nil
	}
	owned, err := p.verifyUserOwnsPlayerUUID(reqCtx, req.User.UserID, meNorm)
	if err != nil {
		return apiErrJSON(http.StatusInternalServerError, "ownership check failed"), nil
	}
	if !owned {
		return apiErrJSON(http.StatusForbidden, "player does not belong to current user"), nil
	}
	res, err := p.doHubRequest(reqCtx, http.MethodPost, "/profile/remapuuid", nil, map[string]string{
		"me":     meNorm,
		"target": targetNorm,
	}, true, 6*time.Second)
	if err != nil {
		p.ctx.Log.Warn(fmt.Sprintf("Union /profile/remapuuid failed: %v", err))
		return apiErrJSON(http.StatusBadGateway, "failed to reach MUA Union Hub"), nil
	}
	return forwardHubJSONResponse(res), nil
}

func (p *MUAUnionPlugin) handleAdminStatus(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	apiRoot, apiRootErr := p.unionAPIRoot()
	siteURL, _ := p.ctx.Site.URL(reqCtx)
	pubSettings, _ := p.ctx.Site.Public(reqCtx)

	keepUUIDOnRename, _ := pubSettings["player.keep_uuid_on_rename"].(bool)
	uuidAlgo, _ := pubSettings["yggdrasil.uuid_algorithm"].(string)
	if uuidAlgo == "" {
		uuidAlgo = "v3"
	}
	requireEmailVerify, _ := pubSettings["auth.require_email_verification"].(bool)

	localServerVer, _ := p.getInt64KV(reqCtx, kvServerListVersion)
	localKeyVer, _ := p.getInt64KV(reqCtx, kvPrivateKeyVersion)
	servers := p.loadCachedServers(reqCtx)
	outbox, _ := listAllOutbox(reqCtx, p.ctx.DB(), 50)
	if outbox == nil {
		outbox = []outboxItem{}
	}

	var hubServerVer, hubKeyVer *int64
	var adminNotice, hubError string
	if apiRootErr != nil {
		hubError = apiRootErr.Error()
	} else if meta, _, err := p.fetchHubMeta(reqCtx, false); err == nil && meta != nil {
		v := meta.Version
		kv := meta.PrivateKeyVersion
		hubServerVer = &v
		hubKeyVer = &kv
		adminNotice = meta.Meta.AdminNotice
	} else if err != nil {
		hubError = err.Error()
	}

	oauth2Enabled := p.ctx.Settings.GetBool("enableUnionOAuth2")
	oauth2Fingerprint := ""
	if pubPEM := p.currentOAuth2PublicKeyPEM(reqCtx); pubPEM != "" {
		oauth2Fingerprint = publicKeySHA256Fingerprint(pubPEM)
	} else if oauth2Enabled {
		if _, pubPEM, err := p.ensureOAuth2KeyPair(reqCtx); err == nil {
			oauth2Fingerprint = publicKeySHA256Fingerprint(pubPEM)
		}
	}

	yggPubFingerprint := ""
	if yggPub, err := p.ctx.Yggdrasil.PublicKey(reqCtx); err == nil && yggPub != "" {
		yggPubFingerprint = publicKeySHA256Fingerprint(yggPub)
	}

	return plugin.JSON(http.StatusOK, map[string]any{
		"wireVersion":         pluginWireVersion,
		"unionApiRoot":        apiRoot,
		"siteUrl":             siteURL,
		"memberKeyConfigured": p.effectiveMemberKey(reqCtx) != "",
		"local": map[string]any{
			"serverListVersion": localServerVer,
			"privateKeyVersion": localKeyVer,
			"yggdrasilPubKeyFp": yggPubFingerprint,
		},
		"hub": map[string]any{
			"serverListVersion": hubServerVer,
			"privateKeyVersion": hubKeyVer,
			"adminNotice":       adminNotice,
			"error":             hubError,
		},
		"checks": map[string]any{
			"keepUuidOnRename":        keepUUIDOnRename,
			"uuidAlgorithm":           uuidAlgo,
			"requireEmailVerify":      requireEmailVerify,
			"hasYggdrasilKeyWrite":    p.ctx.HasPermission("yggdrasil.key.write"),
			"hasPlayersUuidWrite":     p.ctx.HasPermission("players.uuid.write"),
			"allowRemoteKeySync":      p.ctx.Settings.GetBool("allowRemoteKeySync"),
			"allowRemoteUuidRemap":    p.ctx.Settings.GetBool("allowRemoteUUIDRemap"),
			"allowRemoteBackendKey":   p.ctx.Settings.GetBool("allowRemoteBackendKeyRotate"),
			"enableUnionOAuth2":       oauth2Enabled,
			"enableLocalBlacklist":    p.ctx.Settings.GetBool("enableLocalBlacklist"),
			"invitationCodesForUnion": p.ctx.Settings.GetBool("invitationCodesForUnion"),
		},
		"oauth2": map[string]any{
			"enabled":           oauth2Enabled,
			"publicKeySha256Fp": oauth2Fingerprint,
		},
		"servers": servers,
		"outbox":  outbox,
	}), nil
}

func (p *MUAUnionPlugin) handleAdminUpdateList(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	list, err := p.syncServerList(reqCtx)
	if err != nil {
		return apiErrJSON(http.StatusBadGateway, err.Error()), nil
	}
	_ = p.ctx.Audit.Record(reqCtx, "server_list.synced", "plugin", "mua-union", actorAuditMeta(req, map[string]any{
		"version": list.Version,
		"count":   len(list.Servers),
	}))
	return plugin.JSON(http.StatusOK, map[string]any{
		"ok":      true,
		"version": list.Version,
		"count":   len(list.Servers),
	}), nil
}

func (p *MUAUnionPlugin) handleAdminUpdatePrivateKey(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	ver, err := p.syncPrivateKey(reqCtx, "admin_console", actorAuditMeta(req, nil))
	if err != nil {
		return apiErrJSON(http.StatusBadGateway, err.Error()), nil
	}
	return plugin.JSON(http.StatusOK, map[string]any{
		"ok":                true,
		"privateKeyVersion": ver,
	}), nil
}

func (p *MUAUnionPlugin) handleAdminSync(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	count, err := p.triggerFullPlayerSync(reqCtx)
	if err != nil {
		return apiErrJSON(http.StatusBadGateway, err.Error()), nil
	}
	return plugin.JSON(http.StatusOK, map[string]any{
		"ok":    true,
		"count": count,
	}), nil
}

func (p *MUAUnionPlugin) handleAdminDiagnose(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	res, err := p.doHubRequest(reqCtx, http.MethodPost, "/diagnose", nil, nil, true, 12*time.Second)
	if err != nil {
		return plugin.JSON(http.StatusOK, map[string]any{
			"status": "error",
			"data": map[string]any{
				"exception": err.Error(),
			},
		}), nil
	}
	if res.Status >= 200 && res.Status < 300 {
		var data any
		if err := json.Unmarshal(res.Body, &data); err != nil {
			data = map[string]any{"body": string(res.Body)}
		}
		return plugin.JSON(http.StatusOK, map[string]any{
			"status": "ok",
			"data":   data,
		}), nil
	}
	return plugin.JSON(http.StatusOK, map[string]any{
		"status": "error",
		"data": map[string]any{
			"status_code": res.Status,
			"headers":     res.Headers,
			"body":        truncateStr(string(res.Body), 4096),
		},
	}), nil
}

func (p *MUAUnionPlugin) handleAdminRegenerateOAuth2Key(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	pubPEM, err := p.regenerateOAuth2KeyPair(reqCtx)
	if err != nil {
		return apiErrJSON(http.StatusInternalServerError, err.Error()), nil
	}
	fp := publicKeySHA256Fingerprint(pubPEM)
	_ = p.ctx.Audit.Record(reqCtx, "oauth2_key.rotated", "plugin", "mua-union", actorAuditMeta(req, map[string]any{
		"publicKeySha256Fp": fp,
	}))
	return plugin.JSON(http.StatusOK, map[string]any{
		"ok":                true,
		"publicKeySha256Fp": fp,
	}), nil
}

func (p *MUAUnionPlugin) handleAdminRetryOutbox(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	_ = resetAllOutboxDue(reqCtx, p.ctx.DB(), p.nowFunc().Unix())
	budgetCtx, cancel := context.WithTimeout(reqCtx, 20*time.Second)
	defer cancel()
	succeeded, failed := p.processDueOutbox(budgetCtx)
	return plugin.JSON(http.StatusOK, map[string]any{
		"ok":        failed == 0,
		"succeeded": succeeded,
		"failed":    failed,
	}), nil
}

func (p *MUAUnionPlugin) handleAdminDeleteOutbox(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(req.Params["id"]), 10, 64)
	if err != nil || id <= 0 {
		return apiErrJSON(http.StatusBadRequest, "invalid outbox id"), nil
	}
	deleted, err := deleteOutbox(reqCtx, p.ctx.DB(), id)
	if err != nil {
		return apiErrJSON(http.StatusInternalServerError, err.Error()), nil
	}
	return plugin.JSON(http.StatusOK, map[string]any{"ok": true, "deleted": deleted}), nil
}

func (p *MUAUnionPlugin) handleAdminBlacklistList(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	q := url.Values{}
	if v := strings.TrimSpace(req.Query["q"]); v != "" {
		q.Set("q", v)
	}
	if v := strings.TrimSpace(req.Query["page"]); v != "" {
		q.Set("page", v)
	}
	res, err := p.doHubRequest(reqCtx, http.MethodGet, "/blacklist/query", q, nil, true, 8*time.Second)
	if err != nil {
		return apiErrJSON(http.StatusBadGateway, err.Error()), nil
	}
	return forwardHubJSONResponse(res), nil
}

func (p *MUAUnionPlugin) handleAdminBlacklistCreate(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	var body struct {
		Email  string `json:"email"`
		Reason string `json:"reason"`
	}
	if err := req.DecodeJSON(&body); err != nil || strings.TrimSpace(body.Email) == "" {
		return apiErrJSON(http.StatusBadRequest, "email is required"), nil
	}
	res, err := p.doHubRequest(reqCtx, http.MethodPost, "/blacklist/restful", nil, map[string]string{
		"email":  strings.TrimSpace(body.Email),
		"reason": strings.TrimSpace(body.Reason),
	}, true, 8*time.Second)
	if err != nil {
		return apiErrJSON(http.StatusBadGateway, err.Error()), nil
	}
	if res.Status >= 200 && res.Status < 300 {
		_ = p.ctx.Audit.Record(reqCtx, "blacklist.created", "blacklist", strings.TrimSpace(body.Email), actorAuditMeta(req, map[string]any{
			"reason": strings.TrimSpace(body.Reason),
		}))
	}
	return forwardHubJSONResponse(res), nil
}

func (p *MUAUnionPlugin) handleAdminBlacklistInvalidate(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	id := strings.TrimSpace(req.Params["id"])
	if id == "" || !isDigitsOnly(id) {
		return apiErrJSON(http.StatusBadRequest, "invalid blacklist entry id"), nil
	}
	res, err := p.doHubRequest(reqCtx, http.MethodPut, "/blacklist/invalidate/"+url.PathEscape(id), nil, nil, true, 8*time.Second)
	if err != nil {
		return apiErrJSON(http.StatusBadGateway, err.Error()), nil
	}
	if res.Status >= 200 && res.Status < 300 {
		_ = p.ctx.Audit.Record(reqCtx, "blacklist.invalidated", "blacklist", id, actorAuditMeta(req, nil))
	}
	return forwardHubJSONResponse(res), nil
}

func (p *MUAUnionPlugin) handleAdminBlacklistDelete(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	id := strings.TrimSpace(req.Params["id"])
	if id == "" || !isDigitsOnly(id) {
		return apiErrJSON(http.StatusBadRequest, "invalid blacklist entry id"), nil
	}
	res, err := p.doHubRequest(reqCtx, http.MethodDelete, "/blacklist/restful/"+url.PathEscape(id), nil, nil, true, 8*time.Second)
	if err != nil {
		return apiErrJSON(http.StatusBadGateway, err.Error()), nil
	}
	if res.Status >= 200 && res.Status < 300 {
		_ = p.ctx.Audit.Record(reqCtx, "blacklist.deleted", "blacklist", id, actorAuditMeta(req, nil))
	}
	return forwardHubJSONResponse(res), nil
}

func forwardHubJSONResponse(res *hubHTTPResult) *plugin.HTTPResponse {
	if res == nil {
		return apiErrJSON(http.StatusBadGateway, "empty hub response")
	}
	if len(res.Body) == 0 {
		if res.Status == http.StatusNoContent {
			return plugin.NoContent()
		}
		if res.Status >= 400 {
			return apiErrJSON(res.Status, fmt.Sprintf("Union hub returned HTTP %d", res.Status))
		}
		return plugin.JSON(res.Status, map[string]any{"ok": res.Status >= 200 && res.Status < 300})
	}
	var parsed any
	if err := json.Unmarshal(res.Body, &parsed); err == nil {
		if res.Status >= 400 {
			if m, ok := parsed.(map[string]any); ok {
				if _, hasDetail := m["detail"]; !hasDetail {
					for _, k := range []string{"errorMessage", "message", "error"} {
						if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
							m["detail"] = s
							break
						}
					}
				}
				return plugin.JSON(res.Status, m)
			}
		}
		return plugin.JSON(res.Status, parsed)
	}
	msg := truncateStr(string(res.Body), 1024)
	if res.Status >= 400 {
		return plugin.JSON(res.Status, map[string]any{
			"status":  res.Status,
			"message": msg,
			"detail":  msg,
		})
	}
	return plugin.JSON(res.Status, map[string]any{
		"status":  res.Status,
		"message": msg,
	})
}

func normalizeUUIDHex(raw string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), "-", ""))
}

func isDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isChineseLocale(locale string) bool {
	l := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(locale, "_", "-")))
	return l == "" || l == "zh" || strings.HasPrefix(l, "zh-")
}
