package websocket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestWebSocketGatewayPlainTextAndCommand(t *testing.T) {
	wg, b, chat := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	conn := dialWebSocket(t, server.URL)
	defer conn.Close()

	if err := conn.WriteMessage(gorilla.TextMessage, []byte("hello websocket")); err != nil {
		t.Fatalf("write plain text: %v", err)
	}
	plain := readAgentResponse(t, conn)
	if plain.Response != "ws response" {
		t.Fatalf("unexpected plain response: %+v", plain)
	}
	primary := primaryWSRequests(chat.requests)
	if len(primary) != 1 || primary[0].Messages[len(primary[0].Messages)-1].Content != "hello websocket" {
		t.Fatalf("unexpected chat requests: %+v", primary)
	}

	if err := conn.WriteJSON(IncomingMessage{UserID: "alice", Prompt: "/ping"}); err != nil {
		t.Fatalf("write command: %v", err)
	}
	cmd := readAgentResponse(t, conn)
	if cmd.Response != "pong" {
		t.Fatalf("unexpected command response: %+v", cmd)
	}
	if len(primaryWSRequests(chat.requests)) != 1 {
		t.Fatalf("command should not call LLM, got %d calls", len(primaryWSRequests(chat.requests)))
	}
}

func TestWebSocketGatewayStructuredImageDowngrade(t *testing.T) {
	wg, b, chat := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	conn := dialWebSocket(t, server.URL)
	defer conn.Close()

	msg := IncomingMessage{
		UserID: "alice",
		Prompt: "describe this",
		Images: []IncomingImage{{MimeType: "image/png", Data: "not-base64", Source: "bad.png"}},
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("write structured message: %v", err)
	}
	resp := readAgentResponse(t, conn)
	if resp.Response != "ws response" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	primary := primaryWSRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one chat request, got %d", len(primary))
	}
	prompt := primary[0].Messages[len(primary[0].Messages)-1].Content
	if !strings.Contains(prompt, "describe this") || !strings.Contains(prompt, "unsupported attachment: bad.png") {
		t.Fatalf("unexpected prompt %q", prompt)
	}
	if chat.principal.CanonicalUserID == "" || chat.principal.Gateway != "websocket" || chat.principal.ExternalID != "alice" || chat.principal.Assurance != identity.AssuranceSelfAsserted {
		t.Fatalf("unexpected principal: %+v", chat.principal)
	}
}

type wsFakeChatter struct {
	requests  []llm.ChatRequest
	principal identity.Principal
}

func (f *wsFakeChatter) Chat(ctx context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.requests = append(f.requests, req)
	f.principal, _ = requestctx.PrincipalFromContext(ctx)
	if req.Format == "json_object" {
		return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: `{"session_updates":{"summary":"","open_threads":[],"decisions":[],"user_goals":[]},"memory_candidates":[]}`}}, nil
	}
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ws response"}}, nil
}

func primaryWSRequests(requests []llm.ChatRequest) []llm.ChatRequest {
	out := make([]llm.ChatRequest, 0, len(requests))
	for _, req := range requests {
		if req.Format != "json_object" {
			out = append(out, req)
		}
	}
	return out
}

type wsPingHandler struct{}

func (wsPingHandler) Definition() commands.Definition { return commands.Definition{Name: "ping"} }
func (wsPingHandler) Execute(context.Context, commands.Request) (commands.Result, error) {
	return commands.Result{Text: "pong"}, nil
}

func newWebSocketTestGateway(t *testing.T) (*Gateway, *broker.Broker, *wsFakeChatter) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	links := accountlinking.NewService(filepath.Join(dir, "oswald.db"), memories, log)
	soulStore := soul.NewStore(filepath.Join(dir, "soul.md"), log)
	if err := soulStore.Write("You are Oswald."); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	chat := &wsFakeChatter{}
	ai := agent.NewAgent(chat, registry.New(log), "test-model", soulStore, memories, promptbudget.ContextBudget{PromptLimit: 100000}, 3, time.Minute, log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	commandService, err := commands.NewService(wsPingHandler{})
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	wg := &Gateway{Links: links, Runtime: gatewayruntime.Dependencies{Commands: commandService, Log: log}, Log: log}
	return wg, b, chat
}

func dialWebSocket(t *testing.T, serverURL string) *gorilla.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http")
	conn, _, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	return conn
}

func readAgentResponse(t *testing.T, conn *gorilla.Conn) agent.AgentResponse {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp agent.AgentResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatalf("decode response %s: %v", payload, err)
	}
	return resp
}
