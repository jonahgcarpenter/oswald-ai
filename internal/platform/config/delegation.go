package config

import (
	"net/url"
	"sort"
	"strings"
)

// Delegation bounds. max_iterations caps one child turn; the parent's own
// governance backstops still apply underneath. max_concurrent_children bounds
// one batch's live workers; depth caps the orchestrator nesting tree.
const (
	defaultDelegationMaxIterations = 250
	defaultDelegationMaxChildren   = 10
	defaultDelegationMaxDepth      = 1

	maxDelegationIterations = 10000
	maxDelegationChildren   = 32
	maxDelegationDepth      = 8
)

// ProviderEndpoint is a resolved provider transport: the normalized endpoint
// URL plus the interpolated API key (empty when the provider declares none).
// Retained per provider so subagent dispatch can build a client for a
// non-default provider without re-reading operator files.
type ProviderEndpoint struct {
	URL    string
	APIKey string
}

// delegationYAML is the on-disk delegation section shape. Unknown keys are
// rejected by KnownFields decoding so typos fail closed.
type delegationYAML struct {
	Model               *string `yaml:"model"`
	Provider            *string `yaml:"provider"`
	BaseURL             *string `yaml:"base_url"`
	MaxIterations       *int    `yaml:"max_iterations"`
	MaxConcurrent       *int    `yaml:"max_concurrent_children"`
	MaxSpawnDepth       *int    `yaml:"max_spawn_depth"`
	OrchestratorEnabled *bool   `yaml:"orchestrator_enabled"`
}

// applyDelegation validates the delegation section into the effective config.
// Empty means mirror the parent model on the parent provider with defaults;
// every set leaf is validated here so dispatch never discovers bad config.
func applyDelegation(cfg *Config, doc documentYAML, providers map[string]providerYAML) error {
	raw := doc.Delegation
	if raw == nil {
		cfg.DelegationModel = ""
		cfg.DelegationProvider = ""
		cfg.DelegationBaseURL = ""
		cfg.DelegationMaxIterations = defaultDelegationMaxIterations
		cfg.DelegationMaxConcurrentChildren = defaultDelegationMaxChildren
		cfg.DelegationMaxSpawnDepth = defaultDelegationMaxDepth
		cfg.DelegationOrchestratorEnabled = true
		return nil
	}
	if raw.Model != nil {
		model := strings.TrimSpace(*raw.Model)
		if model == "" || len(model) > 256 || strings.ContainsAny(model, "\r\n\x00") {
			return configErr("config_delegation_invalid", "delegation.model")
		}
		cfg.DelegationModel = model
	} else {
		cfg.DelegationModel = ""
	}
	if raw.Provider != nil {
		name := strings.TrimSpace(*raw.Provider)
		if name == "" || len(name) > 128 || strings.ContainsAny(name, " \t\r\n\x00") {
			return configErr("config_delegation_invalid", "delegation.provider")
		}
		if _, ok := providers[name]; !ok {
			return configErr("config_delegation_invalid", "delegation.provider")
		}
		cfg.DelegationProvider = name
	} else {
		cfg.DelegationProvider = ""
	}
	if raw.BaseURL != nil {
		endpoint, err := url.Parse(strings.TrimSpace(*raw.BaseURL))
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return configErr("config_delegation_invalid", "delegation.base_url")
		}
		if strings.TrimSpace(*raw.BaseURL) == "" {
			return configErr("config_delegation_invalid", "delegation.base_url")
		}
		if cfg.DelegationProvider == "" {
			return configErr("config_delegation_invalid", "delegation.base_url")
		}
		cfg.DelegationBaseURL = strings.TrimRight(endpoint.String(), "/")
	} else {
		cfg.DelegationBaseURL = ""
	}
	if raw.MaxIterations != nil {
		if *raw.MaxIterations < 1 || *raw.MaxIterations > maxDelegationIterations {
			return configErr("config_delegation_invalid", "delegation.max_iterations")
		}
		cfg.DelegationMaxIterations = *raw.MaxIterations
	} else {
		cfg.DelegationMaxIterations = defaultDelegationMaxIterations
	}
	if raw.MaxConcurrent != nil {
		if *raw.MaxConcurrent < 1 || *raw.MaxConcurrent > maxDelegationChildren {
			return configErr("config_delegation_invalid", "delegation.max_concurrent_children")
		}
		cfg.DelegationMaxConcurrentChildren = *raw.MaxConcurrent
	} else {
		cfg.DelegationMaxConcurrentChildren = defaultDelegationMaxChildren
	}
	if raw.MaxSpawnDepth != nil {
		if *raw.MaxSpawnDepth < 1 || *raw.MaxSpawnDepth > maxDelegationDepth {
			return configErr("config_delegation_invalid", "delegation.max_spawn_depth")
		}
		cfg.DelegationMaxSpawnDepth = *raw.MaxSpawnDepth
	} else {
		cfg.DelegationMaxSpawnDepth = defaultDelegationMaxDepth
	}
	if raw.OrchestratorEnabled != nil {
		cfg.DelegationOrchestratorEnabled = *raw.OrchestratorEnabled
	} else {
		cfg.DelegationOrchestratorEnabled = true
	}
	return nil
}

// resolveProviderEndpoints retains every declared provider's transport so a
// delegate turn can dial a non-default provider at dispatch time. Resolution
// is best-effort and never fails validation: unusable entries are skipped
// (a delegation naming one fails closed at dispatch instead), so dormant
// providers keep today's load behavior exactly.
func resolveProviderEndpoints(providers map[string]providerYAML) map[string]ProviderEndpoint {
	out := map[string]ProviderEndpoint{}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		provider := providers[name]
		endpoint, err := url.Parse(strings.TrimSpace(provider.API))
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			continue
		}
		endpoint.Path = strings.TrimSuffix(strings.TrimRight(endpoint.Path, "/"), "/v1")
		key := ""
		if provider.Key != nil {
			key = *provider.Key
		}
		out[name] = ProviderEndpoint{URL: endpoint.String(), APIKey: key}
	}
	return out
}
