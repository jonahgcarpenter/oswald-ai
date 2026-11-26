package compact

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

type fakeStore struct {
	generation int
	turns      []memory.SessionTurn
	marked     []int64
}

func (f *fakeStore) ResolveSessionContext(context.Context, string, string, time.Duration) (memory.SessionContext, error) {
	return memory.SessionContext{Generation: f.generation}, nil
}

func (f *fakeStore) RecentCompletedExchangesAfter(context.Context, string, string, int, int64, int) ([]memory.SessionTurn, error) {
	return f.turns, nil
}
func (f *fakeStore) MarkSessionTurnsCompacted(_ context.Context, _, _ string, _ int, turnIDs []int64) error {
	f.marked = append([]int64(nil), turnIDs...)
	return nil
}

type fakeSummarizer struct {
	markdown string
	cached   string
	calls    int
	set      string
	got      int
}

func (f *fakeSummarizer) Summarize(_ context.Context, turns []memory.SessionTurn) (string, *llm.ChatResponse, error) {
	f.calls++
	f.got = len(turns)
	return f.markdown, &llm.ChatResponse{Model: "m"}, nil
}

func (f *fakeSummarizer) Get(_, _ string, _ int) string { return f.cached }

func (f *fakeSummarizer) Set(_, _ string, _ int, markdown string) { f.set = markdown }

// bigCompactTurns builds newest-first turns exceeding the keep tail so
// compaction has candidates. Each turn is ~1K tokens; 20 turns ≈ 20K.
func bigCompactTurns(n int) []memory.SessionTurn {
	turns := make([]memory.SessionTurn, 0, n)
	for i := n; i >= 1; i-- {
		turns = append(turns, memory.SessionTurn{ID: int64(i), UserText: "user " + strings.Repeat("u", 2000), AssistantText: "assistant " + strings.Repeat("a", 2000)})
	}
	return turns
}

func principal() identity.Principal {
	return identity.Principal{CanonicalUserID: "u", Gateway: "discord", ExternalID: "u", Assurance: identity.AssuranceDiscordGateway}
}

func TestCompactRejectsArgsAndRequiresAuth(t *testing.T) {
	h := New(&fakeStore{}, &fakeSummarizer{markdown: "# Hi"})
	result, _ := h.Execute(context.Background(), commands.Request{Principal: principal(), Raw: "/compact extra", Args: []string{"extra"}})
	if result.Outcome.ReasonCode != "invalid_arguments" {
		t.Fatalf("args=%+v", result)
	}
	result, _ = h.Execute(context.Background(), commands.Request{Raw: "/compact"})
	if result.Outcome.ReasonCode != "authentication_required" {
		t.Fatalf("unauthenticated admitted: %+v", result)
	}
}

func TestCompactSummarizesBeyondKeepTail(t *testing.T) {
	store := &fakeStore{generation: 3, turns: bigCompactTurns(20)}
	sum := &fakeSummarizer{markdown: "# Summary"}
	h := New(store, sum)
	result, err := h.Execute(context.Background(), commands.Request{Principal: principal(), SessionKey: "s", Raw: "/compact"})
	if err != nil || sum.calls != 1 || sum.set != "# Summary" {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, sum.calls)
	}
	if sum.got == 0 || len(store.marked) != sum.got {
		t.Fatalf("summarized %d turns, marked=%v", sum.got, store.marked)
	}
	newest := store.turns[0].ID
	for _, id := range store.marked {
		if id == newest {
			t.Fatalf("keep tail turn %d was summarized", newest)
		}
	}
	if !strings.Contains(result.Text, "turns") || result.Outcome.Operation != "session.compact" || !result.Outcome.IsChanged || result.Outcome.AffectedCount != sum.got {
		t.Fatalf("result=%+v", result)
	}
}

func TestCompactKeepsKeepTailVerbatim(t *testing.T) {
	store := &fakeStore{generation: 1, turns: []memory.SessionTurn{
		{ID: 3, UserText: "c", AssistantText: "C"},
		{ID: 2, UserText: "b", AssistantText: "B"},
		{ID: 1, UserText: "a", AssistantText: "A"},
	}}
	sum := &fakeSummarizer{markdown: "# Summary"}
	h := New(store, sum)
	result, err := h.Execute(context.Background(), commands.Request{Principal: principal(), SessionKey: "s", Raw: "/compact"})
	if err != nil || result.Outcome.ReasonCode != "no_history" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if sum.calls != 0 || len(store.marked) != 0 {
		t.Fatalf("short history compacted: calls=%d marked=%v", sum.calls, store.marked)
	}
}

func TestCompactRejectsEmptyHistory(t *testing.T) {
	h := New(&fakeStore{}, &fakeSummarizer{markdown: "# Hi"})
	result, err := h.Execute(context.Background(), commands.Request{Principal: principal(), Raw: "/compact"})
	if err != nil || result.Outcome.ReasonCode != "no_history" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
