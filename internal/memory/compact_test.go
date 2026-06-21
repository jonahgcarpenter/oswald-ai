package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/modelinfo"
)

func TestPruneTurnsRemovesExpiredThenOverflow(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	turns := []Turn{
		turnAt(now.Add(-3*time.Hour), "expired"),
		turnAt(now.Add(-20*time.Minute), "oldest-kept"),
		turnAt(now.Add(-10*time.Minute), "middle"),
		turnAt(now.Add(-1*time.Minute), "newest"),
	}

	result := PruneTurns(now, turns, Options{MaxAge: time.Hour, MaxTurns: 2})
	if result.RemovedExpired != 1 || result.RemovedOverflow != 1 {
		t.Fatalf("unexpected removed counts expired=%d overflow=%d", result.RemovedExpired, result.RemovedOverflow)
	}
	if len(result.Kept) != 2 || result.Kept[0].User.Content != "middle" || result.Kept[1].User.Content != "newest" {
		t.Fatalf("unexpected kept turns: %+v", result.Kept)
	}
}

func TestContextBudgetUsesPromptLimitAndMinimum(t *testing.T) {
	budget := ContextBudgetFromModelDetails(modelinfo.Details{ContextWindow: 10000, MaxInputTokens: 4321, MaxOutputTokens: 999, Source: "test"})
	if budget.PromptBudget() != 4321 {
		t.Fatalf("expected explicit prompt limit, got %d", budget.PromptBudget())
	}

	budget = ContextBudget{ContextWindow: 100, ResponseReserve: 100, ToolReserve: 100, SafetyMargin: 100}
	if budget.PromptBudget() != 256 {
		t.Fatalf("expected minimum prompt budget, got %d", budget.PromptBudget())
	}
}

func TestPruneHistoryToFitDropsOldestPairs(t *testing.T) {
	history := []llm.ChatMessage{
		{Role: "user", Content: strings.Repeat("a", 400)},
		{Role: "assistant", Content: strings.Repeat("b", 400)},
		{Role: "user", Content: "recent question"},
		{Role: "assistant", Content: "recent answer"},
	}

	trimmed, result := PruneHistoryToFit(ContextBudget{PromptLimit: 120}, "system", history, "now", 0, nil)
	if result.RemovedPairs != 1 {
		t.Fatalf("expected one removed pair, got %d", result.RemovedPairs)
	}
	if len(trimmed) != 2 || trimmed[0].Content != "recent question" {
		t.Fatalf("unexpected trimmed history: %+v", trimmed)
	}
	if result.EstimatedAfter > 120 {
		t.Fatalf("expected estimate to fit, got %d", result.EstimatedAfter)
	}
}

func turnAt(t time.Time, user string) Turn {
	return Turn{
		CreatedAt: t,
		User:      llm.ChatMessage{Role: "user", Content: user},
		Assistant: llm.ChatMessage{Role: "assistant", Content: "reply to " + user},
	}
}
