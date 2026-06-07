package modelinfo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type bifrostDetailsResponse struct {
	Models []bifrostModelDetails `json:"models"`
	Total  int                   `json:"total"`
}

type bifrostModelDetails struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	MaxInputTokens  int    `json:"max_input_tokens"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

// ResolveFromBifrost queries Bifrost's model details endpoint.
func ResolveFromBifrost(ctx context.Context, baseURL, apiKey, model string, log *config.Logger) (Details, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return Details{}, fmt.Errorf("bifrost base URL is empty")
	}
	endpoint := fmt.Sprintf("%s/api/models/details?query=%s&limit=5", baseURL, url.QueryEscape(model))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Details{}, fmt.Errorf("create bifrost details request: %w", err)
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Details{}, fmt.Errorf("bifrost details request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Details{}, fmt.Errorf("bifrost details returned HTTP %d", resp.StatusCode)
	}

	var decoded bifrostDetailsResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return Details{}, fmt.Errorf("decode bifrost details response: %w", err)
	}
	selected, ok := selectBifrostModel(decoded.Models, model)
	if !ok {
		return Details{}, fmt.Errorf("bifrost details contained no unambiguous match for %q", model)
	}

	details := Details{
		Name:            selected.Name,
		Provider:        selected.Provider,
		MaxInputTokens:  selected.MaxInputTokens,
		MaxOutputTokens: selected.MaxOutputTokens,
		Source:          "bifrost.models.details",
		Confidence:      "high",
	}
	if details.MaxInputTokens > 0 {
		details.Source = "bifrost.models.details.max_input_tokens"
	}
	log.Server("modelinfo").Info("modelinfo.bifrost.resolved", "resolved model details from bifrost", config.F("model", details.Name), config.F("provider", details.Provider), config.F("max_input_tokens", details.MaxInputTokens), config.F("max_output_tokens", details.MaxOutputTokens), config.F("status", "ok"))
	return details, nil
}

func selectBifrostModel(models []bifrostModelDetails, query string) (bifrostModelDetails, bool) {
	if len(models) == 0 {
		return bifrostModelDetails{}, false
	}
	for _, model := range models {
		if model.Name == query {
			return model, true
		}
	}
	normalizedQuery := normalizeModelName(query)
	matches := make([]bifrostModelDetails, 0, 1)
	for _, model := range models {
		if normalizeModelName(model.Name) == normalizedQuery {
			matches = append(matches, model)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}

	queryVariants := modelNameVariants(query)
	matches = matches[:0]
	for _, model := range models {
		for variant := range modelNameVariants(model.Name) {
			if queryVariants[variant] {
				matches = append(matches, model)
				break
			}
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return bifrostModelDetails{}, false
}

func normalizeModelName(model string) string {
	return strings.TrimSpace(strings.ToLower(model))
}

func modelNameVariants(model string) map[string]bool {
	normalized := normalizeModelName(model)
	variants := map[string]bool{}
	if normalized == "" {
		return variants
	}
	candidates := []string{normalized}
	if slash := strings.LastIndex(normalized, "/"); slash >= 0 && slash < len(normalized)-1 {
		candidates = append(candidates, normalized[slash+1:])
	}
	for _, candidate := range candidates {
		variants[candidate] = true
		if colon := strings.LastIndex(candidate, ":"); colon > 0 {
			variants[candidate[:colon]] = true
		}
	}
	return variants
}
