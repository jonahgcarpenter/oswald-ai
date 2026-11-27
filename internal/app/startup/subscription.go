package startup

import (
	"context"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm/api"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm/chatgpt"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm/claude"
)

// subscriptionClientFor builds the chat client for one provider endpoint.
// Key providers use the OpenAI-compatible gateway; chatgpt-oauth resolves a
// per-request bearer from the profile token store; claude-sdk shells to the
// installed CLI that owns the subscription login. Nil means unavailable so
// the router degrades the link instead of running the wrong transport.
func subscriptionClientFor(profileRoot, provider string, endpoint config.ProviderEndpoint, virtualKey string, log *config.Logger) llm.Chatter {
	switch endpoint.AuthKind {
	case config.ProviderAuthChatGPT:
		client, err := chatgpt.NewClient(endpoint.URL, profileRoot, provider, virtualKey, nil, log)
		if err != nil {
			return nil
		}
		return client
	case config.ProviderAuthClaudeSDK:
		client, err := claude.NewClient()
		if err != nil {
			return nil
		}
		return client
	default:
		return api.NewClient(endpoint.URL, endpoint.APIKey, virtualKey, log)
	}
}

// subscriptionListModels fetches one provider's listing. ChatGPT requires an
// authorized subscription and degrades otherwise; authorized providers fall
// back to the static catalog when the live listing fails. Claude stays on
// its static catalog gated on CLI login; key providers use the gateway.
func subscriptionListModels(ctx context.Context, profileRoot, provider string, endpoint config.ProviderEndpoint, virtualKey string, log *config.Logger) ([]llm.ModelEntry, error) {
	switch endpoint.AuthKind {
	case config.ProviderAuthChatGPT:
		client, err := chatgpt.NewClient(endpoint.URL, profileRoot, provider, virtualKey, nil, log)
		if err != nil {
			return nil, err
		}
		return client.ListModels(ctx)
	case config.ProviderAuthClaudeSDK:
		if _, err := claude.Check(ctx); err != nil {
			return nil, err
		}
		return claude.StaticModels(), nil
	default:
		return api.NewClient(endpoint.URL, endpoint.APIKey, virtualKey, log).ListModels(ctx)
	}
}

// defaultSubscriptionClient builds the startup default client for a profile.
func defaultSubscriptionClient(profile *config.Config, log *config.Logger) llm.Chatter {
	if profile == nil {
		return nil
	}
	return subscriptionClientFor(profile.ProfileRoot, profile.LLMGatewayProvider, config.ProviderEndpoint{
		URL: profile.LLMGatewayURL, APIKey: profile.LLMGatewayAPIKey, AuthKind: profile.LLMGatewayAuthKind,
	}, profile.LLMGatewayVirtualKey, log)
}

// routerSeamsFor returns per-profile router seams bound to one profile root.
func routerSeamsFor(profile *config.Config, log *config.Logger) (agent.NewClientFunc, agent.ListModelsFunc) {
	root := ""
	if profile != nil {
		root = profile.ProfileRoot
	}
	return func(provider string, endpoint config.ProviderEndpoint, virtualKey string) llm.Chatter {
			return subscriptionClientFor(root, provider, endpoint, virtualKey, log)
		}, func(ctx context.Context, provider string, endpoint config.ProviderEndpoint, virtualKey string) ([]llm.ModelEntry, error) {
			return subscriptionListModels(ctx, root, provider, endpoint, virtualKey, log)
		}
}
