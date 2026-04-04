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

// ResolveContextBudget discovers the usable context window for the given model
// from Ollama's /api/show endpoint. Reserve values fall back to package defaults
// when Ollama metadata is unavailable.
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

// PruneHistoryToFit trims the lowest-value user/assistant pairs from history
// until the estimated prompt fits within the active prompt budget.
// Pairs are scored by recency and content significance — short/trivial exchanges
// are removed before long substantive ones, regardless of position.
func PruneHistoryToFit(budget ContextBudget, systemPrompt string, history []ollama.ChatMessage, userPrompt string, tools []ollama.Tool) ([]ollama.ChatMessage, PruneResult) {
	trimmed := append([]ollama.ChatMessage(nil), history...)
	result := PruneResult{
		EstimatedBefore: estimatePromptTokens(systemPrompt, history, userPrompt, tools),
	}

	for len(trimmed) >= 2 && estimatePromptTokens(systemPrompt, trimmed, userPrompt, tools) > budget.PromptBudget() {
		// Find and remove the pair with the lowest retention score.
		lowestIdx := leastValuablePairIndex(trimmed)
		// Remove the pair at lowestIdx (two consecutive messages).
		trimmed = append(trimmed[:lowestIdx], trimmed[lowestIdx+2:]...)
		result.RemovedPairs++
	}

	result.EstimatedAfter = estimatePromptTokens(systemPrompt, trimmed, userPrompt, tools)
	return trimmed, result
}

// leastValuablePairIndex returns the index of the first message in the pair
// that should be dropped to reclaim context budget. It prefers removing short
// or trivial exchanges before long substantive ones, and older pairs before
// newer ones when scores are equal.
func leastValuablePairIndex(msgs []ollama.ChatMessage) int {
	n := len(msgs) / 2 // number of complete pairs
	lowestScore := 1e18
	lowestIdx := 0

	for i := 0; i < n; i++ {
		userMsg := msgs[i*2]
		assistantMsg := msgs[i*2+1]

		score := scorePair(i, n, userMsg, assistantMsg)
		if score < lowestScore {
			lowestScore = score
			lowestIdx = i * 2
		}
	}
	return lowestIdx
}

// scorePair computes a retention score for a user/assistant message pair.
// Higher score = more valuable = keep longer.
// Factors: recency, message length (substantiveness), and identity-bearing content.
func scorePair(pairIdx, totalPairs int, user, assistant ollama.ChatMessage) float64 {
	// Recency bonus: newer pairs score higher. Range [0.1, 1.0].
	recency := 0.1 + 0.9*(float64(pairIdx)/float64(max(totalPairs-1, 1)))

	userLen := len(user.Content)
	assistantLen := len(assistant.Content)
	combined := userLen + assistantLen

	// Length bonus: short exchanges (greetings, acks) score lower.
	// Scale: 0.5 for empty, 1.0 at 200+ chars, capped.
	lengthScore := 0.5 + 0.5*min(float64(combined)/200.0, 1.0)

	// Identity bonus: turns where the user stated personal facts are valuable.
	identityBonus := 1.0
	identityKeywords := []string{
		"my name", "i am ", "i'm ", "i work", "i live", "my job",
		"i prefer", "i like", "i need", "i want", "my age", "i use",
	}
	lowerUser := strings.ToLower(user.Content)
	for _, kw := range identityKeywords {
		if strings.Contains(lowerUser, kw) {
			identityBonus = 1.5
			break
		}
	}

	return recency * lengthScore * identityBonus
}

// max returns the larger of two ints.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// min returns the smaller of two float64s.
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
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
