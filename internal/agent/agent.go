package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// ModelMetrics holds performance data from a single LLM call.
type ModelMetrics struct {
	Model              string  `json:"model"`
	TotalDuration      int64   `json:"total_duration_ms"`
	LoadDuration       int64   `json:"load_duration_ms"`
	PromptEvalDuration int64   `json:"prompt_eval_duration_ms"`
	EvalDuration       int64   `json:"eval_duration_ms"`
	TokensPerSecond    float64 `json:"tokens_per_second"`
}

// AgentResponse is the final payload returned to the gateway after processing.
type AgentResponse struct {
	Category      string        `json:"category"`
	Reason        string        `json:"reason"`
	Model         string        `json:"model"`
	Response      string        `json:"response,omitempty"`
	Error         string        `json:"error,omitempty"`
	RouterMetrics *ModelMetrics `json:"router_metrics,omitempty"`
	ExpertMetrics *ModelMetrics `json:"expert_metrics,omitempty"`
}

// Agent handles all LLM orchestration.
type Agent struct {
	provider    llm.Provider
	routerModel string
	workers     []WorkerAgent
	log         *config.Logger
}

// NewAgent initializes the orchestration agent with a generic LLM provider,
// a router model name, the loaded worker agent registry, and a logger.
func NewAgent(provider llm.Provider, routerModel string, workers []WorkerAgent, log *config.Logger) *Agent {
	return &Agent{
		provider:    provider,
		routerModel: routerModel,
		workers:     workers,
		log:         log,
	}
}

// truncate returns s shortened to at most max runes, appending "…" if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// Process handles the end-to-end agentic workflow: Triage -> Generation.
func (a *Agent) Process(userPrompt string, streamCallback func(chunk string)) (*AgentResponse, error) {
	a.log.Info("Processing request: %q", truncate(userPrompt, 100))

	// Triage routing
	ctxRoute, cancelRoute := context.WithTimeout(context.Background(), 60*time.Second)
	decision, err := DetermineRoute(ctxRoute, a.provider, a.routerModel, a.workers, userPrompt, a.log)
	cancelRoute()

	if err != nil {
		return nil, fmt.Errorf("failed to route request: %w", err)
	}

	// Resolve the matched worker for model name and system prompt
	worker := FindWorker(a.workers, decision.Category)
	if worker == nil {
		// Should not happen if the fallback in DetermineRoute is correct,
		// but guard anyway by using the first worker.
		worker = &a.workers[0]
	}

	expertModel := worker.ResolveModel()
	systemPrompt := worker.SystemPrompt

	a.log.Debug("Expert generation starting: model=%s category=%s", expertModel, decision.Category)

	// Expert Generation via /api/chat
	ctxGen, cancelGen := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelGen()

	isStreaming := streamCallback != nil

	expertMessages := []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	expertReq := llm.ChatRequest{
		Model:    expertModel,
		Messages: expertMessages,
		Stream:   isStreaming,
	}

	// Adapt the gateway's func(string) callback to the Chat API's func(ChatMessage) callback.
	// Only the Content field is relevant for streaming to the client.
	var chatCallback func(llm.ChatMessage)
	if streamCallback != nil {
		chatCallback = func(chunk llm.ChatMessage) {
			if chunk.Content != "" {
				streamCallback(chunk.Content)
			}
		}
	}

	expertResp, err := a.provider.Chat(ctxGen, expertReq, chatCallback)
	if err != nil {
		a.log.Error("Expert model %s failed: %v", expertModel, err)
		return &AgentResponse{
			Category: decision.Category,
			Model:    expertModel,
			Error:    fmt.Sprintf("Oswald's %s model failed to respond: %v", expertModel, err),
		}, nil
	}

	a.log.Info("Response complete: category=%s model=%s", decision.Category, expertModel)

	return &AgentResponse{
		Category:      decision.Category,
		Reason:        decision.Reason,
		Model:         expertModel,
		Response:      expertResp.Message.Content,
		RouterMetrics: mapMetrics(decision.Metrics),
		ExpertMetrics: mapMetrics(expertResp),
	}, nil
}

// mapMetrics converts a *llm.ChatResponse into a *ModelMetrics summary.
func mapMetrics(resp *llm.ChatResponse) *ModelMetrics {
	if resp == nil || resp.EvalDuration <= 0 {
		return nil
	}
	tps := float64(resp.EvalCount) / (float64(resp.EvalDuration) / 1e9)
	return &ModelMetrics{
		Model:              resp.Model,
		TotalDuration:      resp.TotalDuration / 1e6,
		LoadDuration:       resp.LoadDuration / 1e6,
		PromptEvalDuration: resp.PromptEvalDuration / 1e6,
		EvalDuration:       resp.EvalDuration / 1e6,
		TokensPerSecond:    tps,
	}
}
