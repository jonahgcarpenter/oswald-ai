package modelinfo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const openRouterModelsURL = "https://openrouter.ai/api/v1/models"

type openRouterModelsResponse struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID            string                `json:"id"`
	HuggingFaceID string                `json:"hugging_face_id"`
	TopProvider   openRouterTopProvider `json:"top_provider"`
}

type openRouterTopProvider struct {
	ContextLength       int `json:"context_length"`
	MaxCompletionTokens int `json:"max_completion_tokens"`
}

// ResolveFromOpenRouter queries OpenRouter's public model catalog and resolves
// metadata by matching the active model against hugging_face_id.
func ResolveFromOpenRouter(ctx context.Context, model string, log *config.Logger) (Details, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return Details{}, fmt.Errorf("model name is empty")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterModelsURL, nil)
	if err != nil {
		return Details{}, fmt.Errorf("create OpenRouter models request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Details{}, fmt.Errorf("OpenRouter models request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Details{}, fmt.Errorf("OpenRouter models returned HTTP %d", resp.StatusCode)
	}

	var decoded openRouterModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return Details{}, fmt.Errorf("decode OpenRouter models response: %w", err)
	}

	selected, ok := selectOpenRouterModel(decoded.Data, model)
	if !ok {
		return Details{}, fmt.Errorf("OpenRouter models contained no unambiguous hugging_face_id match for %q", model)
	}
	if selected.TopProvider.ContextLength <= 0 {
		return Details{}, fmt.Errorf("OpenRouter model %q has no usable context_length", selected.HuggingFaceID)
	}

	details := Details{
		Name:            selected.HuggingFaceID,
		Provider:        "openrouter",
		ContextWindow:   selected.TopProvider.ContextLength,
		MaxOutputTokens: selected.TopProvider.MaxCompletionTokens,
		Source:          "openrouter.models.hugging_face_id",
		Confidence:      "high",
	}
	log.Server("modelinfo").Info("modelinfo.openrouter.resolved", "resolved model details from OpenRouter", config.F("model", details.Name), config.F("openrouter_model_id", selected.ID), config.F("context_window", details.ContextWindow), config.F("max_output_tokens", details.MaxOutputTokens), config.F("status", "ok"))
	return details, nil
}

func selectOpenRouterModel(models []openRouterModel, query string) (openRouterModel, bool) {
	if len(models) == 0 {
		return openRouterModel{}, false
	}
	for _, model := range models {
		if model.HuggingFaceID == query {
			return model, true
		}
	}

	normalizedQuery := normalizeModelName(query)
	matches := make([]openRouterModel, 0, 1)
	for _, model := range models {
		if normalizeModelName(model.HuggingFaceID) == normalizedQuery {
			matches = append(matches, model)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return openRouterModel{}, false
}

func normalizeModelName(model string) string {
	return strings.TrimSpace(strings.ToLower(model))
}
