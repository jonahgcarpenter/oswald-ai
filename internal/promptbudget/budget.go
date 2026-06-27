package promptbudget

import (
	"encoding/json"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/modelinfo"
)

const (
	defaultContextWindow   = 4096
	defaultResponseReserve = 1024
	defaultToolReserve     = 768
	defaultSafetyMargin    = 256
	messageTokenOverhead   = 12
	imageTokenEstimate     = 768
)

// ContextBudget describes the request-time prompt budget derived from the
// active model's context window.
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

// FromModelDetails derives prompt-budget settings from discovered model metadata.
func FromModelDetails(details modelinfo.Details) ContextBudget {
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

// Result reports request-time prompt estimate details.
type Result struct {
	EstimatedBefore int
	EstimatedAfter  int
}

// EstimateTokens provides the shared token estimate used by the agent.
func EstimateTokens(systemPrompt string, history []llm.ChatMessage, userPrompt string, userImageCount int, tools []llm.Tool) int {
	total := estimateMessageTokens(llm.ChatMessage{Role: "system", Content: systemPrompt})
	for _, msg := range history {
		total += estimateMessageTokens(msg)
	}
	total += estimateMessageTokens(llm.ChatMessage{Role: "user", Content: userPrompt, Images: make([]llm.InputImage, userImageCount)})
	total += estimateToolTokens(tools)
	return total
}

func estimateMessageTokens(msg llm.ChatMessage) int {
	contentLen := len(msg.Role) + len(msg.Content) + len(msg.Thinking) + len(msg.ToolName)
	for _, tc := range msg.ToolCalls {
		contentLen += len(tc.Function.Name)
		if encoded, err := json.Marshal(tc.Function.Arguments); err == nil {
			contentLen += len(encoded)
		}
	}
	return contentLen/4 + messageTokenOverhead + len(msg.Images)*imageTokenEstimate
}

func estimateToolTokens(tools []llm.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return len(tools) * 64
	}
	return len(encoded)/4 + 32
}
