package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const reflectedSoulCanary = "SOUL_CANARY_DO_NOT_LOG_OR_RETURN"

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

func TestGatewayClientChatSerializesForcedRecursiveToolSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools      []Tool      `json:"tools"`
			ToolChoice *ToolChoice `json:"tool_choice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Tools) != 1 || body.ToolChoice == nil || body.ToolChoice.Function.Name != "user_memory_save" {
			t.Fatalf("unexpected forced tool request: %+v", body)
		}
		memories := body.Tools[0].Function.Parameters.Properties["memories"]
		if memories.Items == nil || memories.MaxItems == nil || *memories.MaxItems != 5 || memories.AdditionalProperties != nil {
			t.Fatalf("unexpected recursive schema: %+v", memories)
		}
		if memories.Items.AdditionalProperties == nil || *memories.Items.AdditionalProperties {
			t.Fatalf("item additionalProperties was not false: %+v", memories.Items)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()

	maximum := 5
	additional := false
	tool := Tool{Type: "function", Function: ToolDefinition{Name: "user_memory_save", Parameters: ToolParameters{
		Type: "object", Properties: map[string]ToolParameterProperty{"memories": {
			Type: "array", MaxItems: &maximum, Items: &ToolParameterProperty{Type: "object", AdditionalProperties: &additional},
		}},
	}}}
	client := NewGatewayClient(server.URL, "", "", time.Second, config.NewLogger(config.LevelError))
	_, err := client.Chat(context.Background(), ChatRequest{Model: "model", Tools: []Tool{tool}, ToolChoice: &ToolChoice{Type: "function", Function: ToolChoiceFunction{Name: "user_memory_save"}}}, nil)
	if err != nil {
		t.Fatal(err)
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

func TestGatewayClientChatReturnsTypedHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"XML syntax error on line 7: unexpected EOF %s"}}`, reflectedSoulCanary)))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "", "", time.Second, config.NewLogger(config.LevelError))
	_, err := client.Chat(context.Background(), ChatRequest{Model: "m"}, nil)
	var httpErr *ChatHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("Chat error = %T %v, want *ChatHTTPError", err, err)
	}
	if httpErr.StatusCode != http.StatusInternalServerError || !strings.Contains(httpErr.Body, "XML syntax error") || !strings.Contains(httpErr.Body, reflectedSoulCanary) {
		t.Fatalf("unexpected typed error: %+v", httpErr)
	}
	for label, text := range map[string]string{
		"error":      err.Error(),
		"safe error": config.SafeErrorText(err),
		"wrapped":    fmt.Errorf("model failed: %w", err).Error(),
	} {
		if strings.Contains(text, reflectedSoulCanary) {
			t.Fatalf("%s exposed reflected provider content: %q", label, text)
		}
	}
	if !IsTemporaryOllamaToolParserError(err) {
		t.Fatalf("expected temporary parser error classification: %v", err)
	}
}

func TestGatewayClientProviderErrorsDoNotExposeResponseText(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		call    func(*GatewayClient) error
	}{
		{
			name: "chat response error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"%s"}}`, reflectedSoulCanary)))
			},
			call: func(client *GatewayClient) error {
				_, err := client.Chat(context.Background(), ChatRequest{Model: "m"}, nil)
				return err
			},
		},
		{
			name: "chat stream response error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(fmt.Sprintf("data: {\"error\":{\"message\":%q}}\n\n", reflectedSoulCanary)))
			},
			call: func(client *GatewayClient) error {
				_, err := client.Chat(context.Background(), ChatRequest{Model: "m", Stream: true}, func(ChatMessage) {})
				return err
			},
		},
		{
			name: "embed HTTP error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(reflectedSoulCanary))
			},
			call: func(client *GatewayClient) error {
				_, err := client.Embed(context.Background(), EmbedRequest{Model: "m", Input: "input"})
				return err
			},
		},
		{
			name: "embed response error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"%s"}}`, reflectedSoulCanary)))
			},
			call: func(client *GatewayClient) error {
				_, err := client.Embed(context.Background(), EmbedRequest{Model: "m", Input: "input"})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			var err error
			logs := captureLogs(t, func(log *config.Logger) {
				client := NewGatewayClient(server.URL, "", "", time.Second, log)
				err = tt.call(client)
			})
			if err == nil {
				t.Fatal("expected provider error")
			}
			if strings.Contains(err.Error(), reflectedSoulCanary) || strings.Contains(config.SafeErrorText(err), reflectedSoulCanary) {
				t.Fatalf("provider error exposed reflected content: %q", err)
			}
			if strings.Contains(logs, reflectedSoulCanary) {
				t.Fatalf("provider logs exposed reflected content: %s", logs)
			}
		})
	}
}

func captureLogs(t *testing.T, run func(*config.Logger)) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create log pipe: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	log := config.NewLogger(config.LevelError)
	os.Stderr = oldStderr

	run(log)
	if err := w.Close(); err != nil {
		t.Fatalf("close log writer: %v", err)
	}
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close log reader: %v", err)
	}
	return string(output)
}

func TestTemporaryOllamaToolParserErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "element type", err: &ChatHTTPError{StatusCode: 500, Body: `expected element type <function> but have <parameter>`}, want: true},
		{name: "xml syntax", err: &ChatHTTPError{StatusCode: 500, Body: `XML syntax error on line 7: unexpected EOF`}, want: true},
		{name: "unrelated 500", err: &ChatHTTPError{StatusCode: 500, Body: `out of memory`}, want: false},
		{name: "wrong status", err: &ChatHTTPError{StatusCode: 400, Body: `XML syntax error: unexpected EOF`}, want: false},
		{name: "ordinary error", err: errors.New("XML syntax error: unexpected EOF"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTemporaryOllamaToolParserError(tt.err); got != tt.want {
				t.Fatalf("IsTemporaryOllamaToolParserError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOllamaModelRunnerStoppedErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "runner stopped", err: &ChatHTTPError{StatusCode: 500, Body: `model runner has unexpectedly stopped, this may be due to resource limitations`}, want: true},
		{name: "unrelated 500", err: &ChatHTTPError{StatusCode: 500, Body: `out of memory`}, want: false},
		{name: "wrong status", err: &ChatHTTPError{StatusCode: 502, Body: `model runner has unexpectedly stopped`}, want: false},
		{name: "ordinary error", err: errors.New("model runner has unexpectedly stopped"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsOllamaModelRunnerStoppedError(tt.err); got != tt.want {
				t.Fatalf("IsOllamaModelRunnerStoppedError() = %v, want %v", got, tt.want)
			}
		})
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
