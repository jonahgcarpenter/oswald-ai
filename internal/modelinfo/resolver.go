package modelinfo

import (
	"context"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const (
	defaultContextWindow = 4096
	defaultOutputTokens  = 1024
)

// Details describes model metadata discovered from OpenRouter and environment overrides.
type Details struct {
	Name            string
	Provider        string
	MaxInputTokens  int
	MaxOutputTokens int
	ContextWindow   int
	Source          string
	Confidence      string
}

// Resolve discovers model metadata from OpenRouter, applies environment overrides,
// then falls back to package defaults for any missing values.
func Resolve(ctx context.Context, cfg *config.Config, log *config.Logger) (Details, error) {
	openRouter, err := ResolveFromOpenRouter(ctx, cfg.LLMGatewayModel, log)
	if err != nil {
		log.Server("modelinfo").Warn("modelinfo.openrouter.resolve_failed", "failed to resolve model details from OpenRouter", config.F("model", cfg.LLMGatewayModel), config.F("status", "degraded"), config.ErrorField(err))
	}

	details := openRouter
	if err != nil {
		details = Details{
			Name:       cfg.LLMGatewayModel,
			Provider:   "fallback",
			Source:     "fallback",
			Confidence: "low",
		}
	}
	details.Name = cfg.LLMGatewayModel
	applyEnvOverrides(&details, cfg, openRouter, err == nil, log)

	details = normalizeDetails(details)
	if err != nil {
		return details, fmt.Errorf("modelinfo: OpenRouter details unavailable: %w", err)
	}
	return details, nil
}

func applyEnvOverrides(details *Details, cfg *config.Config, openRouter Details, hasOpenRouter bool, log *config.Logger) {
	sourceHasEnv := false
	if cfg.ModelContextWindow > 0 {
		logOverrideDiscrepancy(log, cfg.LLMGatewayModel, "context_window", cfg.ModelContextWindow, openRouter.ContextWindow, hasOpenRouter)
		details.ContextWindow = cfg.ModelContextWindow
		sourceHasEnv = true
	}
	if cfg.ModelMaxOutputTokens > 0 {
		logOverrideDiscrepancy(log, cfg.LLMGatewayModel, "max_output_tokens", cfg.ModelMaxOutputTokens, openRouter.MaxOutputTokens, hasOpenRouter)
		details.MaxOutputTokens = cfg.ModelMaxOutputTokens
		sourceHasEnv = true
	}
	if !sourceHasEnv {
		return
	}

	if hasOpenRouter {
		source := openRouter.Source
		if source == "" {
			source = "openrouter.models.hugging_face_id"
		}
		details.Source = "env+" + source
		details.Confidence = "high"
		return
	}
	details.Source = "env"
	details.Confidence = "low"
	if details.Provider == "" || details.Provider == "fallback" {
		details.Provider = "env"
	}
}

func logOverrideDiscrepancy(log *config.Logger, model string, field string, envValue int, openRouterValue int, hasOpenRouter bool) {
	if !hasOpenRouter || openRouterValue <= 0 || envValue == openRouterValue {
		return
	}
	log.Server("modelinfo").Warn("modelinfo.override.discrepancy", "model metadata env override differs from OpenRouter", config.F("model", model), config.F("field", field), config.F("env_value", envValue), config.F("openrouter_value", openRouterValue), config.F("status", "ok"))
}

func normalizeDetails(details Details) Details {
	if details.ContextWindow <= 0 {
		details.ContextWindow = defaultContextWindow
	}
	if details.MaxOutputTokens <= 0 {
		details.MaxOutputTokens = defaultOutputTokens
	}
	if details.MaxInputTokens <= 0 && details.ContextWindow > details.MaxOutputTokens {
		details.MaxInputTokens = details.ContextWindow - details.MaxOutputTokens
	}
	if details.Source == "" {
		details.Source = "fallback"
	}
	if details.Confidence == "" {
		details.Confidence = "low"
	}
	return details
}
