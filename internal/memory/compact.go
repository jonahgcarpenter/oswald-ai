package memory

import (
	"encoding/json"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

const imageTokenEstimate = 768

// PromptPruneResult reports how much retained history was removed by
// request-time compaction to satisfy the active model budget.
type PromptPruneResult struct {
	EstimatedBefore int
	EstimatedAfter  int
	RemovedPairs    int
}

// RetentionResult reports which stored turns survived retention compaction and
// which were removed permanently due to max-age or max-turn limits.
type RetentionResult struct {
	Kept            []Turn
	Removed         []Turn
	RemovedExpired  int
	RemovedOverflow int
}

// PruneTurns applies store-level retention compaction to a slice of turns.
// This is permanent pruning driven by max-age and max-turn limits.
func PruneTurns(now time.Time, turns []Turn, opts Options) RetentionResult {
	if len(turns) == 0 {
		return RetentionResult{}
	}

	kept := turns
	removed := []Turn(nil)
	removedExpired := 0
	removedOverflow := 0

	if opts.MaxAge > 0 {
		cutoff := now.Add(-opts.MaxAge)
		firstValid := len(kept)
		for i, candidate := range kept {
			if !candidate.CreatedAt.Before(cutoff) {
				firstValid = i
				break
			}
		}
		removedExpired = firstValid
		removed = append(removed, kept[:firstValid]...)
		kept = kept[firstValid:]
	}

	if opts.MaxTurns > 0 && len(kept) > opts.MaxTurns {
		removedOverflow = len(kept) - opts.MaxTurns
		removed = append(removed, kept[:removedOverflow]...)
		kept = kept[removedOverflow:]
	}

	if len(kept) == 0 {
		return RetentionResult{
			Removed:         removed,
			RemovedExpired:  removedExpired,
			RemovedOverflow: removedOverflow,
		}
	}

	cp := make([]Turn, len(kept))
	copy(cp, kept)
	return RetentionResult{
		Kept:            cp,
		Removed:         removed,
		RemovedExpired:  removedExpired,
		RemovedOverflow: removedOverflow,
	}
}

// PruneHistoryToFit applies request-time prompt compaction. It trims the
// oldest retained user/assistant pairs from retained history until the
// estimated request fits within the active prompt budget.
func PruneHistoryToFit(budget ContextBudget, systemPrompt string, history []ollama.ChatMessage, userPrompt string, userImageCount int, tools []ollama.Tool) ([]ollama.ChatMessage, PromptPruneResult) {
	trimmed := append([]ollama.ChatMessage(nil), history...)
	result := PromptPruneResult{
		EstimatedBefore: EstimatePromptTokens(systemPrompt, history, userPrompt, userImageCount, tools),
	}

	for len(trimmed) >= 2 && EstimatePromptTokens(systemPrompt, trimmed, userPrompt, userImageCount, tools) > budget.PromptBudget() {
		trimmed = trimmed[2:]
		result.RemovedPairs++
	}

	result.EstimatedAfter = EstimatePromptTokens(systemPrompt, trimmed, userPrompt, userImageCount, tools)
	return trimmed, result
}

// EstimatePromptTokens provides the shared token estimate used by both the
// agent and request-time compaction logic.
func EstimatePromptTokens(systemPrompt string, history []ollama.ChatMessage, userPrompt string, userImageCount int, tools []ollama.Tool) int {
	total := estimateMessageTokens(ollama.ChatMessage{Role: "system", Content: systemPrompt})
	for _, msg := range history {
		total += estimateMessageTokens(msg)
	}
	total += estimateMessageTokens(ollama.ChatMessage{Role: "user", Content: userPrompt, Images: make([]string, userImageCount)})
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
	return contentLen/4 + messageTokenOverhead + len(msg.Images)*imageTokenEstimate
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
