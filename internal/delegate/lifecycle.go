package delegate

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Lifecycle helpers: configuration accessors, model resolution, and the
// manager start/stop contract. Delivery loops live in delivery.go, the stall
// monitor in monitor.go, and the tool entry point in manager.go.

func (m *Manager) maxIterations() int {
	if m == nil || m.cfg == nil || m.cfg.DelegationMaxIterations <= 0 {
		return 250
	}
	return m.cfg.DelegationMaxIterations
}

func (m *Manager) maxConcurrent() int {
	if m == nil || m.cfg == nil || m.cfg.DelegationMaxConcurrentChildren <= 0 {
		return 10
	}
	return m.cfg.DelegationMaxConcurrentChildren
}

func (m *Manager) maxDepth() int {
	if m == nil || m.cfg == nil || m.cfg.DelegationMaxSpawnDepth <= 0 {
		return 1
	}
	return m.cfg.DelegationMaxSpawnDepth
}

func (m *Manager) orchestratorOn() bool {
	if m == nil || m.cfg == nil {
		return true
	}
	return m.cfg.DelegationOrchestratorEnabled
}

// resolveTarget selects the model, provider, endpoint, and chat client for a
// child turn. An explicit delegation model wins, otherwise the parent model
// is mirrored; same for the provider, where base_url overrides only the
// endpoint while keeping that provider's key — so an explicit endpoint never
// silently inherits another profile's credentials.
func (m *Manager) resolveTarget() (model, provider, url string, client llm.Chatter, err error) {
	cfg := m.cfg
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
	if name := strings.TrimSpace(cfg.DelegationProvider); name != "" {
		ep, ok := cfg.ProviderEndpoints[name]
		if !ok {
			return "", "", "", nil, fmt.Errorf("delegate: provider %q has no usable endpoint", name)
		}
		if strings.TrimSpace(ep.URL) == "" {
			return "", "", "", nil, fmt.Errorf("delegate: provider %q has no usable endpoint", name)
		}
		url = ep.URL
		key = strings.TrimSpace(ep.APIKey)
		if key == "" {
			return "", "", "", nil, fmt.Errorf("delegate: provider %q has no credential configured", name)
		}
	}
	if base := strings.TrimSpace(cfg.DelegationBaseURL); base != "" {
		url = base
	}
	if model == "" {
		return "", "", "", nil, fmt.Errorf("delegate: no model configured")
	}
	if strings.TrimSpace(url) == "" {
		return "", "", "", nil, fmt.Errorf("delegate: no endpoint configured")
	}
	if m.parentClient != nil && url == strings.TrimSpace(cfg.LLMGatewayURL) && key == strings.TrimSpace(cfg.LLMGatewayAPIKey) {
		return model, provider, url, m.parentClient, nil
	}
	if key == "" {
		return "", "", "", nil, fmt.Errorf("delegate: no credential configured")
	}
	cacheKey := url + "\x00" + key
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()
	if client, ok := m.clients[cacheKey]; ok {
		return model, provider, url, client, nil
	}
	client = m.newClient(url, key, m.vkey, m.log)
	if client == nil {
		return "", "", "", nil, fmt.Errorf("delegate: client construction failed")
	}
	m.clients[cacheKey] = client
	return model, provider, url, client, nil
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
