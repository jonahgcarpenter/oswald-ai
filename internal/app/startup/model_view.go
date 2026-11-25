package startup

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// modelViewFor snapshots a profile's default model transport, provider
// endpoints, and the global channel-override map into the router view. Startup
// construction and the hot-apply rotation both call it, so the two views are
// identical by construction. Overrides are global-only config, so every
// profile's view carries the same map.
func modelViewFor(profile *config.Config) agent.View {
	if profile == nil {
		return agent.View{}
	}
	return agent.View{
		Default: agent.DefaultTarget{
			Model:      profile.LLMGatewayModel,
			Provider:   profile.LLMGatewayProvider,
			URL:        profile.LLMGatewayURL,
			APIKey:     profile.LLMGatewayAPIKey,
			VirtualKey: profile.LLMGatewayVirtualKey,
		},
		Endpoints: profile.ProviderEndpoints,
		Overrides: channelOverrideView(profile.ChannelOverrides),
	}
}

// channelOverrideView deep-copies the config override map into the router
// view's immutable snapshot so a later config revision can never mutate a
// handed-off view.
func channelOverrideView(overrides map[string]map[string]config.ChannelOverride) map[string]map[string]agent.ChannelOverride {
	if len(overrides) == 0 {
		return nil
	}
	out := make(map[string]map[string]agent.ChannelOverride, len(overrides))
	for gateway, entries := range overrides {
		copied := make(map[string]agent.ChannelOverride, len(entries))
		for id, override := range entries {
			copied[id] = agent.ChannelOverride{Provider: override.Provider, Model: override.Model}
		}
		out[gateway] = copied
	}
	return out
}
