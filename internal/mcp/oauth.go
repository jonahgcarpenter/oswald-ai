package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// oauthRuntime is the service-side OAuthHandler for one configured MCP
// server. It serves stored credentials, refreshes them lazily, and never
// starts an interactive flow: without usable tokens it fails closed telling
// the operator to run `oswald mcp auth`.
type oauthRuntime struct {
	serverName string
	mcpURL     string
	oauth      *OAuthConfig
	store      *tokenStore
	mu         sync.Mutex
}

var _ sdkauth.OAuthHandler = (*oauthRuntime)(nil)

func newOAuthRuntime(profileRoot string, cfg ServerConfig) (*oauthRuntime, error) {
	if cfg.OAuth == nil {
		return nil, fmt.Errorf("server has no OAuth configuration")
	}
	oauth := *cfg.OAuth
	oauth.Scopes = append([]string(nil), cfg.OAuth.Scopes...)
	store, err := newTokenStore(profileRoot, cfg.Name)
	if err != nil {
		return nil, err
	}
	return &oauthRuntime{serverName: cfg.Name, mcpURL: cfg.URL, oauth: &oauth, store: store}, nil
}

func (h *oauthRuntime) needAuthError() error {
	return fmt.Errorf("MCP server %q requires OAuth login (run `oswald mcp auth %s`)", h.serverName, h.serverName)
}

// TokenSource returns a static source for the stored credential, refreshing
// it first when expired. The SDK calls this on every request; transport-level
// failures here fail the request without retry.
func (h *oauthRuntime) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tokens, ok, err := h.store.LoadTokens(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, h.needAuthError()
	}
	meta, err := h.metadata(ctx)
	if err != nil {
		return nil, err
	}
	if meta.Issuer != tokens.Issuer {
		return nil, h.needAuthError()
	}
	if tokens.Expiry.IsZero() || time.Now().Add(time.Minute).Before(tokens.Expiry) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tokens.AccessToken, TokenType: tokens.TokenType, RefreshToken: tokens.RefreshToken, Expiry: tokens.Expiry}), nil
	}
	refreshed, err := h.refresh(ctx, meta, tokens)
	if err != nil {
		return nil, err
	}
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: refreshed.AccessToken, TokenType: refreshed.TokenType, RefreshToken: refreshed.RefreshToken, Expiry: refreshed.Expiry}), nil
}

// Authorize runs after a 401 or 403: it refreshes once and lets the
// transport retry. Anything else fails closed into a re-login prompt.
func (h *oauthRuntime) Authorize(ctx context.Context, _ *http.Request, resp *http.Response) error {
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return fmt.Errorf("MCP server %q rejected the request", h.serverName)
	}
	tokens, ok, err := h.store.LoadTokens(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return h.needAuthError()
	}
	meta, err := h.metadata(ctx)
	if err != nil {
		return err
	}
	if meta.Issuer != tokens.Issuer {
		return h.needAuthError()
	}
	if _, err := h.refresh(ctx, meta, tokens); err != nil {
		return h.needAuthError()
	}
	return nil
}

func (h *oauthRuntime) metadata(ctx context.Context) (*serverMetadata, error) {
	if meta, ok, err := h.store.LoadMetadata(ctx); err != nil {
		return nil, err
	} else if ok {
		return &meta, nil
	}
	meta, err := discoverOAuthMetadata(ctx, h.mcpURL)
	if err != nil {
		return nil, err
	}
	if err := h.store.SaveMetadata(ctx, *meta); err != nil {
		return nil, err
	}
	return meta, nil
}

// refresh exchanges the stored refresh token and persists the result. The
// client credential comes from operator configuration or a previously
// registered dynamic client; the client ID must match the stored tokens.
func (h *oauthRuntime) refresh(ctx context.Context, meta *serverMetadata, tokens TokenSet) (*TokenSet, error) {
	if tokens.RefreshToken == "" {
		return nil, h.needAuthError()
	}
	client, err := h.storedClient(ctx, meta, tokens)
	if err != nil {
		return nil, err
	}
	return refreshOAuthToken(ctx, h.store, meta, client, h.oauth.Scopes, nil)
}

func (h *oauthRuntime) storedClient(ctx context.Context, meta *serverMetadata, tokens TokenSet) (*resolvedOAuthClient, error) {
	if id := strings.TrimSpace(h.oauth.ClientID); id != "" {
		if tokens.ClientID != "" && tokens.ClientID != id {
			return nil, h.needAuthError()
		}
		secret := h.oauth.ClientSecret
		return &resolvedOAuthClient{ClientID: id, ClientSecret: secret, Method: clientMethodOperator, AuthStyle: selectAuthStyle(meta.TokenEndpointAuthMethods, secret != "")}, nil
	}
	if record, ok, err := h.store.LoadClient(ctx); err != nil {
		return nil, err
	} else if ok {
		if tokens.ClientID != "" && tokens.ClientID != record.ClientID {
			return nil, h.needAuthError()
		}
		return &resolvedOAuthClient{ClientID: record.ClientID, ClientSecret: record.ClientSecret, RedirectURI: firstRedirectURI(record.RedirectURIs), Method: record.Method, AuthStyle: selectAuthStyle(meta.TokenEndpointAuthMethods, record.ClientSecret != "")}, nil
	}
	if tokens.ClientID != "" {
		return &resolvedOAuthClient{ClientID: tokens.ClientID, Method: clientMethodCIMD, AuthStyle: oauth2.AuthStyleInParams}, nil
	}
	return nil, h.needAuthError()
}

func firstRedirectURI(uris []string) string {
	if len(uris) > 0 {
		return uris[0]
	}
	return ""
}
