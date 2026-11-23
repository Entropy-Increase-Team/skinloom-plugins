package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultUnionAPIRoot    = "https://skin.mualliance.ltd/api/union"
	maxHubResponseBytes    = 4 * 1024 * 1024
	hubMetaCacheTTL        = 10 * time.Minute
	hubForceRefreshMinGap  = 60 * time.Second
	blacklistCacheTTL      = 60 * time.Second
	maxBlacklistCacheItems = 2048
)

var (
	thisNetworkPrefix  = netip.MustParsePrefix("0.0.0.0/8")
	cgnatPrefix        = netip.MustParsePrefix("100.64.0.0/10")
	ietfProtoPrefix    = netip.MustParsePrefix("192.0.0.0/24")
	testNet1Prefix     = netip.MustParsePrefix("192.0.2.0/24")
	sixToFourRelay     = netip.MustParsePrefix("192.88.99.0/24")
	benchmarkPrefix    = netip.MustParsePrefix("198.18.0.0/15")
	testNet2Prefix     = netip.MustParsePrefix("198.51.100.0/24")
	testNet3Prefix     = netip.MustParsePrefix("203.0.113.0/24")
	reservedClassE     = netip.MustParsePrefix("240.0.0.0/4")
	ipv6DiscardPrefix  = netip.MustParsePrefix("100::/64")
	ipv6NAT64Prefix    = netip.MustParsePrefix("64:ff9b::/96")
	ipv6NAT64LocPrefix = netip.MustParsePrefix("64:ff9b:1::/48")
	ipv6IETFPrefix     = netip.MustParsePrefix("2001::/23")
	ipv6DocPrefix      = netip.MustParsePrefix("2001:db8::/32")
	ipv6SixToFour      = netip.MustParsePrefix("2002::/16")
)

func isPublicGlobalIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() || addr.IsUnspecified() || addr.IsInterfaceLocalMulticast() {
		return false
	}
	if addr.Is4() {
		if thisNetworkPrefix.Contains(addr) || cgnatPrefix.Contains(addr) ||
			ietfProtoPrefix.Contains(addr) || testNet1Prefix.Contains(addr) ||
			sixToFourRelay.Contains(addr) || benchmarkPrefix.Contains(addr) ||
			testNet2Prefix.Contains(addr) || testNet3Prefix.Contains(addr) ||
			reservedClassE.Contains(addr) {
			return false
		}
	} else {
		if ipv6DiscardPrefix.Contains(addr) || ipv6NAT64Prefix.Contains(addr) ||
			ipv6NAT64LocPrefix.Contains(addr) || ipv6IETFPrefix.Contains(addr) ||
			ipv6DocPrefix.Contains(addr) || ipv6SixToFour.Contains(addr) {
			return false
		}
	}
	return true
}

