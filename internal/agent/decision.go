package agent

import "github.com/jonahgcarpenter/oswald-ai/internal/llm"

// RouteDecision holds the triage result returned by the router LLM.
type RouteDecision struct {
	Category string            `json:"category"`
	Reason   string            `json:"reason"`
	Metrics  *llm.ChatResponse `json:"-"`
}
