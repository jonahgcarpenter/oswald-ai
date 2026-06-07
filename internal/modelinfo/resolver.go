package modelinfo

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const (
	defaultContextWindow = 4096
	defaultOutputTokens  = 1024
)

// Details describes model metadata discovered from Bifrost or a backing provider.
type Details struct {
	Name            string
	Provider        string
	MaxInputTokens  int
	MaxOutputTokens int
	ContextWindow   int
	Source          string
	Confidence      string
}

// Resolve discovers model metadata through Bifrost, then provider-specific fallbacks.
func Resolve(ctx context.Context, cfg *config.Config, log *config.Logger) (Details, error) {
	fallback := Details{
		Name:            cfg.BifrostModel,
		MaxOutputTokens: defaultOutputTokens,
		ContextWindow:   defaultContextWindow,
		Source:          "fallback",
		Confidence:      "low",
	}

	bifrost, err := ResolveFromBifrost(ctx, cfg.BifrostURL, cfg.BifrostAPIKey, cfg.BifrostModel, log)
	if err != nil {
		log.Server("modelinfo").Warn("modelinfo.bifrost.resolve_failed", "failed to resolve model details from bifrost", config.F("model", cfg.BifrostModel), config.F("status", "degraded"), config.ErrorField(err))
		return fallback, fmt.Errorf("modelinfo: bifrost details unavailable: %w", err)
	}

	if hasUsableLimits(bifrost) {
		return normalizeDetails(bifrost), nil
	}

	if strings.EqualFold(strings.TrimSpace(bifrost.Provider), "ollama") {
		ollama, ollamaErr := ResolveFromOllama(ctx, cfg.OllamaProviderURL, bifrost.Name, log)
		if ollamaErr == nil && ollama.ContextWindow > 0 {
			ollama.Provider = "ollama"
			ollama.Name = firstNonEmpty(ollama.Name, bifrost.Name, cfg.BifrostModel)
			return normalizeDetails(ollama), nil
		}
		if ollamaErr != nil {
			log.Server("modelinfo").Warn("modelinfo.provider.ollama.resolve_failed", "failed to resolve model details from ollama provider", config.F("model", bifrost.Name), config.F("status", "degraded"), config.ErrorField(ollamaErr))
		}
	}

	fallback.Name = firstNonEmpty(bifrost.Name, cfg.BifrostModel)
	fallback.Provider = bifrost.Provider
	return fallback, nil
}

func hasUsableLimits(details Details) bool {
	return details.MaxInputTokens > 0 || details.ContextWindow > 0
}

func normalizeDetails(details Details) Details {
	if details.MaxInputTokens > 0 && details.ContextWindow == 0 {
		if details.MaxOutputTokens > 0 {
			details.ContextWindow = details.MaxInputTokens + details.MaxOutputTokens
		} else {
			details.ContextWindow = details.MaxInputTokens + defaultOutputTokens
		}
	}
	if details.ContextWindow <= 0 {
		details.ContextWindow = defaultContextWindow
	}
	if details.Source == "" {
		details.Source = "fallback"
	}
	if details.Confidence == "" {
		details.Confidence = "low"
	}
	return details
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
