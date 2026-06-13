package memory

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/modelinfo"
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
// much retained history can be included in a single LLM request.
type ContextBudget struct {
	ContextWindow   int
	ResponseReserve int
	ToolReserve     int
	SafetyMargin    int
	PromptLimit     int
	Source          string
}

// PromptBudget returns the portion of the context window available for input
// messages after reserving room for generation, tools, and a safety buffer.
func (b ContextBudget) PromptBudget() int {
	if b.PromptLimit > 0 {
		return b.PromptLimit
	}
	promptBudget := b.ContextWindow - b.ResponseReserve - b.ToolReserve - b.SafetyMargin
	if promptBudget < 256 {
		return 256
	}
	return promptBudget
}

// ContextBudgetFromModelDetails derives prompt-budget settings from discovered model metadata.
func ContextBudgetFromModelDetails(details modelinfo.Details) ContextBudget {
	budget := ContextBudget{
		ContextWindow:   defaultContextWindow,
		ResponseReserve: defaultResponseReserve,
		ToolReserve:     defaultToolReserve,
		SafetyMargin:    defaultSafetyMargin,
		Source:          "fallback",
	}
	if details.ContextWindow > 0 {
		budget.ContextWindow = details.ContextWindow
	}
	if details.MaxInputTokens > 0 {
		budget.PromptLimit = details.MaxInputTokens
	}
	if details.MaxOutputTokens > 0 {
		budget.ResponseReserve = details.MaxOutputTokens
	}
	if details.Source != "" {
		budget.Source = details.Source
	}
	return budget
}
