package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/profiles"
)

const testAPIKey = "test-secret-key-1234567890abcdef"

func testGateway(t *testing.T) *Gateway {
	t.Helper()
	log := config.NewLogger(config.LevelDebug)
	directory, err := profiles.NewDirectory(&config.Config{ProfileRoot: t.TempDir(), ProfileName: "default", OpenAIListenHost: "127.0.0.1", OpenAIListenPort: "12345", OpenAIAPIKey: testAPIKey, Profiles: map[string]*config.Config{"api": {ProfileName: "api"}}}, log)
	if err != nil {
		t.Fatal(err)
	}
	g, err := New("127.0.0.1", "12345", directory, gatewayruntime.Dependencies{}, "route", log)
	if err != nil {
		t.Fatal(err)
	}
	g.resolveKey = func(raw string) (identity.Principal, error) {
		if raw != testAPIKey {
			return identity.Principal{}, profiles.ErrUnmappedIdentity
		}
		return identity.Principal{CanonicalUserID: "default", Gateway: "openai", ExternalID: "api:default", Assurance: identity.AssuranceAPIKey}, nil
	}
	return g
}

func request(g *Gateway, method, path, body string, auth ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if len(auth) == 0 {
		r.Header.Set("Authorization", "Bearer "+testAPIKey)
	} else {
		for _, value := range auth {
			if value != "" {
				r.Header.Add("Authorization", value)
			}
		}
	}
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	return w
}

