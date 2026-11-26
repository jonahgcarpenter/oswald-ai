package compact

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

type sessionModelChatter struct {
	lastModel string
	lastMsgs  []llm.ChatMessage
}

func (c *sessionModelChatter) Chat(_ context.Context, req llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	c.lastModel = req.Model
	c.lastMsgs = req.Messages
	return &llm.ChatResponse{Model: req.Model, Message: llm.ChatMessage{Role: "assistant", Content: "session summary"}}, nil
}

type sessionModelRouter struct {
	target agent.Target
}

func (r *sessionModelRouter) Resolve(context.Context, string, string, string, string) (agent.Target, []agent.LinkDegradation) {
	return r.target, nil
}

func TestCompactUsesSessionModel(t *testing.T) {
	client := &sessionModelChatter{}
	router := &sessionModelRouter{target: agent.Target{Model: "muse-spark-1.3", Provider: "bifrost", Client: client, BillingBaseURL: "https://example.com/v1"}}
	store := &fakeStore{generation: 2, turns: bigCompactTurns(20)}
	sum := &fakeSummarizer{markdown: "# fallback", cached: "# Summary\nPrior decisions."}
	h := New(store, sum, router)
	result, err := h.Execute(context.Background(), commands.Request{Principal: principal(), SessionKey: "s", Raw: "/compact"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Outcome.ReasonCode == "no_history" {
		t.Fatalf("no history: %+v", result)
	}
	if client.lastModel != "muse-spark-1.3" {
		t.Fatalf("compaction model = %q, want session model", client.lastModel)
	}
	if len(client.lastMsgs) != 2 || !strings.Contains(client.lastMsgs[1].Content, "Prior decisions.") {
		t.Fatalf("previous summary not merged: %+v", client.lastMsgs)
	}
	if sum.calls != 0 {
		t.Fatalf("fallback summarizer used: calls=%d", sum.calls)
	}
}

func TestCompactFallsBackWithoutRouter(t *testing.T) {
	store := &fakeStore{generation: 1, turns: []memory.SessionTurn{
		{ID: 3, UserText: "c", AssistantText: "C"},
		{ID: 2, UserText: "b", AssistantText: "B"},
		{ID: 1, UserText: "a", AssistantText: "A"},
	}}
	_ = identity.Principal{}
	h := New(store, &fakeSummarizer{markdown: "# Hi"})
	result, err := h.Execute(context.Background(), commands.Request{Principal: principal(), SessionKey: "s", Raw: "/compact"})
	if err != nil || result.Outcome.ReasonCode != "no_history" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
