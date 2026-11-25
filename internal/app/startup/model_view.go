package startup

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// modelViewFor snapshots a profile's default model transport and provider
// endpoints into the router view. Startup construction and the hot-apply
// rotation both call it, so the two views are identical by construction.
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
	}
}
