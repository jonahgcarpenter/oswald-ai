package modelinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type ollamaShowRequest struct {
	Model   string `json:"model"`
	Verbose bool   `json:"verbose,omitempty"`
}

type ollamaShowResponse struct {
	Parameters string                 `json:"parameters,omitempty"`
	ModelInfo  map[string]interface{} `json:"model_info,omitempty"`
}

// ResolveFromOllama queries a backing Ollama provider for model context metadata.
func ResolveFromOllama(ctx context.Context, baseURL, model string, log *config.Logger) (Details, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return Details{}, fmt.Errorf("ollama provider URL is empty")
	}
	payload, err := json.Marshal(ollamaShowRequest{Model: model, Verbose: true})
	if err != nil {
		return Details{}, fmt.Errorf("marshal ollama show request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/show", bytes.NewReader(payload))
	if err != nil {
		return Details{}, fmt.Errorf("create ollama show request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Details{}, fmt.Errorf("ollama show request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Details{}, fmt.Errorf("ollama show returned HTTP %d", resp.StatusCode)
	}

	var show ollamaShowResponse
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return Details{}, fmt.Errorf("decode ollama show response: %w", err)
	}
	if numCtx, ok := parseNumCtx(show.Parameters); ok {
		log.Server("modelinfo").Info("modelinfo.provider.ollama.resolved", "resolved model details from ollama provider", config.F("model", model), config.F("context_window", numCtx), config.F("source", "provider.ollama.show.parameters.num_ctx"), config.F("status", "ok"))
		return Details{Name: model, Provider: "ollama", ContextWindow: numCtx, Source: "provider.ollama.show.parameters.num_ctx", Confidence: "medium"}, nil
	}
	if ctxLen, key, ok := parseModelInfoContextLength(show.ModelInfo); ok {
		log.Server("modelinfo").Info("modelinfo.provider.ollama.resolved", "resolved model details from ollama provider", config.F("model", model), config.F("context_window", ctxLen), config.F("source", "provider.ollama.show.model_info."+key), config.F("status", "ok"))
		return Details{Name: model, Provider: "ollama", ContextWindow: ctxLen, Source: "provider.ollama.show.model_info." + key, Confidence: "medium"}, nil
	}
	return Details{}, fmt.Errorf("ollama show response did not include context metadata")
}

func parseNumCtx(parameters string) (int, bool) {
	for _, line := range strings.Split(parameters, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "num_ctx" {
			value, err := strconv.Atoi(fields[1])
			if err == nil && value > 0 {
				return value, true
			}
		}
	}
	return 0, false
}

func parseModelInfoContextLength(modelInfo map[string]interface{}) (int, string, bool) {
	for key, raw := range modelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		switch value := raw.(type) {
		case float64:
			if value > 0 {
				return int(value), key, true
			}
		case int:
			if value > 0 {
				return value, key, true
			}
		}
	}
	return 0, "", false
}
