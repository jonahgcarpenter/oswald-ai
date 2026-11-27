package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

func TestSyncChatGenericProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"model":"generic-model",
			"choices":[{"finish_reason":"function_call","message":{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":{"q":"hi"}}}]}}],
			"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"completion_tokens_details":{"reasoning_tokens":1}}
		}`))
	}))
	defer server.Close()
	client := newTestClient(server.URL, "k", "", config.NewLogger(config.LevelError))
	resp, err := client.Chat(context.Background(), llm.ChatRequest{Model: "m", Messages: []llm.ChatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.DoneReason != "tool_calls" {
		t.Fatalf("done_reason = %q, want tool_calls", resp.DoneReason)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("toolcalls = %+v", resp.Message.ToolCalls)
	}
	if v, ok := resp.Message.ToolCalls[0].Function.Arguments["q"]; !ok || v != "hi" {
		t.Fatalf("args = %+v", resp.Message.ToolCalls[0].Function.Arguments)
	}
}

func TestSyncChatMaxCompletionTokensAlias(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls++
		if calls == 1 {
			if _, ok := body["max_tokens"]; !ok {
				t.Fatalf("first request missing max_tokens: %v", body)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"max_completion_tokens is required, not max_tokens"}}`))
			return
		}
		if _, ok := body["max_completion_tokens"]; !ok {
			t.Fatalf("retry missing max_completion_tokens: %v", body)
		}
		if _, ok := body["max_tokens"]; ok {
			t.Fatalf("retry still has max_tokens")
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	max := 10
	client := newTestClient(server.URL, "", "", config.NewLogger(config.LevelError))
	resp, err := client.Chat(context.Background(), llm.ChatRequest{Model: "m", MaxTokens: &max}, nil)
	if err != nil || resp.Message.Content != "ok" || calls != 2 {
		t.Fatalf("resp=%+v err=%v calls=%d", resp, err, calls)
	}
}

func TestSyncChatParallelToolCallsAlias(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"parallel_tool_calls not supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	parallel := true
	client := newTestClient(server.URL, "", "", config.NewLogger(config.LevelError))
	resp, err := client.Chat(context.Background(), llm.ChatRequest{Model: "m", ParallelToolCalls: &parallel}, nil)
	if err != nil || resp.Message.Content != "ok" || calls != 2 {
		t.Fatalf("resp=%+v err=%v calls=%d", resp, err, calls)
	}
}

func TestSyncChatErrorShapes(t *testing.T) {
	for _, body := range []string{
		`{"error":"plain boom"}`,
		`{"detail":"detail boom"}`,
		`{"message":"message boom"}`,
		`{"error":{"message":"object boom"}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(body))
		}))
		_, err := newTestClient(server.URL, "", "", config.NewLogger(config.LevelError)).Chat(context.Background(), llm.ChatRequest{Model: "m"}, nil)
		server.Close()
		var httpErr *llm.ChatHTTPError
		if err == nil {
			t.Fatalf("body %s: expected error", body)
		} else if !strings.Contains(err.Error(), "HTTP 400") {
			t.Fatalf("body %s: err = %v", body, err)
		}
		_ = httpErr
		if !llm.IsPermanentChatProviderError(err) {
			t.Fatalf("body %s: not permanent: %v", body, err)
		}
	}
}

func TestSyncChatNotFoundFallsBackToAsync(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"j1","status":"pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"j1","status":"completed","status_code":200,"result":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"fallback-ok"}}]}}`))
	}))
	defer server.Close()
	client := newTestClient(server.URL, "", "", config.NewLogger(config.LevelError))
	resp, err := client.Chat(context.Background(), llm.ChatRequest{Model: "m"}, nil)
	if err != nil || resp.Message.Content != "fallback-ok" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestSyncEmbedFallbackToAsync(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"e1","status":"pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"e1","status":"completed","status_code":200,"result":{"model":"e","data":[{"embedding":[0.1]}]}}`))
	}))
	defer server.Close()
	client := newTestClient(server.URL, "", "", config.NewLogger(config.LevelError))
	resp, err := client.Embed(context.Background(), llm.EmbedRequest{Model: "e", Input: "hi"})
	if err != nil || len(resp.Embeddings) != 1 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestStreamSendsIncludeUsageAndHandlesMissingIndex(t *testing.T) {
	var sawOptions bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if raw, ok := body["stream_options"]; ok {
			var opts map[string]bool
			if err := json.Unmarshal(raw, &opts); err == nil && opts["include_usage"] {
				sawOptions = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"a\",\"function\":{\"name\":\"one\",\"arguments\":\"{}\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"b\",\"function\":{\"name\":\"two\",\"arguments\":\"{}\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := newTestClient(server.URL, "", "", config.NewLogger(config.LevelError))
	resp, err := client.Chat(context.Background(), llm.ChatRequest{Model: "m", Stream: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sawOptions {
		t.Fatal("stream_options.include_usage not sent")
	}
	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("toolcalls = %+v", resp.Message.ToolCalls)
	}
}
