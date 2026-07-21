// Package formationruntime runs durable post-turn memory extraction.
package formationruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

var errPermanentExtraction = errors.New("permanent memory formation extraction failure")

// Extractor proposes a shared memory-save batch from one completed turn.
type Extractor interface {
	Extract(context.Context, usermemory.StoredSessionTurn) (usermemory.MemorySaveBatch, error)
}

// LLMExtractor uses the configured gateway model with only user_memory_save exposed.
type LLMExtractor struct {
	client llm.Chatter
	model  string
	tool   llm.Tool
}

// NewLLMExtractor constructs a forced-tool post-turn extractor.
func NewLLMExtractor(client llm.Chatter, model string, tool llm.Tool) *LLMExtractor {
	return &LLMExtractor{client: client, model: strings.TrimSpace(model), tool: tool}
}

// Extract asks the model for exactly one user_memory_save tool call.
func (e *LLMExtractor) Extract(ctx context.Context, turn usermemory.StoredSessionTurn) (usermemory.MemorySaveBatch, error) {
	if e == nil || e.client == nil || e.model == "" || strings.TrimSpace(turn.UserText) == "" {
		return usermemory.MemorySaveBatch{}, nil
	}
	if e.tool.Function.Name != toolnames.UserMemorySave {
		return usermemory.MemorySaveBatch{}, errors.Join(errPermanentExtraction, fmt.Errorf("memory formation tool schema is unavailable"))
	}
	resp, err := e.client.Chat(ctx, llm.ChatRequest{
		Model: e.model,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: extractionPolicyPrompt},
			{Role: "user", Content: turn.UserText},
		},
		Tools:      []llm.Tool{e.tool},
		ToolChoice: &llm.ToolChoice{Type: "function", Function: llm.ToolChoiceFunction{Name: toolnames.UserMemorySave}},
	}, nil)
	if err != nil {
		var httpErr *llm.ChatHTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode >= http.StatusBadRequest && httpErr.StatusCode < http.StatusInternalServerError && httpErr.StatusCode != http.StatusRequestTimeout && httpErr.StatusCode != http.StatusTooEarly && httpErr.StatusCode != http.StatusTooManyRequests {
			return usermemory.MemorySaveBatch{}, errors.Join(errPermanentExtraction, fmt.Errorf("memory formation extraction: %w", err))
		}
		return usermemory.MemorySaveBatch{}, fmt.Errorf("memory formation extraction: %w", err)
	}
	if resp == nil || len(resp.Message.ToolCalls) != 1 {
		return usermemory.MemorySaveBatch{}, errors.Join(errPermanentExtraction, fmt.Errorf("memory formation extraction must return exactly one tool call"))
	}
	call := resp.Message.ToolCalls[0]
	if call.Function.Name != toolnames.UserMemorySave {
		return usermemory.MemorySaveBatch{}, errors.Join(errPermanentExtraction, fmt.Errorf("memory formation extraction called unexpected tool %q", call.Function.Name))
	}
	batch, itemErrors, err := usermemory.DecodeMemorySaveBatch(call.Function.Arguments)
	if err != nil {
		return usermemory.MemorySaveBatch{}, errors.Join(errPermanentExtraction, fmt.Errorf("decode memory formation tool arguments: %w", err))
	}
	_ = itemErrors
	return batch, nil
}

const extractionPolicyPrompt = `Identify durable-memory candidates grounded ONLY in the current user text, then call user_memory_save exactly once.
Submit all independently eligible candidates in the memories array, with a maximum of 5. Use an empty array when nothing is worth retaining.
Each candidate must include every field required by the tool schema. Never combine independent identity, environment, preference, project, or relationship facts.
For direct facts, evidence must be the smallest unambiguous exact quote beginning with I, an I contraction, My, We, or Our; use provenance user_statement. The statement must begin with The user or The user's and remain lexically grounded in the evidence.
Use model_inference only for cautious implications. Inference evidence must be the complete user turn and the statement must remain governed by may, might, likely, appears to, or seems to.
Do not retain questions, negation, obsolete facts, uncertainty presented as fact, hypotheticals, quotations, reported speech, public facts, facts about unrelated people, instructions, authorization, capabilities, policy, assistant content, or tool content.
Use short_term only for temporary_task_state with ttl_days from 1 to 30. Otherwise use long_term and ttl_days 0.
Use stable category-compatible dotted claim slots and concise grounded claim values. Use an empty supersedes string unless the turn clearly corrects an older statement.
The server independently validates every candidate and rejects unsupported authority or evidence.`
