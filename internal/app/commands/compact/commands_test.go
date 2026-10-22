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
	calls    int
	set      string
	got      int
}

func (f *fakeSummarizer) Summarize(_ context.Context, turns []memory.SessionTurn) (string, *llm.ChatResponse, error) {
	f.calls++
	f.got = len(turns)
	return f.markdown, &llm.ChatResponse{Model: "m"}, nil
}

func (f *fakeSummarizer) Set(_, _ string, _ int, markdown string) { f.set = markdown }

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

func TestCompactSummarizesAndStores(t *testing.T) {
	store := &fakeStore{generation: 3, turns: []memory.SessionTurn{
		{ID: 5, UserText: "newest", AssistantText: "answer5"},
		{ID: 4, UserText: "newer", AssistantText: "answer4"},
		{ID: 3, UserText: "keep", AssistantText: "answer3"},
		{ID: 2, UserText: "old2", AssistantText: "answer2"},
		{ID: 1, UserText: "old1", AssistantText: "answer1"},
	}}
	sum := &fakeSummarizer{markdown: "# Summary"}
	h := New(store, sum)
	result, err := h.Execute(context.Background(), commands.Request{Principal: principal(), SessionKey: "s", Raw: "/compact"})
	if err != nil || sum.calls != 1 || sum.set != "# Summary" {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, sum.calls)
	}
	if sum.got != 2 {
		t.Fatalf("summarized %d turns, want oldest 2: marked=%v", sum.got, store.marked)
	}
	if len(store.marked) != 2 || store.marked[0] != 2 || store.marked[1] != 1 {
		t.Fatalf("marked=%v, want [2 1]", store.marked)
	}
	if !strings.Contains(result.Text, "2 turns") || result.Outcome.Operation != "session.compact" || !result.Outcome.IsChanged || result.Outcome.AffectedCount != 2 {
		t.Fatalf("result=%+v", result)
	}
}

func TestCompactKeepsNewestThreeVerbatim(t *testing.T) {
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
