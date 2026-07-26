package websocket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/privacyruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/websocketauth"
)

const testSigningKey = "0123456789abcdef0123456789abcdef"

func TestWebSocketGatewayPlainTextAndCommand(t *testing.T) {
	wg, b, chat := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	conn := dialWebSocket(t, server.URL, wg, "alice")
	defer conn.Close()

	if err := conn.WriteMessage(gorilla.TextMessage, []byte("hello websocket")); err != nil {
		t.Fatalf("write plain text: %v", err)
	}
	plain := readAgentResponse(t, conn)
	if plain.Response != "ws response" {
		t.Fatalf("unexpected plain response: %+v", plain)
	}
	primary := primaryWSRequests(chat.requests)
	if len(primary) != 1 || primary[0].Messages[len(primary[0].Messages)-1].Content != "hello websocket" || !strings.Contains(primary[0].Messages[len(primary[0].Messages)-2].Content, "<tenant_profile") {
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

func TestPrivacyInvalidationClosesOnlyMatchingWebSocketConnections(t *testing.T) {
	wg, b, _ := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { wg.handleConnections(w, r, b) }))
	defer server.Close()
	matching := dialWebSocket(t, server.URL, wg, "matching")
	defer matching.Close()
	foreign := dialWebSocket(t, server.URL, wg, "foreign")
	defer foreign.Close()

	wg.HandlePrivacyInvalidation(privacyruntime.Event{ExternalIdentities: []string{"websocket:matching"}, CloseConnections: true})
	_ = matching.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := matching.ReadMessage(); err == nil {
		t.Fatal("matching websocket connection remained open")
	}
	if err := foreign.WriteMessage(gorilla.TextMessage, []byte("still connected")); err != nil {
		t.Fatalf("foreign websocket connection was closed: %v", err)
	}
	if response := readAgentResponse(t, foreign); response.Response == "" {
		t.Fatalf("foreign websocket response=%+v", response)
	}
}

