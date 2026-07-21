package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/indexruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestProcessFinalAnswerPersistsCleanedSessionMemory(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "final answer"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)

	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "[Replying to Alice: \"old\"]\n\nnew prompt", []llm.InputImage{testInputImage(t, 800, 600)}, nil)
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

	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
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

func TestMemoryToolStreamPayloadsUseScopeExplicitKeys(t *testing.T) {
	userPayload := toolStreamPayload(toolnames.UserMemorySave, map[string]interface{}{"statement": "The user prefers concise replies.", "evidence": "I prefer concise replies"}, "accepted", time.Millisecond, false)
	if userPayload.UserMemory == nil || userPayload.UserMemory.Action != "save" || userPayload.GlobalMemory != nil {
		t.Fatalf("unexpected user memory payload: %+v", userPayload)
	}
	globalPayload := toolStreamPayload(toolnames.GlobalMemorySave, map[string]interface{}{"statement": "Oswald is written in Go.", "evidence": "language: Go"}, "accepted", time.Millisecond, false)
	if globalPayload.GlobalMemory == nil || globalPayload.GlobalMemory.Action != "save" || globalPayload.UserMemory != nil {
		t.Fatalf("unexpected global memory payload: %+v", globalPayload)
	}

	for _, payload := range []*ToolStreamPayload{userPayload, globalPayload} {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"memory":`) {
			t.Fatalf("legacy memory stream key was emitted: %s", encoded)
		}
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
	agent, store := newTestAgent(t, chat, nil, reg)

	var chunks []StreamChunk
	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, func(chunk StreamChunk) {
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
	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || strings.Join(turns[0].ToolNames, ",") != "test.lookup" {
		t.Fatalf("successful tool annotation was not persisted: %+v", turns)
	}
}

func TestProcessForcesOneCorrectiveMemorySaveRetry(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("save-1", toolnames.UserMemorySave, map[string]interface{}{"memories": []interface{}{map[string]interface{}{"claim_slot": "name"}}}),
		toolCallResponse("save-2", toolnames.UserMemorySave, map[string]interface{}{"memories": []interface{}{map[string]interface{}{"claim_slot": "identity.name"}}}),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "I saved the corrected memory."}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	calls := 0
	if err := reg.RegisterTool(registry.Spec{Name: toolnames.UserMemorySave, Description: "Save memories"}, func(context.Context, map[string]interface{}) (string, error) {
		calls++
		if calls == 1 {
			return `{"accepted_count":0,"rejected_count":1,"results":[{"index":0,"status":"rejected","reason":"semantic claim slot is incompatible","retryable":true}],"required_action":"retry"}`, nil
		}
		return `{"accepted_count":1,"rejected_count":0,"results":[{"index":0,"status":"accepted","reason":"accepted","retryable":false}]}`, nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	response, err := processAgent(agent, "retry-memory", "websocket", "session", "user-1", "Display", "My name is Jonah.", nil, nil)
	if err != nil || response.Response != "I saved the corrected memory." || calls != 2 {
		t.Fatalf("response=%+v calls=%d err=%v", response, calls, err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 3 {
		t.Fatalf("request count=%d", len(requests))
	}
	forced := requests[1]
	if len(forced.Tools) != 1 || forced.Tools[0].Function.Name != toolnames.UserMemorySave || forced.ToolChoice == nil || forced.ToolChoice.Function.Name != toolnames.UserMemorySave {
		t.Fatalf("corrective request was not forced to user_memory_save: %+v", forced)
	}
}

func TestProcessGuardsFinalAnswerAfterMemoryRetryExhaustion(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("save-1", toolnames.UserMemorySave, map[string]interface{}{"memories": []interface{}{map[string]interface{}{"evidence": "joined quote"}}}),
		toolCallResponse("save-2", toolnames.UserMemorySave, map[string]interface{}{"memories": []interface{}{map[string]interface{}{"evidence": "still wrong"}}}),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "I could not save that memory."}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := reg.RegisterTool(registry.Spec{Name: toolnames.UserMemorySave, Description: "Save memories"}, func(context.Context, map[string]interface{}) (string, error) {
		return `{"accepted_count":0,"rejected_count":1,"results":[{"index":0,"status":"rejected","reason":"evidence is not an exact quote","retryable":true}],"required_action":"retry"}`, nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	response, err := processAgent(agent, "failed-memory", "websocket", "session", "user-1", "Display", "I work for UAM.", nil, nil)
	if err != nil || response.Response != "I could not save that memory." {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 3 || !messagesContain(requests[2].Messages, memorySaveFailureGuard) {
		t.Fatalf("final request did not contain memory failure guard: %+v", requests)
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

	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
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

	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
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

	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
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
	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, func(chunk StreamChunk) {
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

	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].AssistantText != emptyResponseFallback {
		t.Fatalf("unexpected stored turn: %+v", turns)
	}
}

func TestProcessRetriesTemporaryOllamaParserErrorWithTools(t *testing.T) {
	parserErr := &llm.ChatHTTPError{StatusCode: 500, Body: `expected element type <function> but have <parameter>`}
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{err: parserErr},
		{response: &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "recovered"}}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := reg.RegisterTool(registry.Spec{Name: "test.lookup", Description: "Lookup"}, func(context.Context, map[string]interface{}) (string, error) {
		return "lookup result", nil
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)

	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "recovered" || resp.Error != "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	primary := primaryRequests(chat.requests)
	if len(primary) != 2 {
		t.Fatalf("expected two calls, got %d", len(primary))
	}
	if len(primary[0].Tools) == 0 || len(primary[1].Tools) != len(primary[0].Tools) {
		t.Fatalf("retry did not preserve tools: first=%+v retry=%+v", primary[0].Tools, primary[1].Tools)
	}
	if len(primary[1].Messages) != len(primary[0].Messages) || primary[1].Messages[len(primary[1].Messages)-1].Content != primary[0].Messages[len(primary[0].Messages)-1].Content {
		t.Fatalf("retry changed messages: first=%+v retry=%+v", primary[0].Messages, primary[1].Messages)
	}
}

func TestProcessUsesFriendlyFallbackAfterRepeatedOllamaParserError(t *testing.T) {
	parserErr := &llm.ChatHTTPError{StatusCode: 500, Body: `XML syntax error on line 7: unexpected EOF`}
	chat := &fakeChatter{outcomes: []fakeChatOutcome{{err: parserErr}, {err: parserErr}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	var chunks []StreamChunk

	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != emptyResponseFallback || resp.Error != "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	contentChunks := 0
	for _, chunk := range chunks {
		if chunk.Type == ChunkContent && chunk.Text == emptyResponseFallback {
			contentChunks++
		}
	}
	if contentChunks != 1 {
		t.Fatalf("fallback chunks = %d, want 1: %+v", contentChunks, chunks)
	}
	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].AssistantText != emptyResponseFallback {
		t.Fatalf("fallback turn was not persisted: %+v", turns)
	}
}

func TestProcessDoesNotRetryUnrelatedModelError(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{{err: &llm.ChatHTTPError{StatusCode: 500, Body: "out of memory"}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Error == "" || len(primaryRequests(chat.requests)) != 1 {
		t.Fatalf("unexpected response or retry: response=%+v calls=%d", resp, len(chat.requests))
	}
}

func TestProcessRetriesStoppedModelRunnerWithExponentiallySmallerImages(t *testing.T) {
	runnerErr := &llm.ChatHTTPError{StatusCode: 500, Body: `{"error":{"message":"model runner has unexpectedly stopped, this may be due to resource limitations"}}`}
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{err: runnerErr},
		{err: runnerErr},
		{response: &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "recovered"}}},
	}}
	agent, _ := newTestAgent(t, chat, nil, nil)
	input := testInputImage(t, 800, 600)

	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", []llm.InputImage{input}, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "recovered" || resp.Error != "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 3 {
		t.Fatalf("calls = %d, want 3", len(requests))
	}
	wants := []image.Point{{X: 800, Y: 600}, {X: 600, Y: 450}, {X: 450, Y: 338}}
	for i, req := range requests {
		got := inputImageDimensions(t, req.Messages[len(req.Messages)-1].Images[0])
		if got != wants[i] {
			t.Fatalf("attempt %d dimensions = %v, want %v", i+1, got, wants[i])
		}
	}
}

func TestProcessUsesImageSizeFallbackAfterFiveStoppedRunnerAttempts(t *testing.T) {
	runnerErr := &llm.ChatHTTPError{StatusCode: 500, Body: `model runner has unexpectedly stopped`}
	chat := &fakeChatter{outcomes: []fakeChatOutcome{{err: runnerErr}, {err: runnerErr}, {err: runnerErr}, {err: runnerErr}, {err: runnerErr}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	var chunks []StreamChunk

	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", []llm.InputImage{testInputImage(t, 800, 600)}, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != imageSizeFallback || resp.Error != "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != maxImageModelAttempts {
		t.Fatalf("calls = %d, want %d", len(chat.requests), maxImageModelAttempts)
	}
	if got := inputImageDimensions(t, requests[0].Messages[len(requests[0].Messages)-1].Images[0]); got != (image.Point{X: 800, Y: 600}) {
		t.Fatalf("first attempt dimensions = %v, want 800x600", got)
	}
	if got := inputImageDimensions(t, requests[4].Messages[len(requests[4].Messages)-1].Images[0]); got != (image.Point{X: 253, Y: 190}) {
		t.Fatalf("fifth attempt dimensions = %v, want 253x190", got)
	}
	contentChunks := 0
	for _, chunk := range chunks {
		if chunk.Type == ChunkContent && chunk.Text == imageSizeFallback {
			contentChunks++
		}
	}
	if contentChunks != 1 {
		t.Fatalf("fallback chunks = %d, want 1", contentChunks)
	}
	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil || len(turns) != 1 || turns[0].AssistantText != imageSizeFallback {
		t.Fatalf("fallback turn was not persisted: turns=%+v err=%v", turns, err)
	}
}

func TestProcessDoesNotRetryStoppedModelRunnerWithoutImages(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{{err: &llm.ChatHTTPError{StatusCode: 500, Body: `model runner has unexpectedly stopped`}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	resp, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Error == "" || len(primaryRequests(chat.requests)) != 1 {
		t.Fatalf("unexpected response or retry: response=%+v calls=%d", resp, len(chat.requests))
	}
}

func TestProcessFailsWhenTenantProfileCannotBeResolved(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "must not run"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil); err == nil {
		t.Fatal("expected profile resolution failure")
	}
	if len(chat.requests) != 0 {
		t.Fatalf("model called after profile resolution failed: %+v", chat.requests)
	}
}

func TestProcessIncludesRoleCorrectSessionContextWithAutomaticRecallLookup(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "new answer"}}}}
	embedder := &fakeEmbedder{vectors: [][]float64{{0, 1}, {1, 0}, {0, 1}, {0, 1}, {0, 1}, {0, 1}}}
	agent, store := newTestAgent(t, chat, embedder, nil)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, "older unrelated", "old a", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, "older relevant", "old b", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"recent one", "recent two", "recent three", "recent four"} {
		if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, text, "recent answer", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	_, err = processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "follow up", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(embedder.inputs) != 0 {
		t.Fatalf("semantic recall embedded without a live vector revision: %+v", embedder.inputs)
	}
	messages := primaryRequests(chat.requests)[0].Messages
	if len(messages) != 15 || messages[2].Role != "user" || messages[2].Content != "older unrelated" || messages[3].Role != "assistant" || messages[3].Content != "old a" {
		t.Fatalf("history roles or chronology are wrong: %+v", messages)
	}
	if messages[len(messages)-1].Role != "user" || messages[len(messages)-1].Content != "follow up" {
		t.Fatalf("current request is not the final user message: %+v", messages)
	}
}

func TestProcessUsesCommittedSummaryWithRecentVerbatimTail(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "continued"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, fmt.Sprintf("turn %d user", i), fmt.Sprintf("turn %d assistant", i), nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := store.DeliveredSessionTurnsAfter(context.Background(), "user-1", "session-1", profile.Generation, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user-1", "session-1", profile.Generation, turns.Turns[0].ID, turns.Turns[1].ID); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, usermemory.SummaryArtifact{Narrative: "The first two turns established Atlas.", OpenTasks: []string{"Continue"}, GenerationModel: "test-model", GeneratorVersion: "test-v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishSessionSummary(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSessionCompactionJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-summary", "websocket", "session-1", "user-1", "Display", "continue", nil, nil); err != nil {
		t.Fatal(err)
	}
	messages := primaryRequests(chat.requests)[0].Messages
	if len(messages) != 20 || !strings.Contains(messages[2].Content, "session_history_summary") || !strings.Contains(messages[2].Content, "first two turns") {
		t.Fatalf("summary context missing or malformed: %+v", messages)
	}
	if messages[3].Content != "turn 3 user" || messages[len(messages)-2].Content != "turn 10 assistant" || messages[len(messages)-1].Content != "continue" {
		t.Fatalf("recent verbatim tail is wrong: %+v", messages)
	}
}

func TestProcessInjectsTenantScopedRecallWithoutPersistingIt(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "Atlas."}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	_, err := store.SaveMemory(context.Background(), "user-1", usermemory.SaveRequest{
		Scope: usermemory.ScopeLongTerm, Category: "projects", Statement: "The project codename is Atlas.", Evidence: "The user named it.", Confidence: 0.95, Importance: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMemory(context.Background(), "user-2", usermemory.SaveRequest{
		Scope: usermemory.ScopeLongTerm, Category: "projects", Statement: "The private project codename is Borealis.", Evidence: "Another user's project.", Confidence: 1, Importance: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := indexruntime.NewService(store, nil, "", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err = processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "What is the project codename?", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	messages := primaryRequests(chat.requests)[0].Messages
	current := messages[len(messages)-1]
	if !strings.Contains(current.Content, "UNTRUSTED LOWER-AUTHORITY REFERENCE") || !strings.Contains(current.Content, "Atlas") {
		t.Fatalf("automatic recall missing from current user turn: %+v", current)
	}
	if strings.Contains(current.Content, "Borealis") || strings.Contains(messages[0].Content, "Atlas") {
		t.Fatalf("recall crossed tenant or authority boundary: %+v", messages)
	}
	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].UserText != "What is the project codename?" || strings.Contains(turns[0].UserText, "Atlas") {
		t.Fatalf("injected recall was persisted: %+v", turns)
	}
}

func TestProcessDoesNotConversationallyConfirmPendingMemory(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "model response"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	output, err := memoryformation.Evaluate(memoryformation.CandidateInput{
		SourceUserText: "My phone is 555-0100", Statement: "The user's phone is 555-0100.", Evidence: "My phone is 555-0100",
		Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityUserDirect,
		Sensitivity: memoryformation.SensitivityIdentityOrContact, Mode: memoryformation.ModeAutomaticExtraction,
		Scope: memoryformation.ScopeLongTerm, Category: memoryformation.CategoryIdentity,
		Context: memoryformation.ContextDirectAssertion, Confidence: 0.95, Importance: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err := store.ProposeCandidate(context.Background(), "user-1", usermemory.CandidateProposal{Output: output, IdempotencyKey: "pending-phone"})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{CanonicalUserID: "user-1", Gateway: "websocket", ExternalID: "user-1", Assurance: identity.AssuranceWebSocketSignedToken}
	response, err := agent.Process(Request{RequestID: "req-1", Principal: principal, DisplayName: "Display", SessionKey: "session-1", IsDirect: true, Prompt: "yes remember it"})
	if err != nil || response.Response != "model response" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	messages := primaryRequests(chat.requests)[0].Messages
	if strings.Contains(messages[0].Content, "memory confirmation") || strings.Contains(messages[len(messages)-1].Content, "pending_memory_confirmation") || strings.Contains(messages[len(messages)-1].Content, "555-0100") {
		t.Fatalf("pending confirmation was injected: %+v", messages)
	}
	unconfirmed, err := store.LoadCandidate(context.Background(), "user-1", candidate.ID)
	if err != nil || unconfirmed.PublishedMemoryID != 0 {
		t.Fatalf("conversational phrase changed candidate: %+v err=%v", unconfirmed, err)
	}
}

func TestProcessAddsIMessagePlainTextSystemInstruction(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ok"}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	_, err := processAgent(agent, "req-1", "imessage", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	system := primaryRequests(chat.requests)[0].Messages[0]
	if system.Role != "system" || !strings.Contains(system.Content, "iMessage") || !strings.Contains(system.Content, "does not render Markdown") {
		t.Fatalf("missing imessage system instruction: %+v", system)
	}
}

func TestProcessUsesFreshOperatorManagedSoulAsSystemPrompt(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "first"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "second"}},
	}}
	agent, _, soulPath := newTestAgentWithSoulPath(t, chat, nil, nil)

	if _, err := processAgent(agent, "req-1", "imessage", "session-1", "user-1", "Display", "first question", nil, nil); err != nil {
		t.Fatalf("first process: %v", err)
	}
	firstSystem := primaryRequests(chat.requests)[0].Messages[0]
	if firstSystem.Role != "system" || !strings.HasPrefix(firstSystem.Content, "You are Oswald.\n\n# Gateway Instructions") {
		t.Fatalf("soul and gateway instructions have incorrect authority or order: %+v", firstSystem)
	}

	if err := os.WriteFile(soulPath, []byte("You are Oswald after a manual edit."), 0o600); err != nil {
		t.Fatalf("manually edit soul fixture: %v", err)
	}
	if _, err := processAgent(agent, "req-2", "websocket", "session-2", "user-1", "Display", "second question", nil, nil); err != nil {
		t.Fatalf("second process: %v", err)
	}
	secondSystem := primaryRequests(chat.requests)[1].Messages[0]
	if secondSystem.Role != "system" || secondSystem.Content != "You are Oswald after a manual edit." {
		t.Fatalf("manual soul edit was not reloaded as the system prompt: %+v", secondSystem)
	}
}

func TestProcessDoesNotAddIMessageSystemInstructionForOtherGateways(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ok"}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	_, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	system := primaryRequests(chat.requests)[0].Messages[0]
	if strings.Contains(system.Content, "does not render Markdown") {
		t.Fatalf("unexpected imessage system instruction: %+v", system)
	}
}

func TestProcessDoesNotInjectCurrentTimeIntoSystemPrompt(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ok"}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	_, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	system := primaryRequests(chat.requests)[0].Messages[0]
	if strings.Contains(system.Content, "# Current Date and Time") {
		t.Fatalf("system prompt contains injected current time: %q", system.Content)
	}
}

func TestProcessSendsStrippedSpeakerIntroAsProviderUser(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ok"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	intro := "You are speaking with Example User aka examplehandle."
	if err := store.SyncSpeakerIntro("user-1", intro); err != nil {
		t.Fatalf("sync speaker intro: %v", err)
	}

	_, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	req := primaryRequests(chat.requests)[0]
	if req.User != "Example User aka examplehandle" {
		t.Fatalf("provider user = %q, want stripped speaker name", req.User)
	}
	if !messagesContain(req.Messages, intro) {
		t.Fatalf("system messages no longer contain full speaker intro: %+v", req.Messages)
	}
}

func TestSessionMemoryUserContentReplyOnly(t *testing.T) {
	got := sessionMemoryUserContent("[Replying to Alice: \"old\"]", 0)
	if got != "[User replied to a prior message]" {
		t.Fatalf("unexpected content %q", got)
	}
}

func TestProviderUserValueStripsStaticSpeakerPrefix(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "speaker intro",
			input: "You are speaking with Example User aka examplehandle.",
			want:  "Example User aka examplehandle",
		},
		{
			name:  "display name",
			input: "Example User",
			want:  "Example User",
		},
		{
			name:  "canonical id",
			input: "usr_123",
			want:  "usr_123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerUserValue(tt.input); got != tt.want {
				t.Fatalf("providerUserValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestProcessUsesDynamicMCPDiscoveryTools(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("call-discover", "home.tools", map[string]interface{}{"query": "light"}),
		toolCallResponse("call-tool", "home.turn_on", map[string]interface{}{"entity": "light.office"}),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	agent, _ := newTestAgent(t, chat, nil, nil)
	agent.mcpProvider = &fakeMCPProvider{}

	resp, err := processAgent(agent, "req-mcp", "websocket", "session-mcp", "user-1", "User", "turn on office light", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "done" {
		t.Fatalf("response = %q", resp.Response)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) < 2 {
		t.Fatalf("expected multiple requests, got %d", len(requests))
	}
	if !requestHasTool(requests[0], "home.tools") {
		t.Fatalf("first request did not include home.tools: %+v", toolNames(requests[0]))
	}
	if requestHasTool(requests[0], "home.turn_on") {
		t.Fatalf("first request exposed actual MCP tool before discovery")
	}
	if !requestHasTool(requests[1], "home.turn_on") {
		t.Fatalf("second request did not expose actual MCP tool: %+v", toolNames(requests[1]))
	}
}

func TestProcessPreExposesMCPToolsFromRecentSessionTurns(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	agent.mcpProvider = &fakeMCPProvider{}
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-mcp", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session-mcp", "user-1", profile.Generation, "prior question", "prior answer", []string{"home.turn_on", "home.tools", "web.search"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, err := processAgent(agent, "req-mcp", "websocket", "session-mcp", "user-1", "User", "again", nil, nil); err != nil {
		t.Fatalf("process: %v", err)
	}
	request := primaryRequests(chat.requests)[0]
	if !requestHasTool(request, "home.turn_on") {
		t.Fatalf("first request did not pre-expose recent MCP tool: %+v", toolNames(request))
	}
	foundAnnotation := false
	for _, message := range request.Messages {
		if message.Role == "assistant" && strings.Contains(message.Content, "Tools used: home.turn_on, home.tools, web.search") {
			foundAnnotation = true
		}
	}
	if !foundAnnotation {
		t.Fatalf("assistant history missing compact tool annotation: %+v", request.Messages)
	}
}

func TestProcessFreezesTenantProfileUntilNewSession(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "one"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "two"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "three"}},
	}}
	agent, store := newTestAgent(t, chat, nil, nil)
	if _, err := store.SaveMemory(context.Background(), "user-1", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Category: "identity", Statement: "The user is Ada.", Confidence: 1, Importance: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-1", "websocket", "session-1", "user-1", "Ada", "first", nil, nil); err != nil {
		t.Fatal(err)
	}
	firstProfile := tenantProfileMessage(primaryRequests(chat.requests)[0].Messages)
	if _, err := store.SaveMemory(context.Background(), "user-1", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Category: "communication_preferences", Statement: "The user prefers concise replies.", Confidence: 1, Importance: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-2", "websocket", "session-1", "user-1", "Ada", "second", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-3", "websocket", "session-2", "user-1", "Ada", "third", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	frozenProfile := tenantProfileMessage(requests[1].Messages)
	latestProfile := tenantProfileMessage(requests[2].Messages)
	if firstProfile == "" || frozenProfile != firstProfile {
		t.Fatalf("profile changed in active session: first=%q frozen=%q", firstProfile, frozenProfile)
	}
	if !strings.Contains(latestProfile, "concise replies") || latestProfile == firstProfile {
		t.Fatalf("new session did not receive latest profile: %q", latestProfile)
	}
	if len(requests[0].Messages) != 3 || requests[0].Messages[1].Role != "user" || !strings.Contains(requests[1].Messages[1].Content, "authority=\"lower\"") {
		t.Fatalf("tenant profile is not lower-authority user context: %+v", requests[0].Messages)
	}
}

func TestProcessNeverIncludesAnotherUsersTenantProfile(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "one"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "two"}},
	}}
	agent, store := newTestAgent(t, chat, nil, nil)
	for _, tc := range []struct{ user, statement string }{{"user-1", "The user is Alice."}, {"user-2", "The user is Bob."}} {
		if _, err := store.SaveMemory(context.Background(), tc.user, usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Category: "identity", Statement: tc.statement, Confidence: 1, Importance: 5}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := processAgent(agent, "req-a", "websocket", "shared-session", "user-1", "Alice", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-b", "websocket", "shared-session", "user-2", "Bob", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if messagesContain(requests[0].Messages, "Bob") || messagesContain(requests[1].Messages, "Alice") {
		t.Fatalf("cross-user profile leak: a=%+v b=%+v", requests[0].Messages, requests[1].Messages)
	}
}

func TestProcessDoesNotPreExposeMCPToolOutsideRecentFourTurns(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	agent.mcpProvider = &fakeMCPProvider{}
	if err := store.AppendSessionTurn(context.Background(), "session-mcp", "user-1", "old question", "old answer", []string{"home.turn_on"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := store.AppendSessionTurn(context.Background(), "session-mcp", "user-1", "recent question", "recent answer", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := processAgent(agent, "req-mcp", "websocket", "session-mcp", "user-1", "User", "again", nil, nil); err != nil {
		t.Fatalf("process: %v", err)
	}
	request := primaryRequests(chat.requests)[0]
	if requestHasTool(request, "home.turn_on") {
		t.Fatalf("first request exposed tool from fifth-oldest turn: %+v", toolNames(request))
	}
	if strings.Contains(request.Messages[0].Content, "old question") {
		t.Fatalf("system prompt included fifth-oldest turn:\n%s", request.Messages[0].Content)
	}
}

func TestProcessLoadsGlobalMemoryFromIndependentProvider(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	defer store.Close()
	agent.globalMemory = fakeGlobalMemoryPromptProvider{prompt: `<global_memory authority="lower">global memory fact</global_memory>`}
	if _, err := processAgent(agent, "request", "websocket", "session", "user-1", "User", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 1 || !messagesContain(requests[0].Messages, "global memory fact") {
		t.Fatalf("global memory prompt missing: %+v", requests)
	}
	for _, message := range requests[0].Messages {
		if strings.Contains(strings.ToLower(message.Content), "deployment_memory") {
			t.Fatalf("stale global memory vocabulary in prompt: %q", message.Content)
		}
	}
}

func TestAgentKeepsDefaultVisibleGlobalMemoryAfterGlobalMCPResult(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("discover", "home.tools", nil),
		toolCallResponse("global-call", "home.turn_on", map[string]interface{}{"entity": "light"}),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := reg.RegisterSpec(registry.Spec{Name: toolnames.GlobalMemorySave, Source: registry.ToolSourceBuiltin}); err != nil {
		t.Fatal(err)
	}
	agent, store := newTestAgent(t, chat, nil, reg)
	defer store.Close()
	agent.mcpProvider = &fakeMCPProvider{scope: mcp.ScopeGlobal}
	if _, err := processAgent(agent, "request", "websocket", "session", "user-1", "User", "check the deployment", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 3 {
		t.Fatalf("request count=%d", len(requests))
	}
	for i, request := range requests {
		if !requestHasTool(request, toolnames.GlobalMemorySave) {
			t.Fatalf("global memory proposal missing from request %d: %+v", i, toolNames(request))
		}
	}
}

func toolCallResponse(id, name string, args map[string]interface{}) *llm.ChatResponse {
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Function: llm.ToolFunction{Name: name, Arguments: args}}}}}
}

type fakeChatter struct {
	responses []*llm.ChatResponse
	outcomes  []fakeChatOutcome
	requests  []llm.ChatRequest
}

type fakeChatOutcome struct {
	response *llm.ChatResponse
	err      error
}

type fakeGlobalMemoryPromptProvider struct {
	prompt string
	err    error
}

func (f fakeGlobalMemoryPromptProvider) GlobalMemoryPrompt(context.Context) (string, error) {
	return f.prompt, f.err
}

func (f *fakeChatter) Chat(_ context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.requests = append(f.requests, req)
	if len(f.outcomes) > 0 {
		outcome := f.outcomes[0]
		f.outcomes = f.outcomes[1:]
		if outcome.err != nil {
			return nil, outcome.err
		}
		if outcome.response == nil {
			return nil, errors.New("empty fake outcome")
		}
		if cb != nil {
			if outcome.response.Message.Thinking != "" {
				cb(llm.ChatMessage{Thinking: outcome.response.Message.Thinking})
			}
			if outcome.response.Message.Content != "" {
				cb(llm.ChatMessage{Content: outcome.response.Message.Content})
			}
		}
		return outcome.response, nil
	}
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

type fakeMCPProvider struct{ scope string }

func (p *fakeMCPProvider) DiscoveryTools(context.Context, identity.Principal) []llm.Tool {
	return []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "home.tools", Description: "Search Home Assistant tools", Parameters: llm.ToolParameters{Type: "object"}}}}
}

func (p *fakeMCPProvider) ResolveTools(_ context.Context, _ identity.Principal, names []string) []string {
	for _, name := range names {
		if name == "home.turn_on" {
			return []string{name}
		}
	}
	return nil
}

func (p *fakeMCPProvider) LLMTools(_ context.Context, _ identity.Principal, exposed map[string]bool) []llm.Tool {
	if !exposed["home.turn_on"] {
		return nil
	}
	return []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "home.turn_on", Description: "Turn on a light", Parameters: llm.ToolParameters{Type: "object"}}}}
}

func (p *fakeMCPProvider) Execute(ctx context.Context, _ identity.Principal, name string, _ map[string]interface{}, exposed map[string]bool) (mcp.ExecutionResult, bool, error) {
	if name == "home.tools" {
		if exposer := requestctx.ToolExposerFromContext(ctx); exposer != nil {
			exposer.ExposeTools([]string{"home.turn_on"})
		}
		return mcp.ExecutionResult{Content: "Available MCP tools from home:\n1. home.turn_on", IsDiscovery: true}, true, nil
	}
	if name == "home.turn_on" && exposed[name] {
		scope := p.scope
		if scope == "" {
			scope = mcp.ScopeUser
		}
		return mcp.ExecutionResult{Content: "light turned on", Scope: scope, ServerID: "server-1", ServerName: "home", ToolName: name, RemoteToolName: "turn_on"}, true, nil
	}
	return mcp.ExecutionResult{}, false, nil
}

func processAgent(agent *Agent, requestID, gateway, sessionKey, userID, displayName, prompt string, images []llm.InputImage, streamFunc func(StreamChunk)) (*AgentResponse, error) {
	assurance := identity.AssuranceSelfAsserted
	switch gateway {
	case "discord":
		assurance = identity.AssuranceDiscordGateway
	case "imessage":
		assurance = identity.AssuranceBlueBubblesWebhook
	}
	response, err := agent.Process(Request{
		RequestID: requestID,
		Principal: identity.Principal{
			CanonicalUserID: userID,
			Gateway:         gateway,
			ExternalID:      userID,
			Assurance:       assurance,
		},
		DisplayName: displayName,
		SessionKey:  sessionKey,
		Prompt:      prompt,
		Images:      images,
		StreamFunc:  streamFunc,
	})
	if err == nil && response != nil && response.SourceTurnID > 0 && agent.userMemory != nil {
		_ = agent.userMemory.MarkSessionTurnDelivered(context.Background(), userID, response.SourceTurnID)
	}
	return response, err
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
	agent, store, _ := newTestAgentWithSoulPath(t, chat, embedder, reg)
	return agent, store
}

func newTestAgentWithSoulPath(t *testing.T, chat llm.Chatter, embedder llm.Embedder, reg *registry.Registry) (*Agent, *usermemory.Store, string) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	if reg == nil {
		reg = registry.New(log)
	}
	dir := t.TempDir()
	soulPath := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(soulPath, []byte("You are Oswald."), 0o600); err != nil {
		t.Fatalf("write soul fixture: %v", err)
	}
	soulStore := soul.NewStore(soulPath)
	dbPath := filepath.Join(dir, "oswald.db")
	db, err := database.Open(dbPath, log)
	if err != nil {
		t.Fatalf("open account database: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL().Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, ?, ?), (?, ?, ?)`, "user-1", now, now, "user-2", now, now); err != nil {
		t.Fatalf("seed account user: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO linked_accounts (gateway, identifier, canonical_user_id, display_name, linked_at, verified) VALUES ('websocket', 'user-1', 'user-1', 'User 1', ?, 1), ('websocket', 'user-2', 'user-2', 'User 2', ?, 1)`, now, now); err != nil {
		t.Fatalf("seed linked accounts: %v", err)
	}
	db.Close() // nolint:errcheck
	embeddingModel := ""
	if embedder != nil {
		embeddingModel = "embed-model"
	}
	userStore, err := usermemory.NewSQLiteStore(dbPath, embedder, embeddingModel, log)
	if err != nil {
		t.Fatalf("user store: %v", err)
	}
	agent := NewAgent(chat, reg, "test-model", soulStore, userStore, nil, promptbudget.ContextBudget{PromptLimit: 100000}, 3, time.Minute, log)
	return agent, userStore, soulPath
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

func messagesContain(messages []llm.ChatMessage, needle string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, needle) {
			return true
		}
	}
	return false
}

func tenantProfileMessage(messages []llm.ChatMessage) string {
	for _, message := range messages {
		start := strings.Index(message.Content, "<tenant_profile")
		end := strings.Index(message.Content, "</tenant_profile>")
		if start >= 0 && end >= start {
			return message.Content[start : end+len("</tenant_profile>")]
		}
	}
	return ""
}

func requestHasTool(req llm.ChatRequest, name string) bool {
	for _, tool := range req.Tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

func toolNames(req llm.ChatRequest) []string {
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

func testInputImage(t *testing.T, width, height int) llm.InputImage {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 127, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	input, err := media.BuildInputImageFromBytes("image/jpeg", buf.Bytes(), "test.jpg")
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func inputImageDimensions(t *testing.T, input llm.InputImage) image.Point {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(input.Data)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return decoded.Bounds().Size()
}
