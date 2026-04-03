package agent

import (
	"context"
	"encoding/json"
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

// ContextBudget describes how much prompt space a worker can safely use.
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

// PruneResult reports how much history was removed to satisfy the active model budget.
type PruneResult struct {
	EstimatedBefore int
	EstimatedAfter  int
	RemovedPairs    int
}

// ResolveContextBudget discovers a worker's usable context window from Ollama,
// then applies optional worker-level overrides for stable tuning.
func ResolveContextBudget(ctx context.Context, client *ollama.Client, worker *WorkerAgent) (ContextBudget, error) {
	budget := ContextBudget{
		ContextWindow:   defaultContextWindow,
		ResponseReserve: defaultResponseReserve,
		ToolReserve:     defaultToolReserve,
		SafetyMargin:    defaultSafetyMargin,
		Source:          "fallback",
	}

	showResp, err := client.Show(ctx, ollama.ShowRequest{Model: worker.ResolveModel()})
	if err != nil {
		applyBudgetOverrides(&budget, worker)
		budget.Source = budgetSourceWithOverride(budget.Source, worker)
		return budget, fmt.Errorf("failed to discover model context for %s: %w", worker.ResolveModel(), err)
	}

	if numCtx, ok := parseNumCtx(showResp.Parameters); ok {
		budget.ContextWindow = numCtx
		budget.Source = "show.parameters.num_ctx"
	} else if modelCtx, key, ok := parseModelInfoContextLength(showResp.ModelInfo); ok {
		budget.ContextWindow = modelCtx
		budget.Source = "show.model_info." + key
	}

	applyBudgetOverrides(&budget, worker)
	budget.Source = budgetSourceWithOverride(budget.Source, worker)
	return budget, nil
}

func applyBudgetOverrides(budget *ContextBudget, worker *WorkerAgent) {
	if worker.ContextWindow > 0 {
		budget.ContextWindow = worker.ContextWindow
	}
	if worker.ResponseReserve > 0 {
		budget.ResponseReserve = worker.ResponseReserve
	}
	if worker.ToolReserve > 0 {
		budget.ToolReserve = worker.ToolReserve
	}
	if worker.SafetyMargin > 0 {
		budget.SafetyMargin = worker.SafetyMargin
	}
}

func budgetSourceWithOverride(source string, worker *WorkerAgent) string {
	if worker.ContextWindow > 0 || worker.ResponseReserve > 0 || worker.ToolReserve > 0 || worker.SafetyMargin > 0 {
		return source + "+worker_override"
	}
	return source
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

// PruneHistoryToFit trims the oldest user/assistant pairs from history until
// the estimated prompt fits within the active prompt budget.
func PruneHistoryToFit(budget ContextBudget, systemPrompt string, history []ollama.ChatMessage, userPrompt string, tools []ollama.Tool) ([]ollama.ChatMessage, PruneResult) {
	trimmed := append([]ollama.ChatMessage(nil), history...)
	result := PruneResult{
		EstimatedBefore: estimatePromptTokens(systemPrompt, history, userPrompt, tools),
	}

	for len(trimmed) >= 2 && estimatePromptTokens(systemPrompt, trimmed, userPrompt, tools) > budget.PromptBudget() {
		trimmed = trimmed[2:]
		result.RemovedPairs++
	}

	result.EstimatedAfter = estimatePromptTokens(systemPrompt, trimmed, userPrompt, tools)
	return trimmed, result
}

func estimatePromptTokens(systemPrompt string, history []ollama.ChatMessage, userPrompt string, tools []ollama.Tool) int {
	total := estimateMessageTokens(ollama.ChatMessage{Role: "system", Content: systemPrompt})
	for _, msg := range history {
		total += estimateMessageTokens(msg)
	}
	total += estimateMessageTokens(ollama.ChatMessage{Role: "user", Content: userPrompt})
	total += estimateToolTokens(tools)
	return total
}

func estimateMessageTokens(msg ollama.ChatMessage) int {
	contentLen := len(msg.Role) + len(msg.Content) + len(msg.Thinking) + len(msg.ToolName)
	for _, tc := range msg.ToolCalls {
		contentLen += len(tc.Function.Name)
		if encoded, err := json.Marshal(tc.Function.Arguments); err == nil {
			contentLen += len(encoded)
		}
	}
	return contentLen/4 + messageTokenOverhead
}

func estimateToolTokens(tools []ollama.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return len(tools) * 64
	}
	return len(encoded)/4 + 32
}
