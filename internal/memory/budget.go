package memory

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

const (
	defaultContextWindow   = 4096
	defaultResponseReserve = 1024
	defaultToolReserve     = 768
	defaultSafetyMargin    = 256
	messageTokenOverhead   = 12
)

// ContextBudget describes the request-time prompt budget derived from the
// active model's context window. It is used by prompt compaction to decide how
// much retained history can be included in a single Ollama request.
type ContextBudget struct {
	ContextWindow   int
	ResponseReserve int
	ToolReserve     int
	SafetyMargin    int
	Source          string
}

// PromptBudget returns the portion of the context window available for input
// messages after reserving room for generation, tools, and a safety buffer.
func (b ContextBudget) PromptBudget() int {
	promptBudget := b.ContextWindow - b.ResponseReserve - b.ToolReserve - b.SafetyMargin
	if promptBudget < 256 {
		return 256
	}
	return promptBudget
}

// ResolveContextBudget discovers the usable context window for the given model
// from Ollama's /api/show endpoint. This is startup-time budget discovery, not
// compaction itself. Reserve values fall back to package defaults when Ollama
// metadata is unavailable.
func ResolveContextBudget(ctx context.Context, client *ollama.Client, model string) (ContextBudget, error) {
	budget := ContextBudget{
		ContextWindow:   defaultContextWindow,
		ResponseReserve: defaultResponseReserve,
		ToolReserve:     defaultToolReserve,
		SafetyMargin:    defaultSafetyMargin,
		Source:          "fallback",
	}

	showResp, err := client.Show(ctx, ollama.ShowRequest{Model: model})
	if err != nil {
		return budget, fmt.Errorf("failed to discover model context for %s: %w", model, err)
	}

	if numCtx, ok := parseNumCtx(showResp.Parameters); ok {
		budget.ContextWindow = numCtx
		budget.Source = "show.parameters.num_ctx"
	} else if modelCtx, key, ok := parseModelInfoContextLength(showResp.ModelInfo); ok {
		budget.ContextWindow = modelCtx
		budget.Source = "show.model_info." + key
	}

	return budget, nil
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