func TestValidation(t *testing.T) {
	g := testGateway(t)
	for _, tc := range []struct {
		name, body string
		auth       []string
		code       int
	}{
		{"malformed", `{"model":`, []string{"Bearer " + testAPIKey}, 400},
		{"nested unknown", `{"model":"oswald","messages":[{"role":"user","content":"hi","name":"injected"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"reasoning on user", `{"model":"oswald","messages":[{"role":"user","content":"hi","reasoning_content":"override"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"invalid reasoning type", `{"model":"oswald","messages":[{"role":"assistant","content":"prior","reasoning_content":{"text":"private"}},{"role":"user","content":"hi"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"trailing json", `{"model":"oswald","messages":[{"role":"user","content":"hi"}]} {}`, []string{"Bearer " + testAPIKey}, 400},
		{"wrong model", `{"model":"route","messages":[{"role":"user","content":"hi"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"required without tools", `{"model":"oswald","tool_choice":"required","messages":[{"role":"user","content":"hi"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"named missing tool", `{"model":"oswald","tool_choice":{"type":"function","function":{"name":"read"}},"messages":[{"role":"user","content":"hi"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"invalid client tool", `{"model":"oswald","tools":["read"],"messages":[{"role":"user","content":"hi"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"invalid stream options", `{"model":"oswald","stream_options":{"include_usage":"yes"},"messages":[{"role":"user","content":"hi"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"invalid output tokens", `{"model":"oswald","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"unknown top-level field", `{"model":"oswald","foo":1,"messages":[{"role":"user","content":"hi"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"tool message without id", `{"model":"oswald","messages":[{"role":"tool","content":"result"},{"role":"user","content":"hi"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"last assistant", `{"model":"oswald","messages":[{"role":"assistant","content":"hi"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"command", `{"model":"oswald","messages":[{"role":"user","content":"/new"}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"image", `{"model":"oswald","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"mixed text and image", `{"model":"oswald","messages":[{"role":"user","content":[{"type":"text","text":"Hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"unexpected part field", `{"model":"oswald","messages":[{"role":"user","content":[{"type":"text","text":"Hi","image_url":"hidden"}]}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"missing part text", `{"model":"oswald","messages":[{"role":"user","content":[{"type":"text"}]}]}`, []string{"Bearer " + testAPIKey}, 400},
		{"empty parts", `{"model":"oswald","messages":[{"role":"user","content":[]} ]}`, []string{"Bearer " + testAPIKey}, 400},
		{"oversized", `{"model":"oswald","messages":[{"role":"user","content":"` + strings.Repeat("a", 256<<10) + `"}]}`, []string{"Bearer " + testAPIKey}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(g, "POST", "/v1/chat/completions", tc.body, tc.auth...)
			if w.Code != tc.code {
				t.Fatalf("status %d, want %d", w.Code, tc.code)
			}
			var result map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["error"] == nil {
				t.Fatalf("missing JSON error: %v", err)
			}
		})
	}
}

func TestOpenCodeChatRequest(t *testing.T) {
	g := testGateway(t)
	called := 0
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		called++
		if !req.Stateless || req.Text != "Test Oswald" || len(req.ClientHistory) != 3 || req.ClientHistory[0].Role != "system" || req.ClientHistory[0].Content != "client policy" || req.ClientHistory[1].Role != "developer" || req.ClientHistory[2].Role != "assistant" {
			t.Errorf("incorrect client request normalization: %+v", req)
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "Oswald answer"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	// These fields and the SSE response shape were observed with OpenCode's
	// @ai-sdk/openai-compatible provider against a disposable local endpoint.
	body := `{"model":"oswald","stream":true,"stream_options":{"include_usage":true},"max_tokens":8192,"tool_choice":"auto","tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}],"messages":[{"role":"system","content":"client policy"},{"role":"developer","content":"client detail"},{"role":"assistant","content":"prior reply"},{"role":"user","content":"Test Oswald"}]}`
	w := request(g, "POST", "/v1/chat/completions", body, "Bearer "+testAPIKey)
	if w.Code != 200 || called != 1 || !strings.Contains(w.Body.String(), "Oswald answer") || !strings.Contains(w.Body.String(), "data: [DONE]") || strings.Contains(w.Body.String(), `"tool_calls"`) || strings.Contains(w.Body.String(), `"usage"`) {
		t.Fatalf("OpenCode response code=%d calls=%d body=%s", w.Code, called, w.Body.String())
	}
}

func TestOpenCodeFollowUpIgnoresReasoning(t *testing.T) {
	g := testGateway(t)
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if req.Text != "another question" || len(req.ClientHistory) != 2 || req.ClientHistory[1].Role != "assistant" || req.ClientHistory[1].Content != "previous answer" || req.ClientHistory[1].Thinking != "" {
			t.Errorf("unexpected follow-up history: %+v", req.ClientHistory)
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "new answer"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	// OpenCode sends reasoning_content back with its prior assistant message
	// after receiving Oswald's streamed reasoning tokens.
	body := `{"model":"oswald","stream":true,"messages":[{"role":"system","content":"client instructions"},{"role":"assistant","content":"previous answer","reasoning_content":"First thought. Second thought."},{"role":"user","content":"another question"}]}`
	w := request(g, "POST", "/v1/chat/completions", body, "Bearer "+testAPIKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "new answer") {
		t.Fatalf("follow-up response: %d %s", w.Code, w.Body.String())
	}
}

func TestTextPartContent(t *testing.T) {
	g := testGateway(t)
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if req.Text != "Hi there" || len(req.ClientHistory) != 2 || req.ClientHistory[0].Content != "client policy" || req.ClientHistory[1].Content != "previous answer" {
			t.Errorf("unexpected text parts: %+v", req)
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "Hello"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	body := `{"model":"oswald","stream":true,"messages":[{"role":"system","content":[{"type":"text","text":"client policy"}]},{"role":"assistant","content":[{"type":"text","text":"previous answer"}]},{"role":"user","content":[{"type":"text","text":"Hi"},{"type":"text","text":" there"}]}]}`
	w := request(g, "POST", "/v1/chat/completions", body, "Bearer "+testAPIKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Hello") || !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("text parts response: %d %s", w.Code, w.Body.String())
	}
}

func TestCompletionAndStream(t *testing.T) {
	g := testGateway(t)
	called := 0
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		called++
		if !req.Stateless || !req.IsDirect || req.Text != "now" || len(req.ClientHistory) != 2 || req.ClientHistory[0].Content != "before" || req.ClientHistory[1].Role != "assistant" {
			t.Errorf("unexpected runtime request: %+v", req)
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "answer", Thinking: "private"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	for _, stream := range []bool{false, true} {
		body := `{"model":"oswald","messages":[{"role":"user","content":"before"},{"role":"assistant","content":"previous"},{"role":"user","content":"now"}]`
		if stream {
			body += `,"stream":true}`
		} else {
			body += `}`
		}
		w := request(g, "POST", "/v1/chat/completions", body, "Bearer "+testAPIKey)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "answer") {
			t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
		}
		if !stream && !strings.Contains(w.Body.String(), `"reasoning_content":"private"`) {
			t.Fatalf("buffered response must carry thinking: %d %s", w.Code, w.Body.String())
		}
		if stream && strings.Contains(w.Body.String(), "private") {
			t.Fatalf("stream must not invent reasoning without progress: %d %s", w.Code, w.Body.String())
		}
		if stream {
			if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatal("missing SSE content type")
			}
			frames := strings.Split(strings.TrimSuffix(w.Body.String(), "\n\n"), "\n\n")
			if len(frames) != 4 || frames[3] != "data: [DONE]" {
				t.Fatalf("invalid SSE frames: %q", frames)
			}
			for _, frame := range frames[:3] {
				var chunk struct {
					Object  string            `json:"object"`
					Choices []json.RawMessage `json:"choices"`
				}
				if !strings.HasPrefix(frame, "data: ") || json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &chunk) != nil || chunk.Object != "chat.completion.chunk" || len(chunk.Choices) != 1 {
					t.Fatalf("invalid SSE chunk: %q", frame)
				}
			}
		} else if strings.Contains(w.Body.String(), `"usage"`) {
			t.Fatal("invented usage in stateless response")
		}
	}
	if called != 2 {
		t.Fatalf("called %d times", called)
	}
}

func TestStreamProgressPrecedesFinalAnswer(t *testing.T) {
	g := testGateway(t)
	var w *httptest.ResponseRecorder
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if req.StreamFunc == nil {
			t.Fatal("stream callback not passed to runtime")
		}
		req.StreamFunc(agent.StreamChunk{Type: agent.ChunkThinking, Text: "First thought. "})
		req.StreamFunc(agent.StreamChunk{Type: agent.ChunkContent, Text: "abandoned draft"})
		req.StreamFunc(agent.StreamChunk{Type: agent.ChunkToolCall, Tool: &agent.ToolStreamPayload{Name: "private.tool", ResultText: "private result"}})
		req.StreamFunc(agent.StreamChunk{Type: agent.ChunkThinking, Text: "Second thought."})
		if !w.Flushed || !strings.Contains(w.Body.String(), `"reasoning_content":"First thought. "`) || !strings.Contains(w.Body.String(), `"reasoning_content":"Second thought."`) || strings.Contains(w.Body.String(), "Final answer") {
			t.Fatal("progress must flush before the final response")
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "Final answer", Thinking: "First thought. Second thought."}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"oswald","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+testAPIKey)
	w = httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	frames := strings.Split(strings.TrimSuffix(w.Body.String(), "\n\n"), "\n\n")
	if w.Code != 200 || !w.Flushed || len(frames) != 6 || !strings.Contains(frames[1], `"reasoning_content":"First thought. "`) || !strings.Contains(frames[2], `"reasoning_content":"Second thought."`) || !strings.Contains(frames[3], `"content":"Final answer"`) || frames[5] != "data: [DONE]" {
		t.Fatalf("unexpected progress stream: code=%d frames=%q", w.Code, frames)
	}
	if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "abandoned") {
		t.Fatal("stream exposed internal content")
	}
}

func TestStreamFailureAfterProgress(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		g := testGateway(t)
		g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
			req.StreamFunc(agent.StreamChunk{Type: agent.ChunkThinking, Text: "visible reasoning"})
			var err error
			if canceled {
				err = responder.(gatewayruntime.CancellationResponder).CancelAgentResponse()
			} else {
				err = responder.SendAgentError("private provider failure")
			}
			if err != nil {
				t.Error(err)
			}
			return gatewayruntime.Outcome{}
		}
		w := request(g, "POST", "/v1/chat/completions", `{"model":"oswald","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "Bearer "+testAPIKey)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"error":{"code":"server_error"`) || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
			t.Fatalf("canceled=%t stream error: code=%d body=%s", canceled, w.Code, w.Body.String())
		}
	}
}

func TestStreamWithoutReasoningDoesNotInventIt(t *testing.T) {
	g := testGateway(t)
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		req.StreamFunc(agent.StreamChunk{Type: agent.ChunkStatus, Text: "private status"})
		req.StreamFunc(agent.StreamChunk{Type: agent.ChunkContent, Text: "abandoned draft"})
		if err := responder.SendAgentResponse(&agent.Response{Response: "Final answer"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	w := request(g, "POST", "/v1/chat/completions", `{"model":"oswald","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "Bearer "+testAPIKey)
	if w.Code != 200 || !w.Flushed || strings.Contains(w.Body.String(), "reasoning_content") || strings.Contains(w.Body.String(), "abandoned") || strings.Contains(w.Body.String(), "private") || !strings.Contains(w.Body.String(), "Final answer") {
		t.Fatalf("unexpected stream without reasoning: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStreamRejectsAttachmentsAfterProgress(t *testing.T) {
	g := testGateway(t)
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		req.StreamFunc(agent.StreamChunk{Type: agent.ChunkContent, Text: "discarded draft"})
		if err := responder.SendAgentResponse(&agent.Response{Response: "answer", Attachments: []media.OutputAttachment{{Filename: "image.png", Data: []byte("private image")}}}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	w := request(g, "POST", "/v1/chat/completions", `{"model":"oswald","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "Bearer "+testAPIKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":"unsupported_response"`) || strings.Contains(w.Body.String(), "discarded") || strings.Contains(w.Body.String(), "private image") || strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("attachment failure: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFixedAPIProfileAndPreRuntimeLogging(t *testing.T) {
	var logs bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&logs)
	svc, err := profiles.NewDirectory(&config.Config{ProfileRoot: t.TempDir(), ProfileName: "default", OpenAIListenHost: "127.0.0.1", OpenAIListenPort: "12345", OpenAIAPIKey: testAPIKey, Profiles: map[string]*config.Config{"api": {ProfileName: "api"}}}, log)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := svc.ResolveAPIKey(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if principal.CanonicalUserID != "default" {
		t.Fatal("API selected another profile")
	}
	g, err := New("127.0.0.1", "12345", svc, gatewayruntime.Dependencies{}, "route", log)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		called++
		if req.Principal.CanonicalUserID != "default" || !req.Stateless {
			t.Fatal("incorrect local request")
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "ok"}); err != nil {
			t.Fatal(err)
		}
		return gatewayruntime.Outcome{}
	}
	valid := `{"model":"oswald","messages":[{"role":"user","content":"hi"}]}`
	if w := request(g, "POST", "/v1/chat/completions", valid, ""); w.Code != 401 {
		t.Fatalf("keyless request: %d", w.Code)
	}
	if w := request(g, "POST", "/v1/chat/completions", `{"model":"oswald","tools":[]}`); w.Code != 400 {
		t.Fatalf("invalid json: %d", w.Code)
	}
	if w := request(g, "POST", "/v1/chat/completions", valid, "Bearer obsolete-token"); w.Code != 401 {
		t.Fatalf("bad key must be rejected: %d", w.Code)
	}
	if w := request(g, "POST", "/v1/chat/completions", valid); w.Code != 200 {
		t.Fatalf("valid key request: %d", w.Code)
	}
	if called != 1 || strings.Contains(logs.String(), "obsolete-token") || strings.Contains(logs.String(), testAPIKey) || strings.Contains(logs.String(), "Bearer") {
		t.Fatal("unexpected execution or private log data")
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		counts[record["event"].(string)]++
	}
	if counts["gateway.openai.identity.complete"] != 4 || counts["gateway.openai.request.rejected"] != 1 || counts["gateway.request.complete"] != 0 {
		t.Fatalf("unexpected gateway measurements: %v", counts)
	}
}

type failingWriter struct {
	header http.Header
	code   int
}

func (w *failingWriter) Header() http.Header       { return w.header }
func (w *failingWriter) WriteHeader(code int)      { w.code = code }
func (w *failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type flushFailWriter struct{ *httptest.ResponseRecorder }

func (w *flushFailWriter) FlushError() error { return io.ErrClosedPipe }

func TestResponderDeliveryFailures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		w := &failingWriter{header: make(http.Header)}
		r := &responseWriter{w: w, ctx: context.Background(), stream: stream}
		if err := r.SendAgentResponse(&agent.Response{Response: "answer"}); !errors.Is(err, io.ErrClosedPipe) || !r.sent {
			t.Fatalf("stream=%t write failure: %v", stream, err)
		}
	}
	flush := &responseWriter{w: &flushFailWriter{httptest.NewRecorder()}, ctx: context.Background(), stream: true}
	if err := flush.SendAgentResponse(&agent.Response{Response: "answer"}); !errors.Is(err, io.ErrClosedPipe) || !flush.sent || strings.Contains(flush.w.(*flushFailWriter).Body.String(), "[DONE]") {
		t.Fatalf("flush failure: %v", err)
	}
	progress := &responseWriter{w: &failingWriter{header: make(http.Header)}, ctx: context.Background(), stream: true}
	progress.Stream(agent.StreamChunk{Type: agent.ChunkThinking, Text: "private"})
	if !errors.Is(progress.streamErr, io.ErrClosedPipe) || !errors.Is(progress.SendAgentResponse(&agent.Response{Response: "answer"}), io.ErrClosedPipe) {
		t.Fatalf("progress write failure: %v", progress.streamErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &failingWriter{header: make(http.Header)}
	r := &responseWriter{w: w, ctx: ctx}
	if err := r.SendAgentResponse(&agent.Response{Response: "answer"}); !errors.Is(err, context.Canceled) || r.sent || w.code != 0 {
		t.Fatalf("canceled delivery: %v", err)
	}
}

func TestModelsAdvertiseProfileContext(t *testing.T) {
	log := config.NewLogger(config.LevelDebug)
	directory, err := profiles.NewDirectory(&config.Config{ProfileRoot: t.TempDir(), ProfileName: "default", OpenAIListenHost: "127.0.0.1", OpenAIListenPort: "12345", OpenAIAPIKey: testAPIKey, ModelContextWindow: 262144, Profiles: map[string]*config.Config{"api": {ProfileName: "api"}}}, log)
	if err != nil {
		t.Fatal(err)
	}
	g, err := New("127.0.0.1", "12345", directory, gatewayruntime.Dependencies{}, "route", log)
	if err != nil {
		t.Fatal(err)
	}
	g.resolveKey = func(raw string) (identity.Principal, error) {
		if raw != testAPIKey {
			return identity.Principal{}, profiles.ErrUnmappedIdentity
		}
		return identity.Principal{CanonicalUserID: "default", Gateway: "openai", ExternalID: "api:default", Assurance: identity.AssuranceAPIKey}, nil
	}
	w := request(g, "GET", "/v1/models", "")
	var result struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength *int   `json:"context_length"`
			Capabilities  *struct {
				Tools  bool     `json:"tools"`
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Data) != 1 || result.Data[0].ID != "oswald" {
		t.Fatalf("models: %d %s", w.Code, w.Body.String())
	}
	if result.Data[0].ContextLength == nil || *result.Data[0].ContextLength != 262144 {
		t.Fatalf("missing context length: %s", w.Body.String())
	}
	caps := result.Data[0].Capabilities
	if caps == nil || !caps.Tools || len(caps.Input) != 2 || len(caps.Output) != 1 {
		t.Fatalf("missing capabilities: %s", w.Body.String())
	}
}

func TestUsageReportedWhenObserved(t *testing.T) {
	for _, stream := range []bool{false, true} {
		g := testGateway(t)
		g.execute = func(_ gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
			err := responder.SendAgentResponse(&agent.Response{Response: "answer", UsagePromptTokens: 10, UsageCompletionTokens: 5, UsageTotalTokens: 15, UsageReported: true, UsageComplete: true})
			if err != nil {
				t.Error(err)
			}
			return gatewayruntime.Outcome{}
		}
		body := `{"model":"oswald","messages":[{"role":"user","content":"hi"}]`
		if stream {
			body += `,"stream":true,"stream_options":{"include_usage":true}}`
		} else {
			body += `}`
		}
		w := request(g, "POST", "/v1/chat/completions", body, "Bearer "+testAPIKey)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"prompt_tokens":10`) || !strings.Contains(w.Body.String(), `"completion_tokens":5`) || !strings.Contains(w.Body.String(), `"total_tokens":15`) {
			t.Fatalf("stream=%t usage: %d %s", stream, w.Code, w.Body.String())
		}
	}
	g := testGateway(t)
	g.execute = func(_ gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if err := responder.SendAgentResponse(&agent.Response{Response: "answer"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	w := request(g, "POST", "/v1/chat/completions", `{"model":"oswald","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, "Bearer "+testAPIKey)
	if w.Code != 200 || strings.Contains(w.Body.String(), "prompt_tokens") {
		t.Fatalf("unreported usage must be omitted: %d %s", w.Code, w.Body.String())
	}
}

func TestModelsAndUnsupportedOutput(t *testing.T) {
	g := testGateway(t)
	w := request(g, "GET", "/v1/models", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"oswald"`) {
		t.Fatalf("models: %d %s", w.Code, w.Body.String())
	}
	g.execute = func(_ gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		_ = responder.SendAgentError("private provider error")
		return gatewayruntime.Outcome{}
	}
	w = request(g, "POST", "/v1/chat/completions", `{"model":"oswald","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "Bearer "+testAPIKey)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private") || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("error: %d %s", w.Code, w.Body.String())
	}
}

func TestLocalIdentityUnavailable(t *testing.T) {
	g := testGateway(t)
	g.resolveKey = func(string) (identity.Principal, error) {
		return identity.Principal{}, profiles.ErrUnmappedIdentity
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/models", ""},
		{http.MethodPost, "/v1/chat/completions", `{"model":"oswald","messages":[{"role":"user","content":"hi"}]}`},
	} {
		w := request(g, tc.method, tc.path, tc.body)
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), testAPIKey) || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("unavailable identity: code=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestCompatGenerationOptionsIgnored(t *testing.T) {
	g := testGateway(t)
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if req.Text != "hi" {
			t.Errorf("unexpected text: %q", req.Text)
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "ok"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	body := `{"model":"oswald","temperature":0.7,"top_p":0.9,"top_k":40,"frequency_penalty":0.5,"presence_penalty":0.5,"stop":["END"],"seed":7,"user":"opencode","response_format":{"type":"text"},"n":1,"max_completion_tokens":512,"store":false,"messages":[{"role":"user","content":"hi"}]}`
	w := request(g, "POST", "/v1/chat/completions", body, "Bearer "+testAPIKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("compat options: %d %s", w.Code, w.Body.String())
	}
}

func TestBannedRequestHasNoResponseBody(t *testing.T) {
	g := testGateway(t)
	g.execute = func(_ gatewayruntime.Request, _ gatewayruntime.Dependencies, _ gatewayruntime.Responder) gatewayruntime.Outcome {
		return gatewayruntime.Outcome{Action: routing.ActionIgnore, Reason: "user_banned"}
	}
	w := request(g, "POST", "/v1/chat/completions", `{"model":"oswald","messages":[{"role":"user","content":"hi"}]}`, "Bearer "+testAPIKey)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("banned request status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestLargeHistoryPassesToTokenBudget(t *testing.T) {
	g := testGateway(t)
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if len(req.ClientHistory) != 1 || len(req.ClientHistory[0].Content) != 100<<10 {
			t.Errorf("large history not forwarded: %+v", len(req.ClientHistory))
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "ok"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	body := `{"model":"oswald","messages":[{"role":"user","content":"` + strings.Repeat("a", 100<<10) + `"},{"role":"user","content":"hi"}]}`
	w := request(g, "POST", "/v1/chat/completions", body, "Bearer "+testAPIKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("large history: %d", w.Code)
	}
}

func TestManyTurnsPassToTokenBudget(t *testing.T) {
	g := testGateway(t)
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if len(req.ClientHistory) != 64 {
			t.Errorf("turns not forwarded: %d", len(req.ClientHistory))
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "ok"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	var turns strings.Builder
	turns.WriteString(`{"model":"oswald","messages":[`)
	for i := 0; i < 64; i++ {
		if i > 0 {
			turns.WriteString(",")
		}
		if i%2 == 0 {
			turns.WriteString(`{"role":"user","content":"q"}`)
		} else {
			turns.WriteString(`{"role":"assistant","content":"a"}`)
		}
	}
	turns.WriteString(`,{"role":"user","content":"hi"}]}`)
	w := request(g, "POST", "/v1/chat/completions", turns.String(), "Bearer "+testAPIKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("many turns: %d %s", w.Code, w.Body.String())
	}
}

func TestOverBudgetMapsToBadRequest(t *testing.T) {
	g := testGateway(t)
	g.execute = func(_ gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if err := responder.SendAgentResponse(&agent.Response{Kind: "over_budget", Model: "route"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	w := request(g, "POST", "/v1/chat/completions", `{"model":"oswald","messages":[{"role":"user","content":"hi"}]}`, "Bearer "+testAPIKey)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("over budget status=%d body=%q", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["error"] == nil {
		t.Fatalf("missing JSON error: %v", err)
	}
}

func TestClientToolCompatShapes(t *testing.T) {
	g := testGateway(t)
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if len(req.ClientTools) != 3 {
			t.Errorf("compat tools not forwarded: %d", len(req.ClientTools))
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "ok"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	body := `{"model":"oswald","tools":[
		{"type":"function","strict":true,"function":{"name":"opencode.list_resources","description":"Line one.\nLine two with detail.","parameters":{"type":"object","properties":{"server":{"type":"string","description":"MCP server","format":"hostname"}},"required":["server"],"additionalProperties":false}}},
		{"type":"function","function":{"name":"quota_status","parameters":{"type":"object"}}},
		{"type":"function","function":{"name":"read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"$schema":"http://json-schema.org/draft-07/schema#"}}}
	],"messages":[{"role":"user","content":"hi"}]}`
	w := request(g, "POST", "/v1/chat/completions", body, "Bearer "+testAPIKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("compat tools: %d %s", w.Code, w.Body.String())
	}
}

func TestToolContinuationEndsWithTool(t *testing.T) {
	g := testGateway(t)
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if req.Text == "" || len(req.ClientHistory) != 3 || req.ClientHistory[2].Role != "tool" {
			t.Errorf("unexpected continuation request: %+v", req)
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "done"}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	body := `{"model":"oswald","messages":[
		{"role":"user","content":"weather in Paris?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"{\"temp\":21}"}
	]}`
	w := request(g, "POST", "/v1/chat/completions", body, "Bearer "+testAPIKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "done") {
		t.Fatalf("continuation: %d %s", w.Code, w.Body.String())
	}
}

func TestClientToolCallsSerialized(t *testing.T) {
	g := testGateway(t)
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		if len(req.ClientTools) != 1 || req.ClientTools[0].Function.Name != "get_weather" {
			t.Errorf("client tools not forwarded: %+v", req.ClientTools)
		}
		if err := responder.SendAgentResponse(&agent.Response{Kind: "tool_calls", Response: "Checking.", ToolCalls: []llm.ToolCall{{ID: "call_1", Function: llm.ToolFunction{Name: "get_weather", Arguments: map[string]interface{}{"city": "Paris"}, RawArguments: `{"city":"Paris"}`}}}}); err != nil {
			t.Error(err)
		}
		return gatewayruntime.Outcome{}
	}
	body := `{"model":"oswald","tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],"messages":[{"role":"user","content":"weather in Paris?"}]}`
	w := request(g, "POST", "/v1/chat/completions", body, "Bearer "+testAPIKey)
	var result struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Choices) != 1 || result.Choices[0].FinishReason != "tool_calls" || len(result.Choices[0].Message.ToolCalls) != 1 || result.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool call response: %d %s", w.Code, w.Body.String())
	}
}
