package main

import (
	"context"
	"crypto/rsa"
	"embed"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	plugin "github.com/Entropy-Increase-Team/skinloom/sdk/go"
)

//go:embed migrations
var migrationsFS embed.FS

// pluginWireVersion advertises MUA Union protocol compatibility while identifying SkinLoom.
const pluginWireVersion = "6.1.0-0.3.2-skinloom-1.0.0"

type blacklistCacheEntry struct {
	allowed bool
	exp     time.Time
}

// MUAUnionPlugin is the official Tier 2 Native Go plugin implementing the
// Minecraft University Alliance (MUA) Union federation protocol on SkinLoom.
type MUAUnionPlugin struct {
	ctx *plugin.Context

	defaultHTTP       *http.Client
	httpClient        *http.Client // optional override for tests
	allowInsecureTest bool         // test-only flag allowing http:// httptest servers
	clock             func() time.Time

	metaMu           sync.RWMutex
	metaRefreshMu    sync.Mutex
	cachedMeta       *hubMetadata
	cachedHostPub    *rsa.PublicKey
	cachedMetaRoot   string
	cachedMetaExp    time.Time
	lastForceRefresh time.Time

	oauth2BackendPub     *rsa.PublicKey
	oauth2BackendPubRoot string
	oauth2BackendPubExp  time.Time

	secLevel    int
	secLevelKey string
	secLevelExp time.Time

	serversMu     sync.RWMutex
	cachedServers []serverEntry
	serversLoaded bool
	serversExp    time.Time

	oauth2KeyMu  sync.Mutex
	oauth2Priv   *rsa.PrivateKey
	oauth2PubPEM string
	oauth2KeyExp time.Time

	blacklistMu    sync.RWMutex
	blacklistCache map[string]blacklistCacheEntry

	outboxMu sync.Mutex
}

func (p *MUAUnionPlugin) nowFunc() time.Time {
	if p.clock != nil {
		return p.clock()
	}
	return time.Now()
}

func (p *MUAUnionPlugin) Setup(ctx *plugin.Context) error {
	p.ctx = ctx
	if p.defaultHTTP == nil {
		p.defaultHTTP = newSSRFSafeHTTPClient()
	}

	db := ctx.DB()
	if db == nil {
		return fmt.Errorf("mua-union requires isolated SQL storage")
	}
	if err := db.Migrate(context.Background(), migrationsFS, "migrations"); err != nil {
		return fmt.Errorf("migrate mua-union db: %w", err)
	}

	// Register dynamic Yggdrasil metadata provider to expose synced Union member skinDomains.
	ctx.Yggdrasil.RegisterMetaProvider(func(metaCtx context.Context) (*plugin.YggdrasilMetaExtension, error) {
		servers := p.loadCachedServers(metaCtx)
		domains := extractSkinDomains(servers)
		if len(domains) == 0 {
			return &plugin.YggdrasilMetaExtension{}, nil
		}
		return &plugin.YggdrasilMetaExtension{
			SkinDomains: domains,
		}, nil
	})

	// Clear cached Hub metadata and derived caches when settings change; also supersede
	// any legacy runtime rotated key whenever an administrator edits or clears unionMemberKey.
	ctx.Settings.OnChange(func(changeCtx context.Context, settings map[string]any) error {
		p.metaMu.Lock()
		p.cachedMeta = nil
		p.cachedHostPub = nil
		p.cachedMetaExp = time.Time{}
		p.oauth2BackendPub = nil
		p.oauth2BackendPubExp = time.Time{}
		p.secLevelExp = time.Time{}
		p.metaMu.Unlock()

		p.blacklistMu.Lock()
		clear(p.blacklistCache)
		p.blacklistMu.Unlock()

		newKey, _ := settings["unionMemberKey"].(string)
		newKey = strings.TrimSpace(newKey)
		lastKey, _, _ := getKV(changeCtx, p.ctx.DB(), kvLastSettingKey)
		if newKey != lastKey {
			_ = deleteKV(changeCtx, p.ctx.DB(), kvRuntimeMemberKey)
			if newKey == "" {
				_ = deleteKV(changeCtx, p.ctx.DB(), kvLastSettingKey)
			} else {
				_ = setKV(changeCtx, p.ctx.DB(), kvLastSettingKey, newKey)
			}
		}
		return nil
	})

	p.registerPublicUnionRoutes(ctx)
	p.registerUnionOAuth2Routes(ctx)
	p.registerPluginAPIRoutes(ctx)
	p.registerHooksAndCron(ctx)
	p.registerImportService(ctx)

	return nil
}

func (p *MUAUnionPlugin) HealthCheck(ctx context.Context) error {
	if p.ctx == nil || p.ctx.DB() == nil {
		return fmt.Errorf("mua-union database unavailable")
	}
	return p.ctx.DB().SQL().PingContext(ctx)
}

func (p *MUAUnionPlugin) effectiveMemberKey(ctx context.Context) string {
	settingKey := strings.TrimSpace(p.ctx.Settings.GetString("unionMemberKey"))
	if p.ctx != nil && p.ctx.DB() != nil {
		runtimeKey, ok, err := getKV(ctx, p.ctx.DB(), kvRuntimeMemberKey)
		if err == nil && ok && strings.TrimSpace(runtimeKey) != "" {
			lastSetting, hasLast, _ := getKV(ctx, p.ctx.DB(), kvLastSettingKey)
			// Only honor legacy runtime rotated key when settingKey still matches lastSetting
			// (or if settingKey was empty when rotated, i.e., !hasLast || lastSetting == "").
			if settingKey == lastSetting && (settingKey != "" || !hasLast || lastSetting == "") {
				return strings.TrimSpace(runtimeKey)
			}
		}
	}
	return settingKey
}

func (p *MUAUnionPlugin) getInt64KV(ctx context.Context, key string) (int64, error) {
	raw, ok, err := getKV(ctx, p.ctx.DB(), key)
	if err != nil || !ok {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func main() {
	plugin.Serve(&MUAUnionPlugin{})
}
