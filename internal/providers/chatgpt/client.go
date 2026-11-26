package chatgpt

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// StaticModels is the subscription catalog used when the live listing is
// unavailable. Windows match Codex defaults.
func StaticModels() []llm.ModelEntry {
	return []llm.ModelEntry{
		{ID: "gpt-5.5", Window: 272000},
		{ID: "gpt-5.4", Window: 272000},
		{ID: "gpt-5.4-mini", Window: 272000},
		{ID: "gpt-5.3-codex-spark", Window: 272000},
	}
}

// Client authenticates with the ChatGPT subscription: it resolves the OAuth
// bearer per request (refreshing when expired) and delegates to the
// OpenAI-compatible gateway client. The refresh token is single-use; the
// store is the source of truth and concurrent refreshes coalesce.
type Client struct {
	baseURL    string
	virtualKey string
	httpClient *http.Client
	store      *Store
	provider   string
}

// NewClient binds a subscription client to one profile provider.
func NewClient(baseURL, profileRoot, provider, virtualKey string, httpClient *http.Client) (*Client, error) {
	store, err := NewStore(profileRoot, provider)
	if err != nil {
		return nil, err
	}
	api := strings.TrimSpace(baseURL)
	if api == "" {
		api = DefaultAPI
	}
	return &Client{baseURL: api, virtualKey: strings.TrimSpace(virtualKey), httpClient: httpClient, store: store, provider: provider}, nil
}

// ensureToken loads the credential and refreshes it when expired.
func (c *Client) ensureToken(ctx context.Context) (TokenSet, error) {
	tokens, ok, err := c.store.Load(ctx)
	if err != nil {
		return TokenSet{}, err
	}
	if !ok {
		return TokenSet{}, errors.New("chatgpt subscription is not authorized")
	}
	if !Expired(tokens, time.Now()) {
		return tokens, nil
	}
	if strings.TrimSpace(tokens.RefreshToken) == "" {
		return tokens, nil
	}
	key := c.store.dir + "\x00" + c.provider
	refreshMu.Lock()
	if call, dup := refreshPromise[key]; dup {
		refreshMu.Unlock()
		select {
		case <-ctx.Done():
			return TokenSet{}, ctx.Err()
		case <-call.done:
			if call.err != nil {
				return TokenSet{}, call.err
			}
		}
		tokens, ok, err := c.store.Load(ctx)
		if err != nil {
			return TokenSet{}, err
		}
		if !ok {
			return TokenSet{}, errors.New("chatgpt subscription is not authorized")
		}
		return tokens, nil
	}
	call := &refreshCall{done: make(chan struct{})}
	refreshPromise[key] = call
	refreshMu.Unlock()
	call.err = func() error {
		current, ok, err := c.store.Load(ctx)
		if err != nil || !ok {
			return err
		}
		if !Expired(current, time.Now()) {
			return nil
		}
		resp, err := RefreshTokens(ctx, c.httpClient, current.RefreshToken)
		if err != nil {
			return err
		}
		next := TokenSet{AccessToken: resp.AccessToken, RefreshToken: resp.RefreshToken, AccountID: current.AccountID}
		if strings.TrimSpace(next.RefreshToken) == "" {
			next.RefreshToken = current.RefreshToken
		}
		if id := AccountIDFromIDToken(resp.IDToken); id != "" {
			next.AccountID = id
		}
		if resp.ExpiresIn > 0 {
			next.ExpiresAt = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).Unix()
		} else {
			next.ExpiresAt = current.ExpiresAt
		}
		return c.store.Save(ctx, next)
	}()
	close(call.done)
	refreshMu.Lock()
	delete(refreshPromise, key)
	refreshMu.Unlock()
	if call.err != nil {
		return TokenSet{}, call.err
	}
	tokens, ok, loadErr := c.store.Load(ctx)
	if loadErr != nil {
		return TokenSet{}, loadErr
	}
	if !ok {
		return TokenSet{}, errors.New("chatgpt subscription is not authorized")
	}
	return tokens, nil
}

// gateway builds the underlying client for one token.
func (c *Client) gateway(tokens TokenSet) *llm.GatewayClient {
	g := llm.NewGatewayClient(c.baseURL, tokens.AccessToken, c.virtualKey, nil)
	g.AccountID = strings.TrimSpace(tokens.AccountID)
	if c.httpClient != nil {
		g.HTTPClient = c.httpClient
	}
	return g
}

// Chat implements llm.Chatter.
func (c *Client) Chat(ctx context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	tokens, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.gateway(tokens).Chat(ctx, req, cb)
	if err == nil {
		return resp, nil
	}
	if !isUnauthorized(err) || strings.TrimSpace(tokens.RefreshToken) == "" {
		return nil, err
	}
	if err := c.store.Clear(ctx); err != nil {
		return nil, err
	}
	refreshed, rerr := c.ensureToken(ctx)
	if rerr != nil {
		return nil, err
	}
	return c.gateway(refreshed).Chat(ctx, req, cb)
}

// ListModels tries the live listing with subscription auth and falls back to
// the static catalog so /model stays usable offline.
func (c *Client) ListModels(ctx context.Context) ([]llm.ModelEntry, error) {
	tokens, err := c.ensureToken(ctx)
	if err != nil {
		return StaticModels(), nil
	}
	models, err := c.gateway(tokens).ListModels(ctx)
	if err != nil || len(models) == 0 {
		return StaticModels(), nil
	}
	return models, nil
}

func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *llm.ChatHTTPError
	switch {
	case errors.As(err, &httpErr):
		return httpErr.StatusCode == http.StatusUnauthorized
	default:
		return strings.Contains(strings.ToLower(err.Error()), "401")
	}
}
