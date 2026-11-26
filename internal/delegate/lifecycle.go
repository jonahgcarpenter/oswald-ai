package delegate

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Lifecycle helpers: configuration accessors, model resolution, and the
// manager start/stop contract. Delivery loops live in delivery.go, the stall
// monitor in monitor.go, and the tool entry point in manager.go.

// SwapConfig replaces the live configuration the manager reads for delegation
// bounds and model routing after a hot reload. Construction-pinned fields
// (window, virtual key, parent client) stay pinned: they are restart-class and
// only change on a supervised rebuild. Nil-safe: a nil manager or config is a
// no-op, so a reload applier never needs a guard.
func (m *Manager) SwapConfig(next *config.Config) {
	if m == nil || next == nil {
		return
	}
	m.cfgMu.Lock()
	m.cfg = next
	m.cfgMu.Unlock()
}

// config returns the current live configuration snapshot. The returned pointer
// is immutable by contract (reload swaps it wholesale), so callers may read it
// without holding the lock past the call.
func (m *Manager) config() *config.Config {
	if m == nil {
		return nil
	}
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.cfg
}

func (m *Manager) maxIterations() int {
	cfg := m.config()
	if cfg == nil || cfg.DelegationMaxIterations <= 0 {
		return 250
	}
	return cfg.DelegationMaxIterations
}

func (m *Manager) maxConcurrent() int {
	cfg := m.config()
	if cfg == nil || cfg.DelegationMaxConcurrentChildren <= 0 {
		return 10
	}
	return cfg.DelegationMaxConcurrentChildren
}

func (m *Manager) maxDepth() int {
	cfg := m.config()
	if cfg == nil || cfg.DelegationMaxSpawnDepth <= 0 {
		return 1
	}
	return cfg.DelegationMaxSpawnDepth
}

func (m *Manager) orchestratorOn() bool {
	cfg := m.config()
	if cfg == nil {
		return true
	}
	return cfg.DelegationOrchestratorEnabled
}

// resolveTarget selects the model, provider, endpoint, and chat client for a
// child turn. Precedence:
//
//  1. Explicit delegation.model / delegation.provider / delegation.base_url
//     wins with unchanged rules: a named provider must exist and carry a
//     credential, and an explicit endpoint never silently inherits another
//     provider's credentials. Keyless endpoints are rejected here.
//  2. Otherwise the calling turn's resolved routing (requestctx.ParentModel)
//     is mirrored through this profile's provider endpoints, so pins and
//     channel overrides reach delegated children. A parent provider missing
//     from ProviderEndpoints is an error, never a silent fallback. Keyless
//     endpoints are valid here exactly like the router: an empty key caches
//     under its own cache key (path 1 rejects keyless instead).
//  3. Otherwise the parent's LLMGateway* fields are mirrored (default path).
func (m *Manager) resolveTarget(ctx context.Context) (model, provider, url string, client llm.Chatter, err error) {
	cfg := m.config()
	if cfg == nil {
		return "", "", "", nil, fmt.Errorf("delegate: no configuration available")
	}
	if explicitDelegationRouting(cfg) {
		return m.resolveExplicitTarget(cfg)
	}
	if parent, ok := requestctx.ParentModelFromContext(ctx); ok {
		resolvedModel, resolvedProvider, resolvedURL, resolvedClient, found, err := m.resolveParentTarget(cfg, parent)
		if err != nil || found {
			return resolvedModel, resolvedProvider, resolvedURL, resolvedClient, err
		}
	}
	return m.resolveDefaultTarget(cfg)
}

// explicitDelegationRouting reports whether any delegation.* routing override
// is set. Any one of them selects the explicit path, which keeps its
// historical provider/endpoint inheritance rules.
func explicitDelegationRouting(cfg *config.Config) bool {
	return strings.TrimSpace(cfg.DelegationModel) != "" ||
		strings.TrimSpace(cfg.DelegationProvider) != "" ||
		strings.TrimSpace(cfg.DelegationBaseURL) != ""
}

