package agent

import (
	"context"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

const (
	// triageMaxAttempts is the total number of tries before falling back.
	triageMaxAttempts = 3

	// triageSystemPrompt instructs the router model to call exactly one tool.
	triageSystemPrompt = "You are a request router. Call exactly one of the available tools to classify the user message. Do not respond with text — only call a tool."
)

// DetermineRoute asks the router model to classify an incoming message by calling one of
// the dynamically-generated worker tools. If the model fails to call a valid tool, the
// request is retried up to triageMaxAttempts times before falling back to the first worker.
// Returns the routing decision with metrics; on hard provider errors, returns an error.
func DetermineRoute(ctx context.Context, provider llm.Provider, routerModel string, workers []WorkerAgent, prompt string, log *config.Logger) (*RouteDecision, error) {
	tools := BuildTriageTools(workers)
	fallbackCategory := workers[0].Category

	messages := []llm.ChatMessage{
		{Role: "system", Content: triageSystemPrompt},
		{Role: "user", Content: prompt},
	}

	req := llm.ChatRequest{
		Model:    routerModel,
		Messages: messages,
		Tools:    tools,
		Stream:   false,
	}

	var lastResp *llm.ChatResponse

	// Retry loop: attempt to get a valid tool call up to triageMaxAttempts times
	for attempt := 1; attempt <= triageMaxAttempts; attempt++ {
		resp, err := provider.Chat(ctx, req, nil)
		if err != nil {
			// Hard provider error — network/timeout failures don't benefit from retries
			return nil, fmt.Errorf("router failed on attempt %d: %w", attempt, err)
		}

		lastResp = resp

		log.Debug("Triage attempt %d/%d: done_reason=%q tool_calls=%d",
			attempt, triageMaxAttempts, resp.DoneReason, len(resp.Message.ToolCalls))

		if len(resp.Message.ToolCalls) == 0 {
			log.Warn("Triage attempt %d/%d: no tool call in response", attempt, triageMaxAttempts)
			continue
		}

		// Extract category from the first tool call.
		// NOTE: System prompt enforces exactly one tool call; we take the first.
		// Even thinking models emit tool calls in the response, not in the Thinking field.
		toolCall := resp.Message.ToolCalls[0]
		category := CategoryFromToolName(toolCall.Function.Name)

		if worker := FindWorker(workers, category); worker != nil {
			// Success: found a valid worker for this category
			reason := ""
			if r, ok := toolCall.Function.Arguments["reason"]; ok {
				reason, _ = r.(string)
			}
			log.Info("Triage: routed to %s — %s", worker.Category, reason)
			return &RouteDecision{
				Category: worker.Category,
				Reason:   reason,
				Metrics:  resp,
			}, nil
		}

		// Invalid tool name or unknown category; retry
		log.Warn("Triage attempt %d/%d: unknown tool name %q (category %q)",
			attempt, triageMaxAttempts, toolCall.Function.Name, category)
	}

	// All attempts exhausted — fall back to first worker gracefully
	log.Warn("Triage: all %d attempts failed, falling back to %s", triageMaxAttempts, fallbackCategory)
	return &RouteDecision{
		Category: fallbackCategory,
		Reason:   fmt.Sprintf("Fallback routing after %d failed triage attempts.", triageMaxAttempts),
		Metrics:  lastResp,
	}, nil
}
