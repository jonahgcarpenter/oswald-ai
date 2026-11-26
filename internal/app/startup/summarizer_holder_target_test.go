package startup

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

type recordingTargetChatter struct {
	lastModel string
	lastMsgs  []llm.ChatMessage
}

func (c *recordingTargetChatter) Chat(_ context.Context, req llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	c.lastModel = req.Model
	c.lastMsgs = req.Messages
	return &llm.ChatResponse{Model: req.Model, Message: llm.ChatMessage{Role: "assistant", Content: "target summary"}}, nil
}

func TestSummarizerHolderUsesSessionTarget(t *testing.T) {
	pinned := &fakeSummarizerTarget{name: "pinned"}
	holder := newSummarizerHolder(pinned)
	client := &recordingTargetChatter{}
	turns := []memory.SessionTurn{{ID: 1, UserText: "u", AssistantText: "a"}}
	target := agent.Target{Model: "muse-spark-1.3", Provider: "bifrost", Client: client, BillingBaseURL: "https://example.com/v1"}
	markdown, resp, err := holder.SummarizeWithTarget(context.Background(), turns, target, "# Summary\nPrior.")
	if err != nil || markdown != "target summary" {
		t.Fatalf("summarize=%q err=%v", markdown, err)
	}
	if client.lastModel != "muse-spark-1.3" {
		t.Fatalf("model = %q, want session target", client.lastModel)
	}
	if resp == nil || resp.Model != "muse-spark-1.3" {
		t.Fatalf("response = %+v", resp)
	}
	if len(client.lastMsgs) != 2 || !strings.Contains(client.lastMsgs[1].Content, "Prior.") {
		t.Fatalf("previous summary not forwarded: %+v", client.lastMsgs)
	}
	if calls, _, _ := pinned.snapshot(); calls != 0 {
		t.Fatalf("pinned summarizer used: calls=%d", calls)
	}
}

func TestSummarizerHolderFallsBackWithoutTargetClient(t *testing.T) {
	pinned := &fakeSummarizerTarget{name: "pinned"}
	holder := newSummarizerHolder(pinned)
	markdown, _, err := holder.SummarizeWithTarget(context.Background(), nil, agent.Target{}, "")
	if err != nil || markdown != "pinned" {
		t.Fatalf("fallback summarize=%q err=%v", markdown, err)
	}
}