// newSSRFSafeHTTPClient creates an HTTP client that resolves DNS and verifies
// every target IP is a public global unicast address before connecting, while
// refusing redirects to avoid cross-host credential leakage or redirect SSRF.
func newSSRFSafeHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:               nil,
		MaxIdleConns:        20,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			lowerHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
			if lowerHost == "" || lowerHost == "localhost" ||
				strings.HasSuffix(lowerHost, ".localhost") ||
				strings.HasSuffix(lowerHost, ".local") ||
				strings.HasSuffix(lowerHost, ".internal") {
				return nil, fmt.Errorf("blocked internal host %q", host)
			}
			if ip := net.ParseIP(host); ip != nil {
				if !isPublicGlobalIP(ip) {
					return nil, fmt.Errorf("blocked non-public IP %s", ip.String())
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IP addresses resolved for %q", host)
			}
			var safeIPs []net.IP
			for _, rec := range ips {
				if !isPublicGlobalIP(rec.IP) {
					return nil, fmt.Errorf("blocked non-public resolved IP %s for host %q", rec.IP.String(), host)
				}
				safeIPs = append(safeIPs, rec.IP)
			}
			var lastErr error
			for _, ip := range safeIPs {
				conn, dErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dErr == nil {
					return conn, nil
				}
				lastErr = dErr
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type hubMetadata struct {
	Version                     int64  `json:"version"`
	PrivateKeyVersion           int64  `json:"privateKeyVersion"`
	UnionHostSignaturePublicKey string `json:"union_host_signature_public_key"`
	Meta                        struct {
		Description   string `json:"description"`
		DescriptionEN string `json:"description_en"`
		AdminNotice   string `json:"admin_notice"`
	} `json:"meta"`
}

type serverEntry struct {
	Code   string `json:"code"`
	BSRoot string `json:"bs_root"`
	Name   string `json:"name,omitempty"`
}

type serverListResponse struct {
	Servers []serverEntry `json:"servers"`
	Version int64         `json:"version"`
}

type privateKeyResponse struct {
	PrivateKey        string `json:"privateKey"`
	PrivateKeyVersion int64  `json:"privateKeyVersion"`
}

type hubHTTPResult struct {
	Status  int
	Headers map[string][]string
	Body    []byte
}

func (p *MUAUnionPlugin) unionAPIRoot() (string, error) {
	raw := strings.TrimSpace(p.ctx.Settings.GetString("unionApiRoot"))
	if raw == "" {
		raw = defaultUnionAPIRoot
	}
	raw = strings.TrimRight(raw, "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid unionApiRoot URL")
	}
	if u.Scheme != "https" && !(p.allowInsecureTest && u.Scheme == "http") {
		return "", fmt.Errorf("unionApiRoot must use https:// (got %q)", u.Scheme)
	}
	return raw, nil
}

func (p *MUAUnionPlugin) outboundHTTPClient() *http.Client {
	if p.httpClient != nil {
		return p.httpClient
	}
	return p.defaultHTTP
}

func (p *MUAUnionPlugin) doHubRequest(
	ctx context.Context,
	method, relPath string,
	query url.Values,
	jsonBody any,
	withMemberKey bool,
	timeout time.Duration,
) (*hubHTTPResult, error) {
	root, err := p.unionAPIRoot()
	if err != nil {
		return nil, err
	}
	fullURL := root + relPath
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if jsonBody != nil {
		encoded, mErr := json.Marshal(jsonBody)
		if mErr != nil {
			return nil, mErr
		}
		bodyReader = bytes.NewReader(encoded)
	}

	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, fullURL, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "SkinLoom-MUA-Union/"+pluginWireVersion)
	if jsonBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withMemberKey {
		key := p.effectiveMemberKey(ctx)
		if key == "" {
			return nil, errors.New("union member key is not configured")
		}
		req.Header.Set("X-Union-Member-Key", key)
	}

	resp, err := p.outboundHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxHubResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(bodyBytes) > maxHubResponseBytes {
		return nil, errors.New("union hub response exceeded size limit")
	}
	return &hubHTTPResult{
		Status:  resp.StatusCode,
		Headers: resp.Header.Clone(),
		Body:    bodyBytes,
	}, nil
}

