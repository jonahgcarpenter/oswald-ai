package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestProcessFinalAnswerPersistsCleanedSessionMemory(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "final answer"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)

	resp, err := agent.Process("req-1", "websocket", "session-1", "user-1", "Display", "[Replying to Alice: \"old\"]\n\nnew prompt", []llm.InputImage{{MimeType: "image/jpeg", Data: "abc"}}, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "final answer" {
		t.Fatalf("unexpected response %q", resp.Response)
	}
	primary := primaryRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one primary chat call, got %d", len(primary))
	}
	lastMessage := primary[0].Messages[len(primary[0].Messages)-1]
	if len(lastMessage.Images) != 1 {
		t.Fatalf("expected current-turn image in prompt, got %+v", lastMessage.Images)
	}

	turns, err := store.RecentSessionTurns("session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected one persisted turn, got %d", len(turns))
	}
	wantUser := "new prompt\n\n[Attached 1 image(s)]"
	if turns[0].UserText != wantUser || turns[0].AssistantText != "final answer" {
		t.Fatalf("unexpected stored turn: %+v", turns[0])
	}
}

func TestProcessExecutesToolThenFinalAnswerAndStreamsEvents(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Thinking: "thinking", ToolCalls: []llm.ToolCall{{ID: "call-1", Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{"q": "oswald"}}}}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "tool-backed answer"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := reg.RegisterTool(registry.Spec{Name: "test.lookup", Description: "Lookup", Parameters: []registry.ParamSpec{{Name: "q", Type: "string", Required: true}}}, func(_ context.Context, args map[string]interface{}) (string, error) {
		if args["q"] != "oswald" {
			t.Fatalf("unexpected tool args: %+v", args)
		}
		return "lookup result", nil
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)

	var chunks []StreamChunk
	resp, err := agent.Process("req-1", "websocket", "session-1", "user-1", "Display", "question", nil, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "tool-backed answer" || resp.Thinking != "thinking" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	primary := primaryRequests(chat.requests)
	if len(primary) != 2 {
		t.Fatalf("expected two primary chat calls, got %d", len(primary))
	}
	secondMessages := primary[1].Messages
	toolMsg := secondMessages[len(secondMessages)-1]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call-1" || toolMsg.Content != "lookup result" {
		t.Fatalf("unexpected tool message: %+v", toolMsg)
	}
	toolCallIndex := -1
	toolResultIndex := -1
	for i, chunk := range chunks {
		if chunk.Type == ChunkToolCall {
			toolCallIndex = i
		}
		if chunk.Type == ChunkToolResult {
			toolResultIndex = i
		}
	}
	if toolCallIndex < 0 || toolResultIndex < 0 || toolResultIndex <= toolCallIndex || chunks[toolResultIndex].Tool.ResultText != "lookup result" {
		t.Fatalf("unexpected stream chunks: %+v", chunks)
	}
}

func TestProcessDisablesToolsAfterFailureBudget(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1", Function: llm.ToolFunction{Name: "test.fail"}}}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "finished without tools"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := reg.RegisterTool(registry.Spec{Name: "test.fail", Description: "Fail"}, func(context.Context, map[string]interface{}) (string, error) {
		return "", errors.New("boom")
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	agent.maxToolFailureRetries = 1

	resp, err := agent.Process("req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "finished without tools" {
		t.Fatalf("unexpected response %q", resp.Response)
	}
	primary := primaryRequests(chat.requests)
	if len(primary) != 2 {
		t.Fatalf("expected final no-tools call, got %d calls", len(primary))
	}
	if len(primary[1].Tools) != 0 {
		t.Fatalf("expected tools disabled, got %+v", primary[1].Tools)
	}
}

func TestProcessRetriesEmptyVisibleResponse(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Thinking: "reasoning only"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "visible answer"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := reg.RegisterTool(registry.Spec{Name: "test.lookup", Description: "Lookup"}, func(context.Context, map[string]interface{}) (string, error) {
		return "lookup result", nil
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	agent, store := newTestAgent(t, chat, nil, reg)

	resp, err := agent.Process("req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "visible answer" {
		t.Fatalf("unexpected response %q", resp.Response)
	}
	primary := primaryRequests(chat.requests)
	if len(primary) != 2 {
		t.Fatalf("expected retry chat call, got %d calls", len(primary))
	}
	if len(primary[1].Tools) != 0 {
		t.Fatalf("expected retry with tools disabled, got %+v", primary[1].Tools)
	}
	lastMessage := primary[1].Messages[len(primary[1].Messages)-1]
	if lastMessage.Role != "user" || lastMessage.Content != emptyResponseRetryPrompt {
		t.Fatalf("unexpected retry prompt: %+v", lastMessage)
	}

	turns, err := store.RecentSessionTurns("session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].AssistantText != "visible answer" {
		t.Fatalf("unexpected stored turn: %+v", turns)
	}
}

func TestProcessFallsBackAfterEmptyRetry(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Thinking: "reasoning only"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Thinking: "still reasoning"}},
	}}
	agent, store := newTestAgent(t, chat, nil, nil)

	var chunks []StreamChunk
	resp, err := agent.Process("req-1", "websocket", "session-1", "user-1", "Display", "question", nil, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != emptyResponseFallback {
		t.Fatalf("unexpected response %q", resp.Response)
	}
	foundFallbackChunk := false
	for _, chunk := range chunks {
		if chunk.Type == ChunkContent && chunk.Text == emptyResponseFallback {
			foundFallbackChunk = true
		}
	}
	if !foundFallbackChunk {
		t.Fatalf("expected fallback content chunk, got %+v", chunks)
	}

	turns, err := store.RecentSessionTurns("session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].AssistantText != emptyResponseFallback {
		t.Fatalf("unexpected stored turn: %+v", turns)
	}
}

