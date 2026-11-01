package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func steerTestPrincipal() identity.Principal {
	return identity.Principal{CanonicalUserID: "user-1", Gateway: "imessage", ExternalID: "user-1", Assurance: identity.AssuranceBlueBubblesWebhook}
}

func TestSteerInjectedAfterToolResultCreatesSecondCall(t *testing.T) {
	inbox := NewSteerInbox()
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "working", ToolCalls: []llm.ToolCall{{ID: "call-1", Function: llm.ToolFunction{Name: "test.clock"}}}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done with correction"}},
	}}
	a, _ := newTestAgent(t, chat, nil, nil)
	reg := a.registry
	if err := registerTestTool(t, reg, testToolSpec{Name: "test.clock", Schema: &llm.ToolParameters{Type: "object"}}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		inbox.Add("correction: use blue", "steer-1")
		return productiveResult("clock: noon"), nil
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := a.Process(context.Background(), Request{RequestID: "host", Principal: steerTestPrincipal(), SessionKey: "session", Prompt: "start task", Steer: inbox})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Response != "done with correction" {
		t.Fatalf("response = %+v", resp)
	}
	if got := len(chat.requests); got != 2 {
		t.Fatalf("model calls = %d, want 2 (no dead call, steer creates exactly one more)", got)
	}
	second := chat.requests[1].Messages
	if !messagesContain(second, "correction: use blue") {
		t.Fatalf("second call missing steer: %+v", second)
	}
}

func TestSteerForcesSecondCallWhenModelDone(t *testing.T) {
	inbox := NewSteerInbox()
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "first answer"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "revised answer"}},
	}}
	chat.onChat = func(req llm.ChatRequest) {
		if len(chat.requests) == 1 {
			inbox.Add("wait, use blue instead", "steer-1")
		}
	}
	a, _ := newTestAgent(t, chat, nil, nil)
	resp, err := a.Process(context.Background(), Request{RequestID: "host", Principal: steerTestPrincipal(), SessionKey: "session", Prompt: "question", Steer: inbox})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Response != "revised answer" {
		t.Fatalf("response = %+v", resp)
	}
	if got := len(chat.requests); got != 2 {
		t.Fatalf("model calls = %d, want 2 (steer forces second call, never swallowed)", got)
	}
	if !messagesContain(chat.requests[1].Messages, "wait, use blue instead") {
		t.Fatalf("second call missing steer: %+v", chat.requests[1].Messages)
	}
}

func TestSteerEmptyDoesNotCreateDeadCall(t *testing.T) {
	inbox := NewSteerInbox()
	inbox.Add("   ", "steer-empty")
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "final"}},
	}}
	a, _ := newTestAgent(t, chat, nil, nil)
	resp, err := a.Process(context.Background(), Request{RequestID: "host", Principal: steerTestPrincipal(), SessionKey: "session", Prompt: "question", Steer: inbox})
	if err != nil || resp == nil || resp.Response != "final" {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	if got := len(chat.requests); got != 1 {
		t.Fatalf("model calls = %d, want 1 (empty steer never fires a dead call)", got)
	}
}

func TestSteerPersistedInSingleTurn(t *testing.T) {
	inbox := NewSteerInbox()
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "answer"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "revised"}},
	}}
	chat.onChat = func(req llm.ChatRequest) {
		if len(chat.requests) == 1 {
			inbox.Add("follow-up detail", "steer-1")
		}
	}
	a, store := newTestAgent(t, chat, nil, nil)
	resp, err := a.Process(context.Background(), Request{RequestID: "host", Principal: steerTestPrincipal(), SessionKey: "persist-session", Prompt: "original", Steer: inbox})
	if err != nil || resp == nil || resp.SourceTurnID == 0 {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	turns, err := store.RecentSessionTurns("user-1", "persist-session", 1, 10)
	if err != nil || len(turns) != 1 {
		t.Fatalf("turns=%+v err=%v", turns, err)
	}
	if !strings.Contains(turns[0].UserText, "original") || !strings.Contains(turns[0].UserText, "follow-up detail") {
		t.Fatalf("persisted turn missing steer: %q", turns[0].UserText)
	}
}