func TestWebSocketCommandAttachmentResponse(t *testing.T) {
	wg, b, _ := newWebSocketTestGateway(t)
	defer b.Shutdown()
	service, err := commands.NewService(commands.HandlerFunc{
		DefinitionValue: commands.Definition{Name: "export"},
		ExecuteFunc: func(context.Context, commands.Request) (commands.Result, error) {
			return commands.Result{Text: "export ready", Attachment: &commands.Attachment{
				Filename: "export.json", MIMEType: "application/json", Data: []byte(`{"secret":true}`),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wg.Runtime.Commands = service
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()
	conn := dialWebSocket(t, server.URL, wg, "alice")
	defer conn.Close()
	if err := conn.WriteMessage(gorilla.TextMessage, []byte("/export")); err != nil {
		t.Fatal(err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var response CommandResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("decode command response %s: %v", payload, err)
	}
	if response.Type != "command_response" || response.Response != "export ready" || response.Attachment == nil || response.Attachment.Filename != "export.json" || response.Attachment.MIMEType != "application/json" || response.Attachment.Data != "eyJzZWNyZXQiOnRydWV9" {
		t.Fatalf("unexpected command attachment response: %+v", response)
	}
}

func TestWebSocketCommandAttachmentArrayPreservesOrder(t *testing.T) {
	wg, b, _ := newWebSocketTestGateway(t)
	defer b.Shutdown()
	service, err := commands.NewService(commands.HandlerFunc{
		DefinitionValue: commands.Definition{Name: "export"},
		ExecuteFunc: func(context.Context, commands.Request) (commands.Result, error) {
			return commands.Result{Text: "join in order", Attachments: []commands.Attachment{
				{Filename: "export.json.part001", MIMEType: "application/octet-stream", Data: []byte("first")},
				{Filename: "export.json.part002", MIMEType: "application/octet-stream", Data: []byte("second")},
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wg.Runtime.Commands = service
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { wg.handleConnections(w, r, b) }))
	defer server.Close()
	conn := dialWebSocket(t, server.URL, wg, "alice")
	defer conn.Close()
	if err := conn.WriteMessage(gorilla.TextMessage, []byte("/export")); err != nil {
		t.Fatal(err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var response CommandResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	if response.Attachment != nil || len(response.Attachments) != 2 || response.Attachments[0].Filename != "export.json.part001" || response.Attachments[0].Data != "Zmlyc3Q=" || response.Attachments[1].Filename != "export.json.part002" || response.Attachments[1].Data != "c2Vjb25k" {
		t.Fatalf("unexpected ordered attachments: %+v", response)
	}
}

func TestWebSocketGatewayStructuredImageDowngrade(t *testing.T) {
	wg, b, chat := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	conn := dialWebSocket(t, server.URL, wg, "alice")
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
	if chat.principal.CanonicalUserID == "" || chat.principal.Gateway != "websocket" || chat.principal.ExternalID != "alice" || chat.principal.Assurance != identity.AssuranceWebSocketSignedToken || !chat.principal.Authenticated() {
		t.Fatalf("unexpected principal: %+v", chat.principal)
	}
}

func TestWebSocketGatewayRejectsUnauthenticatedHandshake(t *testing.T) {
	wg, b, chat := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	_, resp, err := gorilla.DefaultDialer.Dial(wsURL, nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("unexpected unauthenticated handshake: response=%+v err=%v", resp, err)
	}
	users, err := wg.Links.ListUsers()
	if err != nil || len(users) != 0 || len(chat.requests) != 0 {
		t.Fatalf("authentication failure created state: users=%+v requests=%d err=%v", users, len(chat.requests), err)
	}
}

func TestWebSocketGatewayRejectsIdentitySwitch(t *testing.T) {
	wg, b, _ := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	conn := dialWebSocket(t, server.URL, wg, "alice")
	defer conn.Close()
	if err := conn.WriteJSON(IncomingMessage{UserID: "bob", Prompt: "hello"}); err != nil {
		t.Fatalf("write mismatched identity: %v", err)
	}
	_, _, err := conn.ReadMessage()
	closeErr, ok := err.(*gorilla.CloseError)
	if !ok || closeErr.Code != gorilla.ClosePolicyViolation {
		t.Fatalf("identity switch error = %v, want policy violation", err)
	}
	users, err := wg.Links.ListUsers()
	if err != nil || len(users) != 1 || users[0].Accounts[0].Identifier != "alice" {
		t.Fatalf("identity switch resolved another account: users=%+v err=%v", users, err)
	}
}

func TestWebSocketGatewayRejectsCrossOrigin(t *testing.T) {
	wg, b, _ := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	token := issueWebSocketToken(t, wg, "alice")
	headers := http.Header{"Authorization": []string{"Bearer " + token}, "Origin": []string{"https://evil.example"}}
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	_, resp, err := gorilla.DefaultDialer.Dial(wsURL, headers)
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected cross-origin handshake: response=%+v err=%v", resp, err)
	}
}

func TestWebSocketGatewayDoesNotCreateAccountBeforeUpgradeAndMessage(t *testing.T) {
	wg, b, _ := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	token := issueWebSocketToken(t, wg, "alice")
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request malformed upgrade: %v", err)
	}
	resp.Body.Close()
	users, err := wg.Links.ListUsers()
	if err != nil || len(users) != 1 {
		t.Fatalf("malformed upgrade changed authorized accounts: users=%+v err=%v", users, err)
	}
}

func TestWebSocketGatewayClosesConnectionWhenClientIsRevoked(t *testing.T) {
	wg, b, _ := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	token := issueWebSocketToken(t, wg, "alice")
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := gorilla.DefaultDialer.Dial(wsURL, http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()
	owner, ok, err := wg.Links.ResolveAccount("websocket", "alice")
	if err != nil || !ok {
		t.Fatalf("resolve owner: ok=%v err=%v", ok, err)
	}
	clients, err := wg.Auth.ListClients(context.Background(), owner)
	if err != nil || len(clients) != 1 {
		t.Fatalf("list clients: clients=%+v err=%v", clients, err)
	}
	if err := wg.Auth.RevokeClient(context.Background(), owner, clients[0].ClientID); err != nil {
		t.Fatalf("revoke client: %v", err)
	}
	if err := conn.WriteMessage(gorilla.TextMessage, []byte("after revoke")); err != nil {
		t.Fatalf("write after revoke: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	closeErr, ok := err.(*gorilla.CloseError)
	if !ok || closeErr.Code != gorilla.ClosePolicyViolation {
		t.Fatalf("client revocation error = %v, want policy violation", err)
	}
}

func TestWebSocketGatewayIgnoresMessageDisplayNameOverride(t *testing.T) {
	wg, b, _ := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	token := issueWebSocketToken(t, wg, "alice")
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := gorilla.DefaultDialer.Dial(wsURL, http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(IncomingMessage{DisplayName: "Alice Example", Prompt: "/ping"}); err != nil {
		t.Fatalf("write message: %v", err)
	}
	_ = readAgentResponse(t, conn)
	users, err := wg.Links.ListUsers()
	if err != nil || len(users) != 1 || len(users[0].Accounts) != 1 || users[0].Accounts[0].DisplayName != "Alice" {
		t.Fatalf("authorized display name was overridden: users=%+v err=%v", users, err)
	}
}

func TestWebSocketGatewayRefreshesCanonicalOwnerAfterAccountMerge(t *testing.T) {
	wg, b, chat := newWebSocketTestGateway(t)
	defer b.Shutdown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	}))
	defer server.Close()

	conn := dialWebSocket(t, server.URL, wg, "alice")
	defer conn.Close()
	if err := conn.WriteMessage(gorilla.TextMessage, []byte("first")); err != nil {
		t.Fatalf("write first message: %v", err)
	}
	_ = readAgentResponse(t, conn)
	websocketOwner, ok, err := wg.Links.ResolveAccount("websocket", "alice")
	if err != nil || !ok {
		t.Fatalf("resolve websocket owner: owner=%q ok=%v err=%v", websocketOwner, ok, err)
	}
	discordOwner, err := wg.Links.EnsureAccount("discord", "900", "Discord Owner")
	if err != nil {
		t.Fatalf("ensure discord owner: %v", err)
	}
	challenge, err := wg.Links.CreateChallenge(context.Background(), identity.Principal{CanonicalUserID: discordOwner, Gateway: "discord", ExternalID: "900", Assurance: identity.AssuranceDiscordGateway}, "req")
	if err != nil {
		t.Fatalf("create challenge: %v", err)
	}
	if _, err := wg.Links.ConfirmChallenge(context.Background(), identity.Principal{CanonicalUserID: websocketOwner, Gateway: "websocket", ExternalID: "alice", Assurance: identity.AssuranceWebSocketSignedToken}, challenge.Code, "req"); err != nil {
		t.Fatalf("confirm challenge: %v", err)
	}

	if err := conn.WriteMessage(gorilla.TextMessage, []byte("second")); err != nil {
		t.Fatalf("write second message: %v", err)
	}
	_ = readAgentResponse(t, conn)
	if chat.principal.CanonicalUserID != discordOwner {
		t.Fatalf("refreshed principal user = %q, want %q", chat.principal.CanonicalUserID, discordOwner)
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
	dbPath := filepath.Join(dir, "oswald.db")
	memories := usermemory.NewStore(dbPath, log)
	links := accountlinking.NewService(dbPath, memories, nil, log)
	soulPath := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(soulPath, []byte("You are Oswald."), 0o600); err != nil {
		t.Fatalf("write soul fixture: %v", err)
	}
	soulStore := soul.NewStore(soulPath)
	chat := &wsFakeChatter{}
	ai := agent.NewAgent(chat, registry.New(log), "test-model", soulStore, memories, promptbudget.ContextBudget{PromptLimit: 100000}, 3, time.Minute, log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	commandService, err := commands.NewService(wsPingHandler{})
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	authDB, err := database.Open(dbPath, log)
	if err != nil {
		t.Fatalf("open auth database: %v", err)
	}
	t.Cleanup(func() { _ = authDB.Close() })
	auth, err := websocketauth.New(authDB.SQL(), testSigningKey, 15*time.Minute)
	if err != nil {
		t.Fatalf("new authorization store: %v", err)
	}
	wg := &Gateway{Auth: auth, Links: links, Runtime: gatewayruntime.Dependencies{Commands: commandService, Log: log}, Log: log}
	return wg, b, chat
}

func dialWebSocket(t *testing.T, serverURL string, wg *Gateway, subject string) *gorilla.Conn {
	t.Helper()
	token := issueWebSocketToken(t, wg, subject)
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http")
	conn, _, err := gorilla.DefaultDialer.Dial(wsURL, http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	return conn
}

func issueWebSocketToken(t *testing.T, wg *Gateway, subject string) string {
	t.Helper()
	userID, err := wg.Links.EnsureAccount("websocket", subject, "Alice")
	if err != nil {
		t.Fatalf("ensure websocket account: %v", err)
	}
	device, err := wg.Auth.RequestDevice(context.Background(), "Test Client")
	if err != nil {
		t.Fatalf("request device authorization: %v", err)
	}
	if _, err := wg.Auth.ApproveForUser(context.Background(), userID, "Alice", device.UserCode); err != nil {
		t.Fatalf("approve device authorization: %v", err)
	}
	pair, err := wg.Auth.PollDevice(context.Background(), device.DeviceCode)
	if err != nil {
		t.Fatalf("exchange device authorization: %v", err)
	}
	return pair.AccessToken
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