// resolveExplicitTarget applies the explicit delegation overrides: model and
// provider fall back to the gateway fields when unset, a named provider
// supplies its own endpoint and key, and base_url overrides only the endpoint
// while keeping that provider's key. Subscription providers (chatgpt-oauth,
// claude-sdk) need no static key; their clients resolve credentials at
// request time from the subscription session.
func (m *Manager) resolveExplicitTarget(cfg *config.Config) (model, provider, url string, client llm.Chatter, err error) {
	model = strings.TrimSpace(cfg.DelegationModel)
	if model == "" {
		model = strings.TrimSpace(cfg.LLMGatewayModel)
	}
	provider = strings.TrimSpace(cfg.DelegationProvider)
	if provider == "" {
		provider = strings.TrimSpace(cfg.LLMGatewayProvider)
	}
	url = strings.TrimSpace(cfg.LLMGatewayURL)
	key := strings.TrimSpace(cfg.LLMGatewayAPIKey)
	authKind := strings.TrimSpace(cfg.LLMGatewayAuthKind)
	if name := strings.TrimSpace(cfg.DelegationProvider); name != "" {
		ep, ok := cfg.ProviderEndpoints[name]
		if !ok {
			return "", "", "", nil, fmt.Errorf("delegate: provider %q has no usable endpoint", name)
		}
		if strings.TrimSpace(ep.AuthKind) == config.ProviderAuthClaudeSDK {
			url = strings.TrimSpace(ep.URL)
			key = ""
			authKind = ep.AuthKind
		} else {
			if strings.TrimSpace(ep.URL) == "" {
				return "", "", "", nil, fmt.Errorf("delegate: provider %q has no usable endpoint", name)
			}
			url = ep.URL
			key = strings.TrimSpace(ep.APIKey)
			authKind = strings.TrimSpace(ep.AuthKind)
			if key == "" && authKind != config.ProviderAuthChatGPT && authKind != config.ProviderAuthClaudeSDK {
				return "", "", "", nil, fmt.Errorf("delegate: provider %q has no credential configured", name)
			}
		}
	}
	if base := strings.TrimSpace(cfg.DelegationBaseURL); base != "" {
		url = base
	}
	if model == "" {
		return "", "", "", nil, fmt.Errorf("delegate: no model configured")
	}
	if strings.TrimSpace(url) == "" && authKind != config.ProviderAuthClaudeSDK {
		return "", "", "", nil, fmt.Errorf("delegate: no endpoint configured")
	}
	if key == "" && authKind != config.ProviderAuthChatGPT && authKind != config.ProviderAuthClaudeSDK && !m.reusesParentTransport(cfg, url, key) {
		return "", "", "", nil, fmt.Errorf("delegate: no credential configured")
	}
	client, err = m.clientFor(cfg, provider, config.ProviderEndpoint{URL: url, APIKey: key, AuthKind: authKind})
	if err != nil {
		return "", "", "", nil, err
	}
	return model, provider, url, client, nil
}

// resolveParentTarget mirrors the calling turn's resolved routing through the
// delegate's own provider map. ok=false means the parent model was empty after
// trim, so the caller falls through to the default path; an unknown provider
// or unusable endpoint is an error so a pinned turn never silently runs
// somewhere else. Subscription endpoints resolve without a static key.
func (m *Manager) resolveParentTarget(cfg *config.Config, parent requestctx.ParentModel) (model, provider, url string, client llm.Chatter, ok bool, err error) {
	model = strings.TrimSpace(parent.Model)
	provider = strings.TrimSpace(parent.Provider)
	if model == "" || provider == "" {
		return "", "", "", nil, false, nil
	}
	endpoint, found := cfg.ProviderEndpoints[provider]
	if !found {
		return "", "", "", nil, false, fmt.Errorf("delegate: provider %q has no usable endpoint", provider)
	}
	if strings.TrimSpace(endpoint.URL) == "" && strings.TrimSpace(endpoint.AuthKind) != config.ProviderAuthClaudeSDK {
		return "", "", "", nil, false, fmt.Errorf("delegate: provider %q has no usable endpoint", provider)
	}
	client, err = m.clientFor(cfg, provider, config.ProviderEndpoint{URL: strings.TrimSpace(endpoint.URL), APIKey: strings.TrimSpace(endpoint.APIKey), AuthKind: strings.TrimSpace(endpoint.AuthKind)})
	if err != nil {
		return "", "", "", nil, false, err
	}
	return model, provider, strings.TrimSpace(endpoint.URL), client, true, nil
}

