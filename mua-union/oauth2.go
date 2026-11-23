package main

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	plugin "github.com/Entropy-Increase-Team/skinloom/sdk/go"
)

const (
	unionOAuth2TokenTTLSeconds = 600 // 10 minutes (exact match with UnionOAuth2Controller)
	haCacheTTL                 = 60 * time.Second
)

func (p *MUAUnionPlugin) registerUnionOAuth2Routes(ctx *plugin.Context) {
	ctx.HTTP.Get("/api/union/member/oauth2", p.handleUnionOAuth2PublicKey,
		plugin.WithAuth(plugin.AuthPublic),
		plugin.WithOpenCORS(),
	)
	// Grant endpoint requires browser session cookie; do NOT set WebhookNoCSRF so host populates req.User.
	ctx.HTTP.Get("/api/union/member/oauth2/grant", p.handleUnionOAuth2Grant,
		plugin.WithAuth(plugin.AuthPublic),
	)
}

func (p *MUAUnionPlugin) ensureOAuth2KeyPair(ctx context.Context) (*rsa.PrivateKey, string, error) {
	p.oauth2KeyMu.Lock()
	defer p.oauth2KeyMu.Unlock()

	now := p.nowFunc()
	if p.oauth2Priv != nil && p.oauth2PubPEM != "" && now.Before(p.oauth2KeyExp) {
		return p.oauth2Priv, p.oauth2PubPEM, nil
	}

	if priv, pubPEM, ok := p.loadOAuth2KeyPairFromDBLocked(ctx); ok {
		p.oauth2Priv = priv
		p.oauth2PubPEM = pubPEM
		p.oauth2KeyExp = now.Add(haCacheTTL)
		return priv, pubPEM, nil
	}

	// Cold start: generate a 4096-bit keypair and insert-if-absent so concurrent HA
	// nodes converge atomically on a single winning private key (public key is always
	// derived from the private key, preventing split-brain keypairs).
	privPEM, pubPEM, err := generateRSA4096KeyPairPEM()
	if err != nil {
		return nil, "", err
	}
	inserted, err := insertKVIfAbsent(ctx, p.ctx.DB(), kvOAuth2PrivKey, privPEM, now.Unix())
	if err != nil {
		return nil, "", err
	}
	if !inserted {
		if priv, derivedPub, ok := p.loadOAuth2KeyPairFromDBLocked(ctx); ok {
			p.oauth2Priv = priv
			p.oauth2PubPEM = derivedPub
			p.oauth2KeyExp = now.Add(haCacheTTL)
			return priv, derivedPub, nil
		}
		// Existing row was corrupt; replace it.
		if err := setKV(ctx, p.ctx.DB(), kvOAuth2PrivKey, privPEM); err != nil {
			return nil, "", err
		}
	}
	priv, err := parseRSAPrivateKeyPEM(privPEM)
	if err != nil {
		return nil, "", err
	}
	p.oauth2Priv = priv
	p.oauth2PubPEM = pubPEM
	p.oauth2KeyExp = now.Add(haCacheTTL)
	return priv, pubPEM, nil
}

func (p *MUAUnionPlugin) loadOAuth2KeyPairFromDBLocked(ctx context.Context) (*rsa.PrivateKey, string, bool) {
	privPEM, hasPriv, err := getKV(ctx, p.ctx.DB(), kvOAuth2PrivKey)
	if err != nil || !hasPriv || strings.TrimSpace(privPEM) == "" {
		return nil, "", false
	}
	priv, err := parseRSAPrivateKeyPEM(privPEM)
	if err != nil {
		return nil, "", false
	}
	pubPEM, err := derivePublicKeyPEM(priv)
	if err != nil {
		return nil, "", false
	}
	return priv, pubPEM, true
}

func (p *MUAUnionPlugin) regenerateOAuth2KeyPair(ctx context.Context) (string, error) {
	p.oauth2KeyMu.Lock()
	defer p.oauth2KeyMu.Unlock()
	_, pubPEM, err := p.regenerateOAuth2KeyPairLocked(ctx)
	return pubPEM, err
}

func (p *MUAUnionPlugin) regenerateOAuth2KeyPairLocked(ctx context.Context) (*rsa.PrivateKey, string, error) {
	privPEM, pubPEM, err := generateRSA4096KeyPairPEM()
	if err != nil {
		return nil, "", err
	}
	priv, err := parseRSAPrivateKeyPEM(privPEM)
	if err != nil {
		return nil, "", err
	}
	if err := setKV(ctx, p.ctx.DB(), kvOAuth2PrivKey, privPEM); err != nil {
		return nil, "", err
	}
	// Clean up any legacy separate public key row so private key is the sole source of truth.
	_ = deleteKV(ctx, p.ctx.DB(), kvOAuth2PubKey)
	p.oauth2Priv = priv
	p.oauth2PubPEM = pubPEM
	p.oauth2KeyExp = p.nowFunc().Add(haCacheTTL)
	return priv, pubPEM, nil
}

func (p *MUAUnionPlugin) currentOAuth2PublicKeyPEM(ctx context.Context) string {
	p.oauth2KeyMu.Lock()
	defer p.oauth2KeyMu.Unlock()
	now := p.nowFunc()
	if p.oauth2PubPEM != "" && now.Before(p.oauth2KeyExp) {
		return p.oauth2PubPEM
	}
	if priv, pubPEM, ok := p.loadOAuth2KeyPairFromDBLocked(ctx); ok {
		p.oauth2Priv = priv
		p.oauth2PubPEM = pubPEM
		p.oauth2KeyExp = now.Add(haCacheTTL)
		return pubPEM
	}
	return ""
}

