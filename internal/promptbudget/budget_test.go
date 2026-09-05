package promptbudget

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/modelinfo"
)

func TestContextBudgetUsesTightestRealInputLimit(t *testing.T) {
	budget := FromModelDetails(modelinfo.Details{ContextWindow: 10000, MaxInputTokens: 4321, MaxOutputTokens: 999, Source: "test"})
	if budget.UsableInputLimit() != 4065 || budget.ResponseReserve != 999 || budget.Source != "test" {
		t.Fatalf("unexpected budget: %+v", budget)
	}
	if budget.PromptBudget() != 3297 {
		t.Fatalf("legacy prompt budget = %d, want 3297", budget.PromptBudget())
	}

	budget = ContextBudget{ContextWindow: 100, ResponseReserve: 100, ToolReserve: 100, SafetyMargin: 100}
	if budget.UsableInputLimit() != 0 || budget.PromptBudget() != 0 {
		t.Fatalf("budget must not exceed actual capacity: %+v", budget)
	}
}

func TestContextBudgetCapsExplicitLimitAtContextCapacity(t *testing.T) {
	budget := ContextBudget{ContextWindow: 8000, ResponseReserve: 2000, PromptLimit: 7000, SafetyMargin: 250}
	if got := budget.UsableInputLimit(); got != 5750 {
		t.Fatalf("UsableInputLimit() = %d, want 5750", got)
	}

	budget = ContextBudget{PromptLimit: 4000, SafetyMargin: 250}
	if got := budget.UsableInputLimit(); got != 3750 {
		t.Fatalf("explicit-only UsableInputLimit() = %d, want 3750", got)
	}
}

func TestEstimateTokensIncludesMessagesImagesAndTools(t *testing.T) {
	history := []llm.ChatMessage{
		{Role: "user", Content: strings.Repeat("a", 100)},
		{Role: "assistant", Content: strings.Repeat("b", 100)},
	}
	tools := []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "test.tool"}}}
	withoutImage := EstimateTokens("system", history, "now", 0, tools)
	withImage := EstimateTokens("system", history, "now", 1, tools)
	if withoutImage <= 0 {
		t.Fatalf("expected positive estimate, got %d", withoutImage)
	}
	if withImage <= withoutImage {
		t.Fatalf("expected image estimate to increase token count, got without=%d with=%d", withoutImage, withImage)
	}
}

func TestEstimateTokensCompatibilityMatchesEstimateRequest(t *testing.T) {
	history := []llm.ChatMessage{{Role: "assistant", Content: "Zażółć gęślą jaźń"}}
	tools := []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "test.tool"}}}
	legacy := EstimateTokens("policy", history, "こんにちは", 2, tools)
	messages := []llm.ChatMessage{
		{Role: "system", Content: "policy"},
		history[0],
		{Role: "user", Content: "こんにちは", Images: make([]llm.InputImage, 2)},
	}
	if got := EstimateRequest(messages, tools); got != legacy {
		t.Fatalf("EstimateRequest() = %d, EstimateTokens() = %d", got, legacy)
	}
}

func TestEstimateRequestConservativelyCountsNonASCII(t *testing.T) {
	ascii := EstimateRequest([]llm.ChatMessage{{Role: "user", Content: strings.Repeat("a", 40)}}, nil)
	unicode := EstimateRequest([]llm.ChatMessage{{Role: "user", Content: strings.Repeat("界", 40)}}, nil)
	if unicode <= ascii {
		t.Fatalf("non-ASCII estimate = %d, want greater than ASCII estimate %d", unicode, ascii)
	}
}
