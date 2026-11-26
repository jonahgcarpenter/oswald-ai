package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

type recordingChatter struct {
	lastModel string
	lastMsgs  []llm.ChatMessage
}

func (c *recordingChatter) Chat(_ context.Context, req llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	c.lastModel = req.Model
	c.lastMsgs = req.Messages
	return &llm.ChatResponse{Model: req.Model, Message: llm.ChatMessage{Role: "assistant", Content: "summary markdown"}}, nil
}

func TestSummarizeWithClientUsesSessionModel(t *testing.T) {
	client := &recordingChatter{}
	turns := []memory.SessionTurn{{ID: 1, UserText: "u", AssistantText: "a"}}
	markdown, resp, err := SummarizeWithClient(context.Background(), client, "muse-spark-1.3", turns)
	if err != nil || !strings.Contains(markdown, "summary") {
		t.Fatalf("summarize=%q err=%v", markdown, err)
	}
	if client.lastModel != "muse-spark-1.3" {
		t.Fatalf("model = %q, want session model", client.lastModel)
	}
	if resp == nil || resp.Model != "muse-spark-1.3" {
		t.Fatalf("response model = %+v", resp)
	}
	if len(client.lastMsgs) != 2 || client.lastMsgs[0].Content != SummarizerSystemPrompt {
		t.Fatalf("messages = %+v", client.lastMsgs)
	}
}
