package tools

import (
	"context"
	"fmt"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm/api"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm/chatgpt"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm/claude"
	"os/exec"
	"sort"
	"strings"
)

// registerHandlers wires configured builtin handlers and policies into the registry.

func cronTargetValidators(cfg *config.Config, log *config.Logger) map[string]cron.TargetValidator {
	validators := make(map[string]cron.TargetValidator)
	if url := strings.TrimSpace(cfg.BlueBubblesURL); url != "" && strings.TrimSpace(cfg.BlueBubblesPassword) != "" {
		validators[cron.DeliverIMessage] = cron.NewIMessageTargetValidator(url, cfg.BlueBubblesPassword)
	}
	if token := strings.TrimSpace(cfg.DiscordToken); token != "" {
		validators[cron.DeliverDiscord] = cron.NewDiscordTargetValidator("", token)
	}
	return validators
}

// cronModelChecker validates explicit cron model references against live
// provider state. Errors name only the provider/model identity and the
// configured provider set, never endpoints or credentials.
type cronModelChecker struct {
	endpoints   map[string]config.ProviderEndpoint
	virtualKey  string
	profileRoot string
	log         *config.Logger
}

// ValidateModel implements cron.ModelValidator: the provider must be
// configured and the model must appear in its live listing.
func (c *cronModelChecker) ValidateModel(ctx context.Context, provider, model string) error {
	provider = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(provider), "custom:"))
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return fmt.Errorf("model must be {provider, model}")
	}
	endpoint, ok := c.endpoints[provider]
	if !ok {
		return fmt.Errorf("unknown model provider %q (configured: %s)", provider, strings.Join(sortedEndpointNames(c.endpoints), ", "))
	}
	models, err := subscriptionListing(ctx, c.profileRoot, provider, endpoint, c.virtualKey, c.log)
	if err != nil {
		return fmt.Errorf("model provider %q is unavailable", provider)
	}
	for _, entry := range models {
		if entry.ID == model {
			return nil
		}
	}
	return fmt.Errorf("unknown model %q for provider %q", model, provider)
}

// cronModelValidatorFor builds the production cron model validator from the
// profile's effective provider map. A nil config yields a nil validator so
// registry construction never panics; the handler then enforces syntax only.
func cronModelValidatorFor(cfg *config.Config, log *config.Logger) cron.ModelValidator {
	if cfg == nil {
		return nil
	}
	return &cronModelChecker{endpoints: cfg.ProviderEndpoints, virtualKey: cfg.LLMGatewayVirtualKey, profileRoot: cfg.ProfileRoot, log: log}
}

func sortedEndpointNames(endpoints map[string]config.ProviderEndpoint) []string {
	names := make([]string, 0, len(endpoints))
	for name := range endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// videoBinaryLookPath resolves the ffmpeg/ffprobe pair; tests substitute a
// fake so registration never depends on host binaries.
var videoBinaryLookPath = exec.LookPath

// lookupVideoBinaries resolves the ffmpeg/ffprobe pair for frame sampling.
// Empty strings mean frame extraction is unavailable and video_analyze stays
// disabled; the agent keeps the terminal recipe instead.
func lookupVideoBinaries() (ffmpeg, ffprobe string) {
	ffmpeg, _ = videoBinaryLookPath("ffmpeg")
	ffprobe, _ = videoBinaryLookPath("ffprobe")
	return ffmpeg, ffprobe
}

// subscriptionListing resolves one provider's model listing for validation.
// Unauthorized ChatGPT subscriptions degrade instead of advertising models
// that cannot run; key providers use the live gateway listing.
func subscriptionListing(ctx context.Context, profileRoot, provider string, endpoint config.ProviderEndpoint, virtualKey string, log *config.Logger) ([]llm.ModelEntry, error) {
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

// DelegatePolicy returns the registry policy for the delegate tool: full
// history with searchable results. Startup reuses it when installing the
// live per-profile handler over the placeholder, so both registrations share
// one policy definition.
