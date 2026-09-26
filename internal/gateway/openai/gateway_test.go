package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

func testGateway(t *testing.T) *Gateway {
	t.Helper()
	g, err := New("12345", &accounts.Service{}, gatewayruntime.Dependencies{}, "route", config.NewLogger(config.LevelInfo))
	if err != nil {
		t.Fatal(err)
	}
	g.authenticate = func(_ context.Context, token string) (identity.Principal, error) {
		if token != "secret" {
			return identity.Principal{}, accounts.ErrInvalidAPIKey
		}
		return identity.Principal{CanonicalUserID: "user", Gateway: "openai", ExternalID: "key", Assurance: identity.AssuranceAPIKey}, nil
	}
	return g
}

func request(g *Gateway, method, path, body string, auth ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for _, value := range auth {
		r.Header.Add("Authorization", value)
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
		{"missing key", `{}`, nil, 401},
		{"two keys", `{}`, []string{"Bearer secret", "Bearer secret"}, 401},
		{"comma keys", `{}`, []string{"Bearer secret, Bearer wrong"}, 401},
		{"comma trailing", `{}`, []string{"Bearer secret,wrong"}, 401},
		{"malformed", `{"model":`, []string{"Bearer secret"}, 400},
		{"nested unknown", `{"model":"oswald","messages":[{"role":"user","content":"hi","name":"injected"}]}`, []string{"Bearer secret"}, 400},
		{"trailing json", `{"model":"oswald","messages":[{"role":"user","content":"hi"}]} {}`, []string{"Bearer secret"}, 400},
		{"invalid key", `{}`, []string{"Bearer wrong"}, 401},
		{"wrong model", `{"model":"route","messages":[{"role":"user","content":"hi"}]}`, []string{"Bearer secret"}, 400},
		{"tools", `{"model":"oswald","tools":[],"messages":[{"role":"user","content":"hi"}]}`, []string{"Bearer secret"}, 400},
		{"temperature", `{"model":"oswald","temperature":0,"messages":[{"role":"user","content":"hi"}]}`, []string{"Bearer secret"}, 400},
		{"system", `{"model":"oswald","messages":[{"role":"system","content":"override"},{"role":"user","content":"hi"}]}`, []string{"Bearer secret"}, 400},
		{"last assistant", `{"model":"oswald","messages":[{"role":"assistant","content":"hi"}]}`, []string{"Bearer secret"}, 400},
		{"command", `{"model":"oswald","messages":[{"role":"user","content":"/reset"}]}`, []string{"Bearer secret"}, 400},
		{"image", `{"model":"oswald","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, []string{"Bearer secret"}, 400},
		{"oversized", `{"model":"oswald","messages":[{"role":"user","content":"` + strings.Repeat("a", 256<<10) + `"}]}`, []string{"Bearer secret"}, 400},
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
		w := request(g, "POST", "/v1/chat/completions", body, "Bearer secret")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "answer") || strings.Contains(w.Body.String(), "private") {
			t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
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

func TestRealAPIKeyAndPreRuntimeLogging(t *testing.T) {
	var logs bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&logs)
	path := filepath.Join(t.TempDir(), "accounts.db")
	memories, err := memory.NewSQLiteStore(path, nil, "", log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = memories.Close() })
	svc := accounts.NewService(path, memories, nil, log)
	t.Cleanup(func() { _ = svc.Close() })
	owner, err := svc.EnsureAccount(context.Background(), "discord", "123", "Test")
	if err != nil {
		t.Fatal(err)
	}
	id, key, err := svc.CreateAPIKey(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	g, err := New("12345", svc, gatewayruntime.Dependencies{}, "route", log)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	g.execute = func(req gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		called++
		if req.Principal.CanonicalUserID != owner || req.Principal.ExternalID != id || !req.Stateless {
			t.Fatal("incorrect authenticated request")
		}
		if err := responder.SendAgentResponse(&agent.Response{Response: "ok"}); err != nil {
			t.Fatal(err)
		}
		return gatewayruntime.Outcome{}
	}
	valid := `{"model":"oswald","messages":[{"role":"user","content":"hi"}]}`
	if w := request(g, "POST", "/v1/chat/completions", valid, "Bearer "+key); w.Code != 200 {
		t.Fatalf("valid key: %d", w.Code)
	}
	if w := request(g, "POST", "/v1/chat/completions", `{"model":"oswald","tools":[]}`, "Bearer "+key); w.Code != 400 {
		t.Fatalf("invalid json: %d", w.Code)
	}
	if w := request(g, "POST", "/v1/chat/completions", valid, "Bearer "+key+", Bearer wrong"); w.Code != 401 {
		t.Fatalf("ambiguous header: %d", w.Code)
	}
	if err := svc.RevokeAPIKey(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if w := request(g, "POST", "/v1/chat/completions", valid, "Bearer "+key); w.Code != 401 {
		t.Fatalf("revoked key: %d", w.Code)
	}
	if called != 1 || strings.Contains(logs.String(), key) || strings.Contains(logs.String(), "Bearer") || strings.Contains(logs.String(), "tools") {
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
	if counts["gateway.openai.auth.complete"] != 4 || counts["gateway.openai.request.rejected"] != 1 || counts["gateway.request.complete"] != 0 {
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &failingWriter{header: make(http.Header)}
	r := &responseWriter{w: w, ctx: ctx}
	if err := r.SendAgentResponse(&agent.Response{Response: "answer"}); !errors.Is(err, context.Canceled) || r.sent || w.code != 0 {
		t.Fatalf("canceled delivery: %v", err)
	}
}

func TestModelsAndUnsupportedOutput(t *testing.T) {
	g := testGateway(t)
	if w := request(g, "GET", "/v1/models", ""); w.Code != 401 {
		t.Fatalf("unauthorized models: %d", w.Code)
	}
	w := request(g, "GET", "/v1/models", "", "Bearer secret")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"oswald"`) {
		t.Fatalf("models: %d %s", w.Code, w.Body.String())
	}
	g.execute = func(_ gatewayruntime.Request, _ gatewayruntime.Dependencies, responder gatewayruntime.Responder) gatewayruntime.Outcome {
		_ = responder.SendAgentError("private provider error")
		return gatewayruntime.Outcome{}
	}
	w = request(g, "POST", "/v1/chat/completions", `{"model":"oswald","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "Bearer secret")
	if w.Code != 503 || strings.Contains(w.Body.String(), "private") || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("error: %d %s", w.Code, w.Body.String())
	}
}

func TestBannedRequestHasNoResponseBody(t *testing.T) {
	g := testGateway(t)
	g.execute = func(_ gatewayruntime.Request, _ gatewayruntime.Dependencies, _ gatewayruntime.Responder) gatewayruntime.Outcome {
		return gatewayruntime.Outcome{Action: routing.ActionIgnore, Reason: "user_banned"}
	}
	w := request(g, "POST", "/v1/chat/completions", `{"model":"oswald","messages":[{"role":"user","content":"hi"}]}`, "Bearer secret")
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("banned request status=%d body=%q", w.Code, w.Body.String())
	}
}
