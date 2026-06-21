package agent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
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
	if len(chat.requests) != 1 {
		t.Fatalf("expected one chat call, got %d", len(chat.requests))
	}
	lastMessage := chat.requests[0].Messages[len(chat.requests[0].Messages)-1]
	if len(lastMessage.Images) != 1 {
		t.Fatalf("expected current-turn image in prompt, got %+v", lastMessage.Images)
	}

	turns := store.Turns("session-1")
	if len(turns) != 1 {
		t.Fatalf("expected one persisted turn, got %d", len(turns))
	}
	wantUser := "new prompt\n\n[Attached 1 image(s)]"
	if turns[0].User.Content != wantUser || turns[0].Assistant.Content != "final answer" {
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
	if len(chat.requests) != 2 {
		t.Fatalf("expected two chat calls, got %d", len(chat.requests))
	}
	secondMessages := chat.requests[1].Messages
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
	if len(chat.requests) != 2 {
		t.Fatalf("expected final no-tools call, got %d calls", len(chat.requests))
	}
	if len(chat.requests[1].Tools) != 0 {
		t.Fatalf("expected tools disabled, got %+v", chat.requests[1].Tools)
	}
}

func TestProcessUsesFakeEmbeddingsForSemanticMemory(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "new answer"}}}}
	embedder := &fakeEmbedder{vectors: [][]float64{{1, 0}, {1, 0}}}
	agent, store := newTestAgent(t, chat, embedder, nil)
	agent.embeddingModel = "embed-model"
	store.ReplaceTurns("session-1", []memory.Turn{
		{CreatedAt: time.Now().Add(-2 * time.Minute), User: llm.ChatMessage{Role: "user", Content: "irrelevant"}, Assistant: llm.ChatMessage{Role: "assistant", Content: "old a"}, Embedding: []float64{0, 1}},
		{CreatedAt: time.Now().Add(-1 * time.Minute), User: llm.ChatMessage{Role: "user", Content: "relevant"}, Assistant: llm.ChatMessage{Role: "assistant", Content: "old b"}, Embedding: []float64{1, 0}},
	})

	_, err := agent.Process("req-1", "websocket", "session-1", "user-1", "Display", "follow up", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(embedder.inputs) != 2 {
		t.Fatalf("expected query and turn embeddings, got %d", len(embedder.inputs))
	}
	messages := chat.requests[0].Messages
	foundRelevant := false
	foundIrrelevant := false
	for _, msg := range messages {
		if msg.Content == "relevant" {
			foundRelevant = true
		}
		if msg.Content == "irrelevant" {
			foundIrrelevant = true
		}
	}
	if !foundRelevant || foundIrrelevant {
		t.Fatalf("expected only relevant history, got messages %+v", messages)
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

func newTestAgent(t *testing.T, chat llm.Chatter, embedder llm.Embedder, reg *registry.Registry) (*Agent, *memory.Store) {
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
	userStore := usermemory.NewStore(filepath.Join(dir, "users"), log)
	store := memory.NewStore(memory.Options{}, log)
	agent := NewAgent(chat, embedder, reg, "test-model", "", soulStore, userStore, memory.ContextBudget{PromptLimit: 100000}, 3, time.Minute, store, log)
	return agent, store
}
