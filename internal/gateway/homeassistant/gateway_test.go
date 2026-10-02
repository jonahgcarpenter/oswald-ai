package homeassistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/profiles"
)

const testToken = "0123456789abcdef0123456789abcdef"

type fakeProcessor struct {
	requests chan agent.Request
}

func (p *fakeProcessor) Process(_ context.Context, request agent.Request) (*agent.Response, error) {
	p.requests <- request
	if request.StreamFunc != nil {
		request.StreamFunc(agent.StreamChunk{Type: agent.ChunkContent, Text: "Hello "})
	}
	return &agent.Response{Model: "test", Response: "Hello world"}, nil
}

func TestNewRejectsShortToken(t *testing.T) {
	links, log := testLinks(t)
	if _, err := New("8000", "short", links, gatewayDependencies(log), log); err == nil {
		t.Fatal("expected short token rejection")
	}
}

func TestAuthenticationRequiresOneExactBearerToken(t *testing.T) {
	links, log := testLinks(t)
	gateway, err := New("8000", testToken, links, gatewayDependencies(log), log)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		values  []string
		allowed bool
	}{
		{name: "missing"},
		{name: "wrong", values: []string{"Bearer wrong"}},
		{name: "wrong scheme", values: []string{testToken}},
		{name: "duplicate", values: []string{"Bearer " + testToken, "Bearer " + testToken}},
		{name: "valid", values: []string{"Bearer " + testToken}, allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/homeassistant/ws", nil)
			for _, value := range test.values {
				request.Header.Add("Authorization", value)
			}
			if got := gateway.authenticate(request); got != test.allowed {
				t.Fatalf("authenticate()=%t want %t", got, test.allowed)
			}
		})
	}
}

func TestConversationIDValidationAllowsHomeAssistantDefinedValues(t *testing.T) {
	for _, value := range []string{"conversation:living room.1", "01KZY2FAP2Y3D8R9R2R8P8Q2YQ"} {
		if !validConversationID(value) {
			t.Fatalf("validConversationID(%q) = false", value)
		}
	}
	for _, value := range []string{"", "bad\nvalue", strings.Repeat("x", 513)} {
		if validConversationID(value) {
			t.Fatalf("validConversationID(%q) = true", value)
		}
	}
}

func TestDecodeRequestRequiresStrictUserAndConversationIdentity(t *testing.T) {
	valid := `{"type":"conversation","request_id":"request-1","user_id":"ha-user-1","conversation_id":"conversation:one","text":"hello"}`
	if _, err := decodeRequest([]byte(valid)); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	for _, payload := range []string{
		`{"type":"conversation","request_id":"request-1","conversation_id":"conversation-1","text":"hello"}`,
		`{"type":"conversation","request_id":"request-1","user_id":"ha-user-1","text":"hello"}`,
		`{"type":"conversation","request_id":"request-1","user_id":"ha-user-1","conversation_id":"conversation-1","text":"hello","unknown":true}`,
	} {
		if _, err := decodeRequest([]byte(payload)); err == nil {
			t.Fatalf("decodeRequest(%s) succeeded", payload)
		}
	}
}

