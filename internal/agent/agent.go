package agent

import (
	"context"
	"fmt"
	"time"

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
}

// NewAgent initializes the orchestration agent with a generic LLM provider,
// a router model name, and the loaded worker agent registry.
func NewAgent(provider llm.Provider, routerModel string, workers []WorkerAgent) *Agent {
	return &Agent{
		provider:    provider,
		routerModel: routerModel,
		workers:     workers,
	}
}

// Process handles the end-to-end agentic workflow: Triage -> Generation.
func (a *Agent) Process(userPrompt string, streamCallback func(chunk string)) (*AgentResponse, error) {
	// Triage routing
	ctxRoute, cancelRoute := context.WithTimeout(context.Background(), 60*time.Second)
	decision, err := DetermineRoute(ctxRoute, a.provider, a.routerModel, a.workers, userPrompt)
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

	// Expert Generation
	ctxGen, cancelGen := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelGen()

	isStreaming := streamCallback != nil

	expertReq := llm.Request{
		Model:  expertModel,
		Prompt: userPrompt,
		System: systemPrompt,
		Stream: isStreaming,
	}

	expertResp, err := a.provider.Generate(ctxGen, expertReq, streamCallback)
	if err != nil {
		return &AgentResponse{
			Category: decision.Category,
			Model:    expertModel,
			Error:    fmt.Sprintf("Oswald's %s model failed to respond: %v", expertModel, err),
		}, nil
	}

	return &AgentResponse{
		Category:      decision.Category,
		Reason:        decision.Reason,
		Model:         expertModel,
		Response:      expertResp.Response,
		RouterMetrics: mapMetrics(decision.Metrics),
		ExpertMetrics: mapMetrics(expertResp),
	}, nil
}

func mapMetrics(resp *llm.Response) *ModelMetrics {
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
