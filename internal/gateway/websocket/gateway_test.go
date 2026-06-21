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
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
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
	if len(chat.requests) != 1 || chat.requests[0].Messages[len(chat.requests[0].Messages)-1].Content != "hello websocket" {
		t.Fatalf("unexpected chat requests: %+v", chat.requests)
	}

	if err := conn.WriteJSON(IncomingMessage{UserID: "alice", Prompt: "/ping"}); err != nil {
		t.Fatalf("write command: %v", err)
	}
	cmd := readAgentResponse(t, conn)
	if cmd.Response != "pong" {
		t.Fatalf("unexpected command response: %+v", cmd)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("command should not call LLM, got %d calls", len(chat.requests))
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
	if len(chat.requests) != 1 {
		t.Fatalf("expected one chat request, got %d", len(chat.requests))
	}
	prompt := chat.requests[0].Messages[len(chat.requests[0].Messages)-1].Content
	if !strings.Contains(prompt, "describe this") || !strings.Contains(prompt, "unsupported attachment: bad.png") {
		t.Fatalf("unexpected prompt %q", prompt)
	}
}

type wsFakeChatter struct{ requests []llm.ChatRequest }

func (f *wsFakeChatter) Chat(_ context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.requests = append(f.requests, req)
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ws response"}}, nil
}

type wsPingHandler struct{}

func (wsPingHandler) CanHandle(input string) bool                 { return input == "/ping" }
func (wsPingHandler) Handle(string, string) (string, bool, error) { return "pong", true, nil }

func newWebSocketTestGateway(t *testing.T) (*Gateway, *broker.Broker, *wsFakeChatter) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	links := accountlinking.NewService(filepath.Join(dir, "links.json"), memories, log)
	soulStore := soul.NewStore(filepath.Join(dir, "soul.md"), log)
	if err := soulStore.Write("You are Oswald."); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	chat := &wsFakeChatter{}
	ai := agent.NewAgent(chat, nil, registry.New(log), "test-model", "", soulStore, memories, memory.ContextBudget{PromptLimit: 100000}, 3, time.Minute, memory.NewStore(memory.Options{}, log), log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	wg := &Gateway{Links: links, Commands: commands.NewRouter(wsPingHandler{}), Log: log}
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