func (p *MUAUnionPlugin) handleUnionOAuth2PublicKey(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	if !p.ctx.Settings.GetBool("enableUnionOAuth2") {
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Union OAuth2 is not enabled on this server.",
		}), nil
	}
	_, pubPEM, err := p.ensureOAuth2KeyPair(reqCtx)
	if err != nil {
		p.ctx.Log.Error(fmt.Sprintf("ensure Union OAuth2 keypair failed: %v", err))
		return plugin.JSON(http.StatusInternalServerError, map[string]any{
			"error": "failed to initialize Union OAuth2 signing key",
		}), nil
	}
	return plugin.JSON(http.StatusOK, map[string]string{
		"signaturePublicKey": pubPEM,
	}), nil
}

func (p *MUAUnionPlugin) handleUnionOAuth2Grant(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
	if !p.ctx.Settings.GetBool("enableUnionOAuth2") {
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Union OAuth2 is not enabled on this server.",
		}), nil
	}

	// If not signed in, redirect browser to SkinLoom login and bounce right back here.
	if req.User == nil || !req.User.Authenticated || req.User.UserID <= 0 {
		nextPath := "/api/union/member/oauth2/grant"
		if len(req.Query) > 0 {
			q := url.Values{}
			for k, v := range req.Query {
				q.Set(k, v)
			}
			nextPath += "?" + q.Encode()
		}
		return plugin.Redirect(http.StatusFound, "/auth/login?next="+url.QueryEscape(nextPath)), nil
	}

	root, err := p.unionAPIRoot()
	if err != nil {
		return plugin.JSON(http.StatusInternalServerError, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Union API root is misconfigured.",
		}), nil
	}
	memberKey := p.effectiveMemberKey(reqCtx)
	if memberKey == "" {
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Union member key is not configured.",
		}), nil
	}

	privKey, _, err := p.ensureOAuth2KeyPair(reqCtx)
	if err != nil {
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "The signature key of this site is invalid. Please contact the site manager.",
		}), nil
	}

	unionPubKey, err := p.fetchOAuth2BackendPubKey(reqCtx)
	if err != nil {
		p.ctx.Log.Warn(fmt.Sprintf("Union OAuth2 backend pubkey fetch failed: %v", err))
		return plugin.JSON(http.StatusBadGateway, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Union server down.",
		}), nil
	}

	// Fetch authoritative user profile to verify active status, email verification (SL3),
	// and guarantee up-to-date email & nickname. Never fall back to unverified session fields.
	uid := req.User.UserID
	u, uErr := p.ctx.Users.Get(reqCtx, uid)
	if uErr != nil || u == nil {
		p.ctx.Log.Warn(fmt.Sprintf("Union OAuth2 user lookup failed for uid %d: %v", uid, uErr))
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Unable to verify account status.",
		}), nil
	}
	if u.Status != "" && !strings.EqualFold(u.Status, "active") {
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Your account is not active.",
		}), nil
	}
	if pubSite, sErr := p.ctx.Site.Public(reqCtx); sErr == nil {
		if reqVerify, ok := pubSite["auth.require_email_verification"].(bool); ok && reqVerify && !u.EmailVerified {
			return plugin.JSON(http.StatusForbidden, map[string]any{
				"error":        "ForbiddenOperationException",
				"errorMessage": "Please verify your email address before signing in with MUA Union OAuth2.",
			}), nil
		}
	}
	nickname := strings.TrimSpace(u.Nickname)
	if nickname == "" {
		nickname = req.User.Nickname
	}
	email := strings.TrimSpace(u.Email)
	if email == "" {
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Account email is unavailable.",
		}), nil
	}

	userInfoJSON, err := json.Marshal(map[string]any{
		"uid":        uid,
		"nickname":   nickname,
		"email":      email,
		"expires_at": p.nowFunc().Unix() + unionOAuth2TokenTTLSeconds,
	})
	if err != nil {
		return nil, errors.New("marshal userInfo failed")
	}
	userInfoB64 := base64.StdEncoding.EncodeToString(userInfoJSON)

	macHex, sigB64, err := signUnionOAuth2Token(userInfoB64, memberKey, privKey)
	if err != nil {
		p.ctx.Log.Error(fmt.Sprintf("Union OAuth2 sign failed: %v", err))
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Failed to sign the token.",
		}), nil
	}

	tokenJSON, err := json.Marshal(map[string]string{
		"userInfo":  userInfoB64,
		"mac":       macHex,
		"signature": sigB64,
	})
	if err != nil {
		return nil, err
	}

	encryptedToken, err := encryptChunkedPKCS1v15(tokenJSON, unionPubKey)
	if err != nil {
		p.ctx.Log.Error(fmt.Sprintf("Union OAuth2 encrypt failed: %v", err))
		return plugin.JSON(http.StatusForbidden, map[string]any{
			"error":        "ForbiddenOperationException",
			"errorMessage": "Failed to encrypt the token.",
		}), nil
	}

	params := url.Values{}
	for k, v := range req.Query {
		if k == "userInfoToken" {
			continue
		}
		params.Set(k, v)
	}
	params.Set("userInfoToken", encryptedToken)

	redirectURI := root + "/oauth2/continue?" + params.Encode()
	resp := plugin.Redirect(http.StatusFound, redirectURI)
	resp.Headers["Cache-Control"] = "no-store, no-cache"
	resp.Headers["Pragma"] = "no-cache"
	resp.Headers["Referrer-Policy"] = "no-referrer"
	return resp, nil
}
