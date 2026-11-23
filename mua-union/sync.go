package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	plugin "github.com/Entropy-Increase-Team/skinloom/sdk/go"
)

type playerEventPayload struct {
	PlayerID int64  `json:"playerId"`
	UserID   int64  `json:"userId"`
	Name     string `json:"name"`
	UUID     string `json:"uuid"`
	OldName  string `json:"oldName,omitempty"`
}

type yggdrasilJoinPayload struct {
	UserID   int64  `json:"userId"`
	PlayerID int64  `json:"playerId"`
	UUID     string `json:"uuid"`
	ServerID string `json:"serverId"`
	IP       string `json:"ip,omitempty"`
}

func (p *MUAUnionPlugin) registerHooksAndCron(ctx *plugin.Context) {
	ctx.Hooks.Event("player.created", func(evCtx context.Context, raw json.RawMessage) error {
		var ev playerEventPayload
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil
		}
		p.syncPlayerChangeOrQueue(evCtx, "create", ev.UUID, ev.Name)
		return nil
	})

	ctx.Hooks.Event("player.renamed", func(evCtx context.Context, raw json.RawMessage) error {
		var ev playerEventPayload
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil
		}
		p.syncPlayerChangeOrQueue(evCtx, "rename", ev.UUID, ev.Name)
		return nil
	})

	ctx.Hooks.Event("player.deleted", func(evCtx context.Context, raw json.RawMessage) error {
		var ev playerEventPayload
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil
		}
		p.syncPlayerChangeOrQueue(evCtx, "delete", ev.UUID, ev.Name)
		return nil
	})

	ctx.Hooks.Guard("yggdrasil.join", func(guardCtx context.Context, raw json.RawMessage) error {
		if !p.ctx.Settings.GetBool("enableLocalBlacklist") {
			return nil
		}
		if p.effectiveMemberKey(guardCtx) == "" {
			return nil
		}
		var payload yggdrasilJoinPayload
		if err := json.Unmarshal(raw, &payload); err != nil || payload.UserID <= 0 {
			return nil
		}
		u, err := p.ctx.Users.Get(guardCtx, payload.UserID)
		if err != nil || u == nil || strings.TrimSpace(u.Email) == "" {
			return nil
		}
		allowed, err := p.checkBlacklistAllowed(guardCtx, u.Email)
		if err != nil {
			p.ctx.Log.Warn(fmt.Sprintf("Union blacklist check failed for user %d: %v", payload.UserID, err))
			return err // GuardYggdrasilJoin is FailOpen in host, so transient Hub outage does not lock out users.
		}
		if !allowed {
			return plugin.Reject("Your account is restricted by the MUA Union blacklist.")
		}
		return nil
	})

	ctx.Cron.Schedule("mua-union-maintenance", "*/5 * * * *", func(cronCtx context.Context) error {
		now := p.nowFunc().Unix()
		_ = pruneExpiredNonces(cronCtx, p.ctx.DB(), now)
		_ = pruneStaleOutbox(cronCtx, p.ctx.DB(), now-30*24*3600)
		_, _ = p.processDueOutbox(cronCtx)
		return nil
	})
}

func (p *MUAUnionPlugin) syncPlayerChangeOrQueue(ctx context.Context, action, uuid, name string) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return
	}
	if p.effectiveMemberKey(ctx) == "" {
		return
	}
	// Preserve causal order per UUID: if earlier events for this UUID are pending in
	// sync_outbox, coalesce into the queue instead of bypassing out of order.
	if pending, err := hasPendingOutboxForUUID(ctx, p.ctx.DB(), uuid); err == nil && pending {
		_ = enqueueOutboxCoalesced(ctx, p.ctx.DB(), action, uuid, name, "queued behind pending sync", p.nowFunc().Unix())
		return
	}
	if err := p.dispatchPlayerSync(ctx, action, uuid, name); err != nil {
		p.ctx.Log.Warn(fmt.Sprintf("Union outbound %s sync for player %s (%s) failed, queuing retry: %v", action, name, uuid, err))
		_ = enqueueOutboxCoalesced(ctx, p.ctx.DB(), action, uuid, name, err.Error(), p.nowFunc().Unix())
	}
}

func (p *MUAUnionPlugin) dispatchPlayerSync(ctx context.Context, action, uuid, name string) error {
	switch action {
	case "create":
		return p.postProfileCreated(ctx, uuid, name)
	case "rename":
		return p.putProfileRenamed(ctx, uuid, name)
	case "delete":
		return p.deleteProfile(ctx, uuid)
	default:
		return fmt.Errorf("unknown sync action %q", action)
	}
}

