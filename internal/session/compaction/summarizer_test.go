package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

type fakeChatter struct {
	requests []llm.ChatRequest
	response string
}

func (f *fakeChatter) Chat(_ context.Context, req llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.requests = append(f.requests, req)
	return &llm.ChatResponse{Model: req.Model, Message: llm.ChatMessage{Role: "assistant", Content: f.response}}, nil
}

func TestSummarizeUsesSystemJSONUserAndZeroTools(t *testing.T) {
	chat := &fakeChatter{response: "# Summary\n- hello"}
	s, err := NewSummarizer(chat, "fake-model")
	if err != nil {
		t.Fatal(err)
	}
	turns := []memory.SessionTurn{{ID: 1, UserText: "hi", AssistantText: "hello"}}
	markdown, _, err := s.Summarize(context.Background(), turns)
	if err != nil || markdown != "# Summary\n- hello" {
		t.Fatalf("markdown=%q err=%v", markdown, err)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("requests=%d", len(chat.requests))
	}
	req := chat.requests[0]
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("messages=%+v", req.Messages)
	}
	if len(req.Tools) != 0 {
		t.Fatalf("tools=%+v", req.Tools)
	}
	if !strings.Contains(req.Messages[0].Content, "compaction") && !strings.Contains(req.Messages[0].Content, "Summarize") {
		t.Fatalf("system prompt missing: %q", req.Messages[0].Content)
	}
	if !strings.Contains(req.Messages[1].Content, `"turn_id":1`) || !strings.Contains(req.Messages[1].Content, "hi") {
		t.Fatalf("session JSON missing: %q", req.Messages[1].Content)
	}
}

func TestSummarizerCacheGetSet(t *testing.T) {
	s, _ := NewSummarizer(&fakeChatter{}, "m")
	if got := s.Get("u", "s", 1); got != "" {
		t.Fatalf("empty cache=%q", got)
	}
	s.Set("u", "s", 1, "  # Hi  ")
	if got := s.Get("u", "s", 1); got != "# Hi" && got != "  # Hi  " {
		t.Fatalf("cache=%q", got)
	}
	s.Set("u", "s", 1, "   ")
	if got := s.Get("u", "s", 1); got != "" {
		t.Fatalf("clear failed=%q", got)
	}
}

func TestRenderMarkdownSummaryWrapsUntrusted(t *testing.T) {
	if RenderMarkdownSummary("  ") != "" {
		t.Fatal("empty markdown rendered")
	}
	got := RenderMarkdownSummary("# Hi")
	if !strings.Contains(got, "compacted_session_history") || !strings.Contains(got, "# Hi") {
		t.Fatalf("render=%q", got)
	}
	if strings.Contains(got, "Generated historical reference only") {
		t.Fatalf("static header was not removed: %q", got)
	}
}
