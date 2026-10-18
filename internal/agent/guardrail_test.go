package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestProcessWarnsOnExactFailureRepeat(t *testing.T) {
	args := map[string]interface{}{"q": "same"}
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("first", "test.flaky", args),
		toolCallResponse("retry", "test.flaky", args),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := registerTestTool(t, reg, testToolSpec{Name: "test.flaky", Description: "Flaky"}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{}, errors.New("boom")
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	if _, err := processAgent(agent, "req-1", "imessage", "session-1", "user-1", "User", "go", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) < 3 {
		t.Fatalf("model calls=%d, want at least 3", len(requests))
	}
	retry := toolResultByID(requests[2].Messages, "retry")
	if retry == nil {
		t.Fatalf("retry result missing: %+v", requests[2].Messages)
	}
	if !strings.Contains(retry.Content, "boom") {
		t.Fatalf("retry lost original error: %q", retry.Content)
	}
	if !strings.Contains(retry.Content, "repeated_exact_failure_warning") || !strings.Contains(retry.Content, "count=2") {
		t.Fatalf("retry missing exact-failure warn: %q", retry.Content)
	}
}

func TestProcessStubIsTransientOnly(t *testing.T) {
	full := strings.Repeat("y", 600)
	args := map[string]interface{}{"q": "same"}
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("first", "test.big", args),
		toolCallResponse("second", "test.big", args),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	// Identical calls always execute, so the streak/stub path is exercised.
	if err := registerTestTool(t, reg, testToolSpec{Name: "test.big", Description: "Big"}, governance.ToolPolicy{}, func(context.Context, map[string]interface{}) (governance.Result, error) {
		return productiveResult(full), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, store := newTestAgent(t, chat, nil, reg)
	if _, err := processAgent(agent, "req-1", "imessage", "session-1", "user-1", "User", "go", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) < 3 {
		t.Fatalf("model calls=%d, want at least 3", len(requests))
	}
	second := toolResultByID(requests[2].Messages, "second")
	if second == nil {
		t.Fatalf("second result missing: %+v", requests[2].Messages)
	}
	if !strings.Contains(second.Content, "byte-identical") {
		t.Fatalf("second result missing stub: %q", second.Content)
	}
	if strings.Contains(second.Content, full) {
		t.Fatal("transient stub leaked full text into model-visible message")
	}
	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil || len(turns) != 1 {
		t.Fatalf("stored turns=%+v err=%v", turns, err)
	}
	var persisted []string
	for _, batch := range turns[0].ToolHistory.Batches {
		for _, call := range batch.Calls {
			persisted = append(persisted, call.Result)
		}
	}
	if len(persisted) != 2 || persisted[0] != full || persisted[1] != full {
		got := []int{}
		for _, p := range persisted {
			got = append(got, len(p))
		}
		t.Fatalf("durable history must keep full text, got %d calls lens=%v", len(persisted), got)
	}
}

func TestProcessIntraBatchRepeatExecutes(t *testing.T) {
	args := map[string]interface{}{"q": "same"}
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "one", Function: llm.ToolFunction{Name: "test.once", Arguments: args}},
			{ID: "two", Function: llm.ToolFunction{Name: "test.once", Arguments: args}},
		}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	calls := 0
	if err := registerTestTool(t, reg, testToolSpec{Name: "test.once", Description: "Once"}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		calls++
		return productiveResult("ok"), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	if _, err := processAgent(agent, "req-1", "imessage", "session-1", "user-1", "User", "go", nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("handler invocations=%d, want 2", calls)
	}
	requests := primaryRequests(chat.requests)
	second := toolResultByID(requests[1].Messages, "two")
	if second == nil || second.Content != "ok" {
		t.Fatalf("second in-batch call did not execute: %+v", requests[1].Messages)
	}
}
