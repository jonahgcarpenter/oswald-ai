package memory

import (
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

func TestStoreAppendHistoryAndRecentTurns(t *testing.T) {
	store := NewStore(Options{MaxTurns: 3}, config.NewLogger(config.LevelError))
	for _, label := range []string{"one", "two", "three", "four"} {
		store.AppendTurn("s", llm.ChatMessage{Role: "user", Content: label}, llm.ChatMessage{Role: "assistant", Content: "reply " + label}, []float64{1, 0})
	}

	history := store.History("s")
	if len(history) != 6 {
		t.Fatalf("expected 3 retained pairs, got %d messages", len(history))
	}
	if history[0].Content != "two" || history[4].Content != "four" {
		t.Fatalf("unexpected retained history: %+v", history)
	}

	recent := store.RecentTurns("s", 2, 2)
	if len(recent) != 2 || recent[0].User.Content != "three" || recent[1].User.Content != "two" {
		t.Fatalf("unexpected recent turns: %+v", recent)
	}
}

func TestRelevantTurnsSelectsSemanticAndRecentInOriginalOrder(t *testing.T) {
	store := NewStore(Options{}, config.NewLogger(config.LevelError))
	store.ReplaceTurns("s", []Turn{
		{CreatedAt: time.Now().Add(-3 * time.Minute), User: llm.ChatMessage{Content: "low"}, Assistant: llm.ChatMessage{Content: "a"}, Embedding: []float64{0, 1}},
		{CreatedAt: time.Now().Add(-2 * time.Minute), User: llm.ChatMessage{Content: "semantic"}, Assistant: llm.ChatMessage{Content: "b"}, Embedding: []float64{1, 0}},
		{CreatedAt: time.Now().Add(-1 * time.Minute), User: llm.ChatMessage{Content: "recent"}, Assistant: llm.ChatMessage{Content: "c"}, Embedding: []float64{0, 1}},
	})

	result := store.RelevantTurns("s", []float64{1, 0}, RetrievalOptions{RecentTurns: 1, MaxRelevantTurns: 1, MinSimilarity: 0.9, IncludeRecent: true})
	if result.CandidateTurnCount != 3 || result.RecentTurnCount != 1 || result.SemanticTurnCount != 1 {
		t.Fatalf("unexpected counts: %+v", result)
	}
	if len(result.Turns) != 2 || result.Turns[0].User.Content != "semantic" || result.Turns[1].User.Content != "recent" {
		t.Fatalf("unexpected selected turns: %+v", result.Turns)
	}
}