// resolveDefaultTarget mirrors the parent's gateway fields, the pre-router
// behavior. Keyless endpoints are rejected unless the transport is the
// construction-pinned parent client or a subscription transport.
func (m *Manager) resolveDefaultTarget(cfg *config.Config) (model, provider, url string, client llm.Chatter, err error) {
	model = strings.TrimSpace(cfg.LLMGatewayModel)
	provider = strings.TrimSpace(cfg.LLMGatewayProvider)
	url = strings.TrimSpace(cfg.LLMGatewayURL)
	key := strings.TrimSpace(cfg.LLMGatewayAPIKey)
	authKind := strings.TrimSpace(cfg.LLMGatewayAuthKind)
	if model == "" {
		return "", "", "", nil, fmt.Errorf("delegate: no model configured")
	}
	if strings.TrimSpace(url) == "" && authKind != config.ProviderAuthClaudeSDK {
		return "", "", "", nil, fmt.Errorf("delegate: no endpoint configured")
	}
	if key == "" && authKind != config.ProviderAuthChatGPT && authKind != config.ProviderAuthClaudeSDK && !m.reusesParentTransport(cfg, url, key) {
		return "", "", "", nil, fmt.Errorf("delegate: no credential configured")
	}
	client, err = m.clientFor(cfg, provider, config.ProviderEndpoint{URL: url, APIKey: key, AuthKind: authKind})
	if err != nil {
		return "", "", "", nil, err
	}
	return model, provider, url, client, nil
}

// reusesParentTransport reports whether the endpoint is the construction-pinned
// parent transport, in which case the manager reuses that client instead of
// constructing another one.
func (m *Manager) reusesParentTransport(cfg *config.Config, url, key string) bool {
	return m.parentClient != nil && cfg != nil &&
		url == strings.TrimSpace(cfg.LLMGatewayURL) && key == strings.TrimSpace(cfg.LLMGatewayAPIKey)
}

// clientFor returns the pinned parent client for the parent transport, else a
// client cached by provider, URL, key, and auth kind. Subscription providers
// with the same URL stay distinct by provider name so token stores never mix.
func (m *Manager) clientFor(cfg *config.Config, provider string, endpoint config.ProviderEndpoint) (llm.Chatter, error) {
	url := strings.TrimSpace(endpoint.URL)
	key := strings.TrimSpace(endpoint.APIKey)
	if m.reusesParentTransport(cfg, url, key) {
		return m.parentClient, nil
	}
	cacheKey := provider + "\x00" + url + "\x00" + key + "\x00" + strings.TrimSpace(endpoint.AuthKind)
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()
	if client, ok := m.clients[cacheKey]; ok {
		return client, nil
	}
	client := m.newClient(provider, config.ProviderEndpoint{URL: url, APIKey: key, AuthKind: strings.TrimSpace(endpoint.AuthKind)}, m.vkey, m.log)
	if client == nil {
		return nil, fmt.Errorf("delegate: client construction failed")
	}
	m.clients[cacheKey] = client
	return client, nil
}

// enabledToolsFromContext reads the calling turn's toolset filter for child
// inheritance. Absent means unrestricted, matching the parent default.
func enabledToolsFromContext(ctx context.Context) []string {
	if scope, ok := requestctx.ParentToolScopeFromContext(ctx); ok {
		return append([]string(nil), scope.Toolsets...)
	}
	return nil
}

// mcpServersFromContext reads the calling turn's MCP server allowlist for
// child inheritance. Absent means unrestricted, matching the parent default.
func mcpServersFromContext(ctx context.Context) []string {
	if scope, ok := requestctx.ParentToolScopeFromContext(ctx); ok {
		return append([]string(nil), scope.MCPServers...)
	}
	return nil
}
