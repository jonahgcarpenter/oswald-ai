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
}

func (f *fakeStore) ResolveSessionContext(context.Context, string, string, time.Duration) (memory.SessionContext, error) {
	return memory.SessionContext{Generation: f.generation}, nil
}

func (f *fakeStore) RecentCompletedExchangesAfter(context.Context, string, string, int, int64, int) ([]memory.SessionTurn, error) {
	return f.turns, nil
}

type fakeSummarizer struct {
	markdown string
	calls    int
	set      string
}

func (f *fakeSummarizer) Summarize(context.Context, []memory.SessionTurn) (string, *llm.ChatResponse, error) {
	f.calls++
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
	store := &fakeStore{generation: 3, turns: []memory.SessionTurn{{ID: 1, UserText: "hi", AssistantText: "hello"}}}
	sum := &fakeSummarizer{markdown: "# Summary"}
	h := New(store, sum)
	result, err := h.Execute(context.Background(), commands.Request{Principal: principal(), SessionKey: "s", Raw: "/compact"})
	if err != nil || sum.calls != 1 || sum.set != "# Summary" {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, sum.calls)
	}
	if !strings.Contains(result.Text, "1 turn") || result.Outcome.Operation != "session.compact" || !result.Outcome.IsChanged {
		t.Fatalf("result=%+v", result)
	}
}

func TestCompactRejectsEmptyHistory(t *testing.T) {
	h := New(&fakeStore{}, &fakeSummarizer{markdown: "# Hi"})
	result, err := h.Execute(context.Background(), commands.Request{Principal: principal(), Raw: "/compact"})
	if err != nil || result.Outcome.ReasonCode != "no_history" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