func (p *MUAUnionPlugin) fetchHubMeta(ctx context.Context, forceRefresh bool) (*hubMetadata, *rsa.PublicKey, error) {
	root, err := p.unionAPIRoot()
	if err != nil {
		return nil, nil, err
	}
	now := p.nowFunc()
	p.metaMu.RLock()
	if p.cachedMeta != nil && p.cachedMetaRoot == root {
		if !forceRefresh && now.Before(p.cachedMetaExp) {
			meta, pub := p.cachedMeta, p.cachedHostPub
			p.metaMu.RUnlock()
			return meta, pub, nil
		}
		// Cooldown guard prevents unauthenticated webhook floods from amplifying outbound Hub traffic.
		if forceRefresh && !p.lastForceRefresh.IsZero() && now.Sub(p.lastForceRefresh) < hubForceRefreshMinGap {
			meta, pub := p.cachedMeta, p.cachedHostPub
			p.metaMu.RUnlock()
			return meta, pub, nil
		}
	}
	p.metaMu.RUnlock()

	// Coalesce concurrent metadata refreshes so a burst of requests triggers at most one Hub fetch.
	p.metaRefreshMu.Lock()
	defer p.metaRefreshMu.Unlock()

	now = p.nowFunc()
	p.metaMu.RLock()
	if p.cachedMeta != nil && p.cachedMetaRoot == root {
		if !forceRefresh && now.Before(p.cachedMetaExp) {
			meta, pub := p.cachedMeta, p.cachedHostPub
			p.metaMu.RUnlock()
			return meta, pub, nil
		}
		if forceRefresh && !p.lastForceRefresh.IsZero() && now.Sub(p.lastForceRefresh) < hubForceRefreshMinGap {
			meta, pub := p.cachedMeta, p.cachedHostPub
			p.metaMu.RUnlock()
			return meta, pub, nil
		}
	}
	p.metaMu.RUnlock()

	res, err := p.doHubRequest(ctx, http.MethodGet, "", nil, nil, false, 6*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch union hub metadata: %w", err)
	}
	if res.Status < 200 || res.Status >= 300 {
		return nil, nil, fmt.Errorf("union hub metadata returned HTTP %d", res.Status)
	}
	var meta hubMetadata
	if err := json.Unmarshal(res.Body, &meta); err != nil {
		return nil, nil, fmt.Errorf("decode union hub metadata: %w", err)
	}
	var pub *rsa.PublicKey
	if strings.TrimSpace(meta.UnionHostSignaturePublicKey) != "" {
		parsedPub, pErr := parseRSAPublicKeyPEM(meta.UnionHostSignaturePublicKey)
		if pErr != nil {
			return nil, nil, fmt.Errorf("invalid union hub public key: %w", pErr)
		}
		pub = parsedPub
	}

	now = p.nowFunc()
	p.metaMu.Lock()
	p.cachedMeta = &meta
	p.cachedHostPub = pub
	p.cachedMetaRoot = root
	p.cachedMetaExp = now.Add(hubMetaCacheTTL)
	if forceRefresh {
		p.lastForceRefresh = now
	}
	p.metaMu.Unlock()

	return &meta, pub, nil
}

func (p *MUAUnionPlugin) fetchServerList(ctx context.Context) (*serverListResponse, error) {
	res, err := p.doHubRequest(ctx, http.MethodGet, "/serverlist", nil, nil, true, 8*time.Second)
	if err != nil {
		return nil, err
	}
	if res.Status < 200 || res.Status >= 300 {
		return nil, fmt.Errorf("hub /serverlist returned HTTP %d: %s", res.Status, truncateStr(string(res.Body), 200))
	}
	var raw struct {
		Servers json.RawMessage `json:"servers"`
		Version int64           `json:"version"`
	}
	if err := json.Unmarshal(res.Body, &raw); err != nil {
		return nil, fmt.Errorf("decode /serverlist response: %w", err)
	}
	servers := parseServerEntries(raw.Servers)
	return &serverListResponse{
		Servers: servers,
		Version: raw.Version,
	}, nil
}

// parseServerEntries accepts either a JSON array of ServerEntry or a JSON object map keyed by server id/code.
func parseServerEntries(raw []byte) []serverEntry {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" || string(trimmed) == "[]" {
		return []serverEntry{}
	}
	var arr []serverEntry
	if err := json.Unmarshal(trimmed, &arr); err == nil {
		return arr
	}
	var obj map[string]serverEntry
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		out := make([]serverEntry, 0, len(obj))
		for k, v := range obj {
			if v.Code == "" {
				v.Code = k
			}
			out = append(out, v)
		}
		return out
	}
	return []serverEntry{}
}

