package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	tokenbudget "github.com/jonahgcarpenter/oswald-ai/internal/session/compaction/budget"
)

func TestTrimThenCompactDropsOldestUnsummarized(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "continued"}}}}
	a, store := newTestAgent(t, chat, nil, nil)
	a.budget = tokenbudget.ContextBudget{PromptLimit: 20000}
	sum := &fakeMarkdownSummarizer{markdown: "# Summary\nTrimmed."}
	a.SetMarkdownSummarizer(sum)
	profile, err := store.ResolveSessionContext(context.Background(), "user-1", "session-trim", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const seedTurns = 110
	for i := 1; i <= seedTurns; i++ {
		big := strings.Repeat(fmt.Sprintf("turn-%d-", i), 40)
		if err := store.AppendSessionTurnForGeneration(context.Background(), "session-trim", "user-1", profile.Generation, "user "+big, "assistant "+big, nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session-trim", profile.Generation, 0, 500)
	if err != nil || len(before) != seedTurns {
		t.Fatalf("setup turns=%d err=%v", len(before), err)
	}
	oldest := before[len(before)-1].ID
	if _, err := processAgent(a, "req-trim", "imessage", "session-trim", "user-1", "Display", "continue", nil, nil); err != nil {
		t.Fatal(err)
	}
	if sum.calls != 1 {
		t.Fatalf("compaction did not trigger: calls=%d", sum.calls)
	}
	for _, id := range sum.gotIDs {
		if id == oldest {
			t.Fatalf("trimmed oldest turn %d was summarized; it should have been dropped unsummarized", oldest)
		}
	}
	after, err := store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session-trim", profile.Generation, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	// Newest 3 seeded turns plus the just-completed turn remain.
	if len(after) != 4 {
		t.Fatalf("context turns=%d, want 4", len(after))
	}
	for _, turn := range after {
		if turn.ID == oldest {
			t.Fatalf("trimmed turn %d still in context", oldest)
		}
	}
	messages := primaryRequests(chat.requests)[0].Messages
	if messagesContain(messages, before[len(before)-1].UserText[:40]) {
		t.Fatalf("trimmed turn leaked into prompt")
	}
}
