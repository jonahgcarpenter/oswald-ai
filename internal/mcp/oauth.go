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
		if adopted, ok, err := h.store.BindIssuerIfEmpty(ctx, h.currentIssuer(ctx)); err != nil {
			return nil, err
		} else if ok {
			tokens = adopted
		} else {
			return nil, h.needAuthError()
		}
	}
	meta, err := h.metadata(ctx)
	if err != nil {
		return nil, err
	}
	if !issuerMatches(tokens.Issuer, meta.Issuer) {
		// Issuer drift fails closed: the grant is bound to its issuer and
		// must never be sent elsewhere. Stored files are kept intact (a
		// refresh is never POSTed to the new issuer); the operator
		// re-authorizes to adopt the new issuer.
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

// currentIssuer returns the cached discovery issuer without network use,
// or "" when nothing is cached yet.
func (h *oauthRuntime) currentIssuer(ctx context.Context) string {
	meta, ok, err := h.store.LoadMetadata(ctx)
	if err != nil || !ok {
		return ""
	}
	return meta.Issuer
}

// Authorize runs after a 401 or 403: it refreshes once and lets the
// transport retry. Anything else fails closed into a re-login prompt.
// Concurrent recoveries on an already-rotated token are deduped: when the
// failed request's bearer no longer matches the stored access token, a peer
// already refreshed and the retry can proceed without another POST.
func (h *oauthRuntime) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return fmt.Errorf("MCP server %q rejected the request", h.serverName)
	}
	// A later insufficient_scope challenge names scopes beyond the stored
	// grant. The service never starts an interactive flow; fail closed into
	// a re-login prompt so the next `oswald mcp auth` run can request the
	// union of the stored and challenged scopes. Scope values are never
	// logged. Anything else keeps the single-refresh retry path below.
	challengeScope := extractChallengeScope(resp)
	challengeErr := parseWWWAuthenticateField(resp, "error")
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
	if !issuerMatches(tokens.Issuer, meta.Issuer) {
		return h.needAuthError()
	}
	if strings.EqualFold(challengeErr, "insufficient_scope") && !challengeScopesCovered(tokens.Scopes, challengeScope) {
		return h.needAuthError()
	}
	if failed := bearerToken(req); failed != "" && failed != tokens.AccessToken {
		return nil
	}
	if _, err := h.refresh(ctx, meta, tokens); err != nil {
		return h.needAuthError()
	}
	return nil
}

// bearerToken extracts the bearer credential of the failed request, if any.
func bearerToken(req *http.Request) string {
	if req == nil {
		return ""
	}
	header := req.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
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
// A dead dynamic registration (invalid_client) is discarded so the next CLI
// flow re-registers; operator clients never auto-heal.
func (h *oauthRuntime) refresh(ctx context.Context, meta *serverMetadata, tokens TokenSet) (*TokenSet, error) {
	if tokens.RefreshToken == "" {
		return nil, h.needAuthError()
	}
	client, err := h.storedClient(ctx, meta, tokens)
	if err != nil {
		return nil, err
	}
	scopes := h.oauth.Scopes
	if len(scopes) == 0 {
		scopes = tokens.Scopes
	}
	refreshed, err := refreshOAuthToken(ctx, h.store, meta, client, scopes, oauthResource(meta, h.mcpURL), tokens.RefreshToken, nil)
	if err != nil {
		if isInvalidClient(err) && client.Method != clientMethodOperator {
			_ = h.store.ClearClient(ctx)
			_ = h.store.ClearTokens(ctx)
		}
		return nil, err
	}
	return refreshed, nil
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
