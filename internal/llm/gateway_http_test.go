package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestGatewayClientChatPostsRequestAndParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-key" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-bf-vk"); got != "virtual-key" {
			t.Fatalf("x-bf-vk = %q", got)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["model"] != "test-model" || body["stream"] != false {
			t.Fatalf("unexpected request body: %+v", body)
		}
		_, _ = w.Write([]byte(`{
			"model":"served-model",
			"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
		}`))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL+"/", "api-key", "virtual-key", time.Second, config.NewLogger(config.LevelError))
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "test-model", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Model != "served-model" || resp.Message.Content != "hello" || resp.PromptTokens != 3 || resp.TotalTokens != 5 || resp.DoneReason != "stop" {
		t.Fatalf("unexpected Chat response: %+v", resp)
	}
}

func TestGatewayClientChatHTTPAndDecodeErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "") {
		}
		switch r.Header.Get("x-test-case") {
		case "bad-json":
			_, _ = w.Write([]byte(`not-json`))
		default:
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "", "", time.Second, config.NewLogger(config.LevelError))
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "m"}, nil); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("Chat HTTP error = %v, want HTTP 502", err)
	}

	client.HTTPClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.Header.Set("x-test-case", "bad-json")
		return http.DefaultTransport.RoundTrip(req)
	})
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "m"}, nil); err == nil || !strings.Contains(err.Error(), "decode chat response") {
		t.Fatalf("Chat decode error = %v, want decode error", err)
	}
}

func TestGatewayClientEmbedParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path = %q, want /v1/embeddings", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"model":"embed-model","data":[{"embedding":[1.5,2.5]}]}`))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "", "", time.Second, config.NewLogger(config.LevelError))
	resp, err := client.Embed(context.Background(), EmbedRequest{Model: "embed-model", Input: "text"})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if resp.Model != "embed-model" || len(resp.Embeddings) != 1 || len(resp.Embeddings[0]) != 2 || resp.Embeddings[0][1] != 2.5 {
		t.Fatalf("unexpected Embed response: %+v", resp)
	}
}

func TestGatewayClientChatStreamAccumulatesContentThinkingAndTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"stream-model\",\"choices\":[{\"delta\":{\"reasoning\":\"think\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"tool.name\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":8,\"total_tokens\":15}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "", "", time.Second, config.NewLogger(config.LevelError))
	var chunks []ChatMessage
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "fallback-model", Stream: true}, func(chunk ChatMessage) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("stream Chat returned error: %v", err)
	}
	if resp.Model != "stream-model" || resp.Message.Thinking != "think" || resp.Message.Content != "hello" || resp.DoneReason != "stop" {
		t.Fatalf("unexpected stream response: %+v", resp)
	}
	if resp.PromptTokens != 7 || resp.CompletionTokens != 8 || resp.TotalTokens != 15 {
		t.Fatalf("unexpected stream usage: %+v", resp)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].ID != "call_1" || resp.Message.ToolCalls[0].Function.Arguments["x"] != float64(1) {
		t.Fatalf("unexpected stream tool call: %+v", resp.Message.ToolCalls)
	}
	if len(chunks) != 2 || chunks[0].Thinking != "think" || chunks[1].Content != "hello" {
		t.Fatalf("unexpected stream chunks: %+v", chunks)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