func (p *MUAUnionPlugin) fetchPrivateKey(ctx context.Context) (*privateKeyResponse, error) {
	res, err := p.doHubRequest(ctx, http.MethodGet, "/privatekey", nil, nil, true, 8*time.Second)
	if err != nil {
		return nil, err
	}
	if res.Status < 200 || res.Status >= 300 {
		return nil, fmt.Errorf("hub /privatekey returned HTTP %d: %s", res.Status, truncateStr(string(res.Body), 200))
	}
	var out privateKeyResponse
	if err := json.Unmarshal(res.Body, &out); err != nil {
		return nil, fmt.Errorf("decode /privatekey response: %w", err)
	}
	if strings.TrimSpace(out.PrivateKey) == "" {
		return nil, errors.New("hub /privatekey returned empty privateKey")
	}
	return &out, nil
}

func (p *MUAUnionPlugin) postFullSync(ctx context.Context, profileList map[string]string) error {
	res, err := p.doHubRequest(ctx, http.MethodPost, "/sync", nil, map[string]any{
		"profileList": profileList,
	}, true, 15*time.Second)
	if err != nil {
		return err
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("hub /sync returned HTTP %d: %s", res.Status, truncateStr(string(res.Body), 200))
	}
	return nil
}

func (p *MUAUnionPlugin) postProfileCreated(ctx context.Context, uuid, name string) error {
	res, err := p.doHubRequest(ctx, http.MethodPost, "/profile", nil, map[string]string{
		"id":   uuid,
		"name": name,
	}, true, 6*time.Second)
	if err != nil {
		return err
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("hub POST /profile returned HTTP %d", res.Status)
	}
	return nil
}

func (p *MUAUnionPlugin) putProfileRenamed(ctx context.Context, uuid, name string) error {
	res, err := p.doHubRequest(ctx, http.MethodPut, "/profile/"+url.PathEscape(uuid), nil, map[string]string{
		"name": name,
	}, true, 6*time.Second)
	if err != nil {
		return err
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("hub PUT /profile/%s returned HTTP %d", uuid, res.Status)
	}
	return nil
}

func (p *MUAUnionPlugin) deleteProfile(ctx context.Context, uuid string) error {
	res, err := p.doHubRequest(ctx, http.MethodDelete, "/profile/"+url.PathEscape(uuid), nil, nil, true, 6*time.Second)
	if err != nil {
		return err
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("hub DELETE /profile/%s returned HTTP %d", uuid, res.Status)
	}
	return nil
}

func (p *MUAUnionPlugin) fetchOAuth2BackendPubKey(ctx context.Context) (*rsa.PublicKey, error) {
	root, err := p.unionAPIRoot()
	if err != nil {
		return nil, err
	}
	now := p.nowFunc()
	p.metaMu.RLock()
	if p.oauth2BackendPub != nil && p.oauth2BackendPubRoot == root && now.Before(p.oauth2BackendPubExp) {
		pub := p.oauth2BackendPub
		p.metaMu.RUnlock()
		return pub, nil
	}
	p.metaMu.RUnlock()

	res, err := p.doHubRequest(ctx, http.MethodGet, "/oauth2/backend", nil, nil, false, 6*time.Second)
	if err != nil {
		return nil, err
	}
	if res.Status < 200 || res.Status >= 300 {
		return nil, fmt.Errorf("hub /oauth2/backend returned HTTP %d", res.Status)
	}
	var payload struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(res.Body, &payload); err != nil {
		return nil, fmt.Errorf("decode /oauth2/backend: %w", err)
	}
	if strings.TrimSpace(payload.PublicKey) == "" {
		return nil, errors.New("hub /oauth2/backend returned empty publicKey")
	}
	pub, err := parseRSAPublicKeyPEM(payload.PublicKey)
	if err != nil {
		return nil, err
	}
	p.metaMu.Lock()
	p.oauth2BackendPub = pub
	p.oauth2BackendPubRoot = root
	p.oauth2BackendPubExp = p.nowFunc().Add(hubMetaCacheTTL)
	p.metaMu.Unlock()
	return pub, nil
}