func TestGatewayAuthenticatesAndProcessesOneConversation(t *testing.T) {
	links, log := testLinks(t)
	gateway, err := New("8000", testToken, links, gatewayDependencies(log), log)
	if err != nil {
		t.Fatal(err)
	}
	processor := &fakeProcessor{requests: make(chan agent.Request, 1)}
	requestBroker := broker.NewBroker(processor, 1, log.Server("broker"))
	requestBroker.Start()
	t.Cleanup(requestBroker.Shutdown)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateway.handleConnection(w, r, requestBroker)
	}))
	defer server.Close()

	header := http.Header{"Authorization": []string{"Bearer " + testToken}}
	connection, response, err := gorilla.DefaultDialer.Dial(strings.Replace(server.URL, "http://", "ws://", 1), header)
	if err != nil {
		t.Fatalf("dial: %v response=%v", err, response)
	}
	defer connection.Close()
	ready := readProtocolMessage(t, connection)
	if ready.Type != "ready" || ready.ProtocolVersion != protocolVersion {
		t.Fatalf("ready=%+v", ready)
	}
	request := conversationRequest{Type: "conversation", RequestID: "request-1", UserID: "ha-user-1", DisplayName: "Alice", ConversationID: "conversation-1", Text: "hello"}
	if err := connection.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	content := readProtocolMessage(t, connection)
	result := readProtocolMessage(t, connection)
	if content.Type != "content" || content.RequestID != request.RequestID || content.Text != "Hello " {
		t.Fatalf("content=%+v", content)
	}
	if result.Type != "result" || result.RequestID != request.RequestID || result.Response != "Hello world" || result.Model != "test" {
		t.Fatalf("result=%+v", result)
	}

	processed := <-processor.requests
	if processed.Principal.Gateway != "homeassistant" || processed.Principal.ExternalID != request.UserID || !processed.Principal.Authenticated() {
		t.Fatalf("principal=%+v", processed.Principal)
	}
	if processed.SessionKey != "homeassistant:ha-user-1:conversation-1" || processed.Prompt != "hello" {
		t.Fatalf("request=%+v", processed)
	}
	owner, err := links.Resolve("homeassistant", request.UserID, true)
	if err != nil || owner.CanonicalUserID != processed.Principal.CanonicalUserID {
		t.Fatalf("incorrect profile ownership: %v", err)
	}
}

func TestGatewayRejectsAuthenticationBeforeProfileResolution(t *testing.T) {
	links, log := testLinks(t)
	gateway, err := New("8000", testToken, links, gatewayDependencies(log), log)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateway.handleConnection(w, r, nil)
	}))
	defer server.Close()

	_, response, err := gorilla.DefaultDialer.Dial(strings.Replace(server.URL, "http://", "ws://", 1), nil)
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err=%v response=%v", err, response)
	}
	if links.calls.Load() != 0 {
		t.Fatal("unauthenticated request resolved a profile")
	}
}

func TestGatewayRejectsInvalidRequestBeforeProfileResolution(t *testing.T) {
	links, log := testLinks(t)
	gateway, err := New("8000", testToken, links, gatewayDependencies(log), log)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateway.handleConnection(w, r, nil)
	}))
	defer server.Close()
	header := http.Header{"Authorization": []string{"Bearer " + testToken}}
	connection, _, err := gorilla.DefaultDialer.Dial(strings.Replace(server.URL, "http://", "ws://", 1), header)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = readProtocolMessage(t, connection)
	if err := connection.WriteMessage(gorilla.TextMessage, []byte(`{"type":"conversation","request_id":"request-1","user_id":"ha-user-1","conversation_id":"conversation-1","text":"hello","unknown":true}`)); err != nil {
		t.Fatal(err)
	}
	message := readProtocolMessage(t, connection)
	if message.Type != "error" || message.Code != "invalid_request" {
		t.Fatalf("message=%+v", message)
	}
	if links.calls.Load() != 0 {
		t.Fatal("invalid request resolved a profile")
	}
}

func readProtocolMessage(t *testing.T, connection *gorilla.Conn) protocolMessage {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var message protocolMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

type testProfileResolver struct {
	*profiles.Directory
	calls atomic.Int32
}

func (r *testProfileResolver) Resolve(platform, id string, direct bool) (identity.Principal, error) {
	r.calls.Add(1)
	return r.Directory.Resolve(platform, id, direct)
}

func testLinks(t *testing.T) (*testProfileResolver, *config.Logger) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	directory, err := profiles.NewDirectory(&config.Config{ProfileRoot: t.TempDir(), ProfileName: "default", HomeAssistantListenPort: "8000", ProfileRoutes: []config.ProfileRoute{{Platform: "homeassistant", UserID: "ha-user-1", Profile: "default"}}}, log)
	if err != nil {
		t.Fatal(err)
	}
	return &testProfileResolver{Directory: directory}, log
}

func gatewayDependencies(log *config.Logger) gatewayruntime.Dependencies {
	return gatewayruntime.Dependencies{Log: log}
}
