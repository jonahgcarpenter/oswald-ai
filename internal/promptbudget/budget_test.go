package promptbudget

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/modelinfo"
)

func TestContextBudgetUsesPromptLimitAndMinimum(t *testing.T) {
	budget := FromModelDetails(modelinfo.Details{ContextWindow: 10000, MaxInputTokens: 4321, MaxOutputTokens: 999, Source: "test"})
	if budget.PromptBudget() != 4321 || budget.ResponseReserve != 999 || budget.Source != "test" {
		t.Fatalf("unexpected budget: %+v", budget)
	}

	budget = ContextBudget{ContextWindow: 100, ResponseReserve: 100, ToolReserve: 100, SafetyMargin: 100}
	if budget.PromptBudget() != 256 {
		t.Fatalf("expected minimum prompt budget, got %d", budget.PromptBudget())
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