func (p *MUAUnionPlugin) processDueOutbox(ctx context.Context) (int, int) {
	if p.effectiveMemberKey(ctx) == "" {
		return 0, 0
	}
	p.outboxMu.Lock()
	defer p.outboxMu.Unlock()

	startNow := p.nowFunc().Unix()
	items, err := listDueOutbox(ctx, p.ctx.DB(), startNow, 50)
	if err != nil || len(items) == 0 {
		return 0, 0
	}
	succeeded, failed := 0, 0
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		claimNow := p.nowFunc().Unix()
		claimed, cErr := claimOutboxItem(ctx, p.ctx.DB(), it.ID, claimNow, claimNow+120)
		if cErr != nil || !claimed {
			continue
		}
		if err := p.dispatchPlayerSync(ctx, it.Action, it.UUID, it.Name); err != nil {
			doneNow := p.nowFunc().Unix()
			nextAttempts := it.Attempts + 1
			// Exponential backoff: 60s, 120s, 240s, ..., capped at 3600s.
			delay := int64(60) << min(nextAttempts-1, 6)
			if delay > 3600 {
				delay = 3600
			}
			_ = markOutboxAttemptFailed(ctx, p.ctx.DB(), it.ID, nextAttempts, doneNow+delay, err.Error())
			failed++
			continue
		}
		_, _ = deleteOutbox(ctx, p.ctx.DB(), it.ID)
		succeeded++
	}
	return succeeded, failed
}

func (p *MUAUnionPlugin) syncServerList(ctx context.Context) (*serverListResponse, error) {
	list, err := p.fetchServerList(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(list.Servers)
	if err != nil {
		return nil, err
	}
	if err := setKV(ctx, p.ctx.DB(), kvServerList, string(raw)); err != nil {
		return nil, err
	}
	if err := setKV(ctx, p.ctx.DB(), kvServerListVersion, strconv.FormatInt(list.Version, 10)); err != nil {
		return nil, err
	}
	p.setCachedServers(list.Servers)
	p.ctx.Log.Info(fmt.Sprintf("Synced MUA Union server list: version=%d servers=%d", list.Version, len(list.Servers)))
	return list, nil
}

func (p *MUAUnionPlugin) syncPrivateKey(ctx context.Context, source string, actorMeta map[string]any) (int64, error) {
	if !p.ctx.HasPermission("yggdrasil.key.write") {
		return 0, errors.New("yggdrasil.key.write permission is not granted")
	}
	res, err := p.fetchPrivateKey(ctx)
	if err != nil {
		return 0, err
	}
	if err := p.ctx.Yggdrasil.SetSigningKey(ctx, res.PrivateKey); err != nil {
		return 0, fmt.Errorf("apply signing key to SkinLoom core: %w", err)
	}
	if err := setKV(ctx, p.ctx.DB(), kvPrivateKeyVersion, strconv.FormatInt(res.PrivateKeyVersion, 10)); err != nil {
		return 0, err
	}
	meta := map[string]any{
		"source":            source,
		"privateKeyVersion": res.PrivateKeyVersion,
	}
	for k, v := range actorMeta {
		meta[k] = v
	}
	_ = p.ctx.Audit.Record(ctx, "signing_key.synced", "plugin", "mua-union", meta)
	p.ctx.Log.Info(fmt.Sprintf("Synced MUA Union Yggdrasil signing key: version=%d source=%s", res.PrivateKeyVersion, source))
	return res.PrivateKeyVersion, nil
}

func (p *MUAUnionPlugin) triggerFullPlayerSync(ctx context.Context) (int, error) {
	profiles := map[string]string{}
	var afterID int64
	for {
		page, err := p.ctx.Players.ListAll(ctx, afterID, 500)
		if err != nil {
			return 0, fmt.Errorf("list players (afterID=%d): %w", afterID, err)
		}
		if page == nil || len(page.Items) == 0 {
			break
		}
		for _, pl := range page.Items {
			u := strings.TrimSpace(pl.UUID)
			if u != "" && pl.Name != "" {
				profiles[u] = pl.Name
			}
		}
		if page.NextAfterID <= afterID {
			break
		}
		afterID = page.NextAfterID
	}
	if err := p.postFullSync(ctx, profiles); err != nil {
		return 0, err
	}
	p.ctx.Log.Info(fmt.Sprintf("Completed MUA Union full player sync: count=%d", len(profiles)))
	return len(profiles), nil
}

func (p *MUAUnionPlugin) loadCachedServers(ctx context.Context) []serverEntry {
	now := p.nowFunc()
	p.serversMu.RLock()
	if p.serversLoaded && now.Before(p.serversExp) {
		cp := append([]serverEntry(nil), p.cachedServers...)
		p.serversMu.RUnlock()
		return cp
	}
	p.serversMu.RUnlock()

	raw, ok, err := getKV(ctx, p.ctx.DB(), kvServerList)
	if err != nil || !ok {
		return []serverEntry{}
	}
	servers := parseServerEntries([]byte(raw))
	p.setCachedServers(servers)
	return servers
}

func (p *MUAUnionPlugin) setCachedServers(servers []serverEntry) {
	p.serversMu.Lock()
	p.cachedServers = append([]serverEntry(nil), servers...)
	p.serversLoaded = true
	p.serversExp = p.nowFunc().Add(haCacheTTL)
	p.serversMu.Unlock()
}

func extractSkinDomains(servers []serverEntry) []string {
	seen := make(map[string]struct{}, len(servers))
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		raw := strings.TrimSpace(s.BSRoot)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		host := strings.ToLower(strings.TrimSpace(u.Hostname()))
		if host == "" {
			continue
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}