func (p *MUAUnionPlugin) fetchSecurityLevel(ctx context.Context) (int, error) {
	key := p.effectiveMemberKey(ctx)
	if key == "" {
		return -1, errors.New("member key not configured")
	}
	now := p.nowFunc()
	p.metaMu.RLock()
	if p.secLevelKey == key && now.Before(p.secLevelExp) {
		lvl := p.secLevel
		p.metaMu.RUnlock()
		return lvl, nil
	}
	p.metaMu.RUnlock()

	res, err := p.doHubRequest(ctx, http.MethodPost, "/code", nil, map[string]string{
		"token": key,
	}, false, 6*time.Second)
	if err != nil {
		return -1, err
	}
	if res.Status < 200 || res.Status >= 300 {
		return -1, fmt.Errorf("hub /code returned HTTP %d", res.Status)
	}
	var codeResp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(res.Body, &codeResp); err != nil || strings.TrimSpace(codeResp.Code) == "" {
		return -1, errors.New("hub /code did not return backend code")
	}
	lvlRes, err := p.doHubRequest(ctx, http.MethodGet, "/backend/"+url.PathEscape(codeResp.Code)+"/security/level", nil, nil, false, 6*time.Second)
	if err != nil {
		return -1, err
	}
	if lvlRes.Status < 200 || lvlRes.Status >= 300 {
		return -1, fmt.Errorf("hub security level returned HTTP %d", lvlRes.Status)
	}
	trimmed := strings.TrimSpace(string(lvlRes.Body))
	lvl := -1
	if n, err := strconv.Atoi(trimmed); err == nil {
		lvl = n
	} else {
		var parsed int
		if err := json.Unmarshal(lvlRes.Body, &parsed); err == nil {
			lvl = parsed
		}
	}
	if lvl < 0 {
		return -1, fmt.Errorf("unexpected security level body %q", truncateStr(trimmed, 40))
	}
	p.metaMu.Lock()
	p.secLevel = lvl
	p.secLevelKey = key
	p.secLevelExp = p.nowFunc().Add(hubMetaCacheTTL)
	p.metaMu.Unlock()
	return lvl, nil
}

func (p *MUAUnionPlugin) checkBlacklistAllowed(ctx context.Context, email string) (bool, error) {
	normEmail := strings.ToLower(strings.TrimSpace(email))
	if normEmail == "" {
		return true, nil
	}
	now := p.nowFunc()
	p.blacklistMu.RLock()
	if ent, ok := p.blacklistCache[normEmail]; ok && now.Before(ent.exp) {
		p.blacklistMu.RUnlock()
		return ent.allowed, nil
	}
	p.blacklistMu.RUnlock()

	q := url.Values{}
	q.Set("email", normEmail)
	// Fit comfortably inside SkinLoom's 2s InvokeGuard deadline.
	res, err := p.doHubRequest(ctx, http.MethodGet, "/blacklist/checkallowed", q, nil, true, 1500*time.Millisecond)
	if err != nil {
		return true, err
	}
	if res.Status < 200 || res.Status >= 300 {
		return true, fmt.Errorf("hub /blacklist/checkallowed returned HTTP %d", res.Status)
	}
	var allowed bool
	parsedOK := false
	if err := json.Unmarshal(bytes.TrimSpace(res.Body), &allowed); err == nil {
		parsedOK = true
	} else {
		trimmed := strings.ToLower(strings.TrimSpace(string(res.Body)))
		if trimmed == "true" || trimmed == "1" {
			allowed = true
			parsedOK = true
		} else if trimmed == "false" || trimmed == "0" {
			allowed = false
			parsedOK = true
		}
	}
	if !parsedOK {
		return true, fmt.Errorf("unexpected /blacklist/checkallowed body %q", truncateStr(strings.TrimSpace(string(res.Body)), 40))
	}
	p.blacklistMu.Lock()
	if p.blacklistCache == nil {
		p.blacklistCache = make(map[string]blacklistCacheEntry)
	}
	if len(p.blacklistCache) >= maxBlacklistCacheItems {
		clear(p.blacklistCache)
	}
	p.blacklistCache[normEmail] = blacklistCacheEntry{
		allowed: allowed,
		exp:     p.nowFunc().Add(blacklistCacheTTL),
	}
	p.blacklistMu.Unlock()
	return allowed, nil
}