func TestProcessIncludesRecentSessionContextWithoutSemanticLookup(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "new answer"}}}}
	embedder := &fakeEmbedder{vectors: [][]float64{{0, 1}, {1, 0}, {0, 1}, {0, 1}, {0, 1}, {0, 1}}}
	agent, store := newTestAgent(t, chat, embedder, nil)
	if err := store.AppendSessionTurn(context.Background(), "session-1", "user-1", "older unrelated", "old a", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurn(context.Background(), "session-1", "user-1", "older relevant", "old b", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"recent one", "recent two", "recent three", "recent four"} {
		if err := store.AppendSessionTurn(context.Background(), "session-1", "user-1", text, "recent answer", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	seedEmbeddingCount := len(embedder.inputs)

	_, err := agent.Process("req-1", "websocket", "session-1", "user-1", "Display", "follow up", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	newEmbeddings := embedder.inputs[seedEmbeddingCount:]
	if len(newEmbeddings) != 0 {
		t.Fatalf("expected no automatic embeddings during request, got %d inputs: %+v", len(newEmbeddings), newEmbeddings)
	}
	messages := primaryRequests(chat.requests)[0].Messages
	foundRecent := false
	foundOlder := false
	for _, msg := range messages {
		if msg.Content != "" && contains(msg.Content, "recent four") {
			foundRecent = true
		}
		if msg.Content != "" && contains(msg.Content, "older relevant") {
			foundOlder = true
		}
	}
	if !foundRecent || foundOlder {
		t.Fatalf("expected recent session context only, got messages %+v", messages)
	}
}

func TestProcessAddsIMessagePlainTextSystemInstruction(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ok"}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	_, err := agent.Process("req-1", "imessage", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	system := primaryRequests(chat.requests)[0].Messages[0]
	if system.Role != "system" || !strings.Contains(system.Content, "iMessage") || !strings.Contains(system.Content, "does not render Markdown") {
		t.Fatalf("missing imessage system instruction: %+v", system)
	}
}

func TestProcessDoesNotAddIMessageSystemInstructionForOtherGateways(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ok"}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	_, err := agent.Process("req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	system := primaryRequests(chat.requests)[0].Messages[0]
	if strings.Contains(system.Content, "does not render Markdown") {
		t.Fatalf("unexpected imessage system instruction: %+v", system)
	}
}

func TestSessionMemoryUserContentReplyOnly(t *testing.T) {
	got := sessionMemoryUserContent("[Replying to Alice: \"old\"]", 0)
	if got != "[User replied to a prior message]" {
		t.Fatalf("unexpected content %q", got)
	}
}

type fakeChatter struct {
	responses []*llm.ChatResponse
	requests  []llm.ChatRequest
}

func (f *fakeChatter) Chat(_ context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		return nil, errors.New("no fake response")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	if cb != nil {
		if resp.Message.Thinking != "" {
			cb(llm.ChatMessage{Thinking: resp.Message.Thinking})
		}
		if resp.Message.Content != "" {
			cb(llm.ChatMessage{Content: resp.Message.Content})
		}
	}
	return resp, nil
}

type fakeEmbedder struct {
	vectors [][]float64
	inputs  []string
}

func (f *fakeEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	f.inputs = append(f.inputs, req.Input)
	if len(f.vectors) == 0 {
		return nil, errors.New("no fake embedding")
	}
	vec := f.vectors[0]
	f.vectors = f.vectors[1:]
	return &llm.EmbedResponse{Model: req.Model, Embeddings: [][]float64{vec}}, nil
}

func newTestAgent(t *testing.T, chat llm.Chatter, embedder llm.Embedder, reg *registry.Registry) (*Agent, *usermemory.Store) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	if reg == nil {
		reg = registry.New(log)
	}
	dir := t.TempDir()
	soulStore := soul.NewStore(filepath.Join(dir, "soul.md"), log)
	if err := soulStore.Write("You are Oswald."); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	userStore, err := usermemory.NewSQLiteStore(filepath.Join(dir, "oswald.db"), embedder, "embed-model", log)
	if err != nil {
		t.Fatalf("user store: %v", err)
	}
	agent := NewAgent(chat, reg, "test-model", soulStore, userStore, promptbudget.ContextBudget{PromptLimit: 100000}, 3, time.Minute, log)
	return agent, userStore
}

func primaryRequests(requests []llm.ChatRequest) []llm.ChatRequest {
	out := make([]llm.ChatRequest, 0, len(requests))
	for _, req := range requests {
		if req.Format == "json_object" {
			continue
		}
		out = append(out, req)
	}
	return out
}

func contains(value, needle string) bool {
	return strings.Contains(value, needle)
}
