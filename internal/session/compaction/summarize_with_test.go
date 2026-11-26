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
	markdown, resp, err := SummarizeWithClient(context.Background(), client, "muse-spark-1.3", turns, "")
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
	if strings.Contains(client.lastMsgs[1].Content, "Previous summary") {
		t.Fatalf("empty previous leaked into prompt: %q", client.lastMsgs[1].Content)
	}
}

func TestSummarizeWithClientMergesPreviousSummary(t *testing.T) {
	client := &recordingChatter{}
	turns := []memory.SessionTurn{{ID: 7, UserText: "newer", AssistantText: "work"}}
	markdown, _, err := SummarizeWithClient(context.Background(), client, "muse-spark-1.3", turns, "# Summary\nOld decisions.")
	if err != nil || !strings.Contains(markdown, "summary") {
		t.Fatalf("summarize=%q err=%v", markdown, err)
	}
	userMsg := client.lastMsgs[1].Content
	if !strings.Contains(userMsg, "Previous summary to preserve and extend") || !strings.Contains(userMsg, "Old decisions.") || !strings.Contains(userMsg, `"turn_id":7`) {
		t.Fatalf("merge prompt missing previous summary or new turns: %q", userMsg)
	}
}

func TestSummarizeWithClientProjectsLongToolResults(t *testing.T) {
	client := &recordingChatter{}
	big := strings.Repeat("r", 9000)
	turns := []memory.SessionTurn{{ID: 3, UserText: "u", AssistantText: "a", ToolHistory: memory.ToolHistory{Version: memory.ToolHistoryVersion, Batches: []memory.ToolHistoryBatch{{Calls: []memory.ToolHistoryCall{{Name: "terminal", Status: "succeeded", Result: big}}}}}}}
	if _, _, err := SummarizeWithClient(context.Background(), client, "muse-spark-1.3", turns, ""); err != nil {
		t.Fatal(err)
	}
	payload := client.lastMsgs[1].Content
	if strings.Contains(payload, big) {
		t.Fatal("summarizer input carried the full tool result")
	}
	if !strings.Contains(payload, "trimmed") {
		t.Fatalf("summarizer input missing truncation marker: %.200s", payload)
	}
}
