package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// thinkTagRe strips <think>...</think> blocks emitted by reasoning models
// (e.g. qwen3, deepseek-r1) before the actual JSON response.
var thinkTagRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// jsonObjectRe extracts the first {...} JSON object from a string, tolerating
// preamble text, postamble text, and markdown code fences.
var jsonObjectRe = regexp.MustCompile(`(?s)\{.*\}`)

// extractJSON strips thinking blocks and pulls the first JSON object out of raw.
func extractJSON(raw string) string {
	// Remove <think>...</think> blocks produced by reasoning models
	cleaned := thinkTagRe.ReplaceAllString(raw, "")
	// Pull the first {...} object — handles preamble/postamble and code fences
	if m := jsonObjectRe.FindString(cleaned); m != "" {
		return m
	}
	return strings.TrimSpace(cleaned)
}

// DetermineRoute asks the fast router model to classify the incoming message
// using the registered worker agents to build the classification prompt.
func DetermineRoute(ctx context.Context, provider llm.Provider, routerModel string, workers []WorkerAgent, prompt string, log *config.Logger) (*RouteDecision, error) {
	systemPrompt := BuildTriagePrompt(workers)

	req := llm.Request{
		Model:  routerModel,
		Prompt: prompt,
		System: systemPrompt,
		Format: "json", // Tells Ollama to enforce JSON output
		Stream: false,  // We need the full JSON object at once, no streaming
	}

	// Send it to the generic provider interface, passing nil since we don't need streaming for an internal step
	resp, err := provider.Generate(ctx, req, nil)
	if err != nil {
		return nil, fmt.Errorf("router failed to reach Ollama: %w", err)
	}

	log.Debug("Triage raw response: %q", resp.Response)

	fallback := workers[0].Category

	// The client already promotes Thinking → Response for thinking models,
	// so resp.Response is always the canonical output to parse here.
	candidate := extractJSON(resp.Response)

	var decision RouteDecision
	parseErr := json.Unmarshal([]byte(candidate), &decision)

	if parseErr != nil {
		log.Warn("Triage: failed to parse JSON, falling back to %s: %v | raw: %q", fallback, parseErr, resp.Response)
		decision = RouteDecision{
			Category: fallback,
			Reason:   "Fallback routing due to unparseable JSON from router.",
		}
	} else if FindWorker(workers, decision.Category) == nil {
		// Valid JSON but category doesn't match any registered worker
		log.Warn("Triage: unknown category %q, falling back to %s | raw: %q", decision.Category, fallback, resp.Response)
		decision = RouteDecision{
			Category: fallback,
			Reason:   fmt.Sprintf("Fallback routing: router returned unknown category %q.", decision.Category),
		}
	}

	// Attach the full response metrics to the decision object
	decision.Metrics = resp

	return &decision, nil
}
