package imessage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestIMessageProcessDirectMessageSendsReply(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "hello imessage",
		Handle: messageHandle{Address: "+1 (555) 123-4567"},
		Chats:  []messageChat{{GUID: "chat-direct", Style: chatStyleDirect}},
	})

	primary := primaryIMessageRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	last := primary[0].Messages[len(primary[0].Messages)-1]
	if last.Content != "hello imessage" {
		t.Fatalf("unexpected prompt %q", last.Content)
	}
	if bb.sentMessage() != "imessage response" {
		t.Fatalf("unexpected sent messages: %+v", bb.sentMessages())
	}
	if _, ok := g.lookupMessage("sent-1"); !ok {
		t.Fatal("expected sent bot message remembered")
	}
}

func TestIMessageIgnoresUnmentionedGroupMessage(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "hello group",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
	})

	if len(primaryIMessageRequests(chat.requests)) != 0 {
		t.Fatalf("expected no LLM request, got %d", len(primaryIMessageRequests(chat.requests)))
	}
	if len(bb.sentMessages()) != 0 {
		t.Fatalf("expected no sent messages, got %+v", bb.sentMessages())
	}
}

func TestIMessageReplyToBotIncludesReplyContext(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, chat := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()
	g.rememberBotMessage("bot-msg", "imessage:chat;+;group:+15551234567", "chat;+;group", "+15551234567", "prior bot answer")

	g.processIncomingMessage(webhookMessage{
		GUID:        "msg-2",
		Text:        "follow up",
		ReplyToGUID: "bot-msg",
		Handle:      messageHandle{Address: "+15551234567"},
		Chats:       []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
	})

	primary := primaryIMessageRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	prompt := primary[0].Messages[len(primary[0].Messages)-1].Content
	if !strings.Contains(prompt, "[Replying to Oswald: \"prior bot answer\"]") || !strings.Contains(prompt, "follow up") {
		t.Fatalf("unexpected prompt %q", prompt)
	}
}

func TestIMessageAcceptedMessageStartsTypingAndMarksRead(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "@Oswald hello",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
	})

	if !bb.waitForPath("/api/v1/chat/chat%3B+%3Bgroup/typing") {
		t.Fatalf("expected escaped typing request, got paths %+v", bb.paths())
	}
	if !bb.waitForPath("/api/v1/chat/chat%3B+%3Bgroup/read") {
		t.Fatalf("expected escaped read receipt request, got paths %+v", bb.paths())
	}
}

func TestIMessageReadReceiptNotSentForIgnoredGroupMessage(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "hello group",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
	})

	if bb.waitForPath("/api/v1/chat/chat%3B+%3Bgroup/read") {
		t.Fatalf("expected no read receipt for ignored message, got paths %+v", bb.paths())
	}
}

func TestIMessageTypingAndReadRequirePrivateAPIHelper(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	bb.setCapabilities(false, true)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "hello imessage",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{{GUID: "chat-direct", Style: chatStyleDirect}},
	})

	if bb.waitForPath("/api/v1/chat/chat-direct/typing") {
		t.Fatalf("expected no typing request without private api, got paths %+v", bb.paths())
	}
	if bb.waitForPath("/api/v1/chat/chat-direct/read") {
		t.Fatalf("expected no read receipt without private api, got paths %+v", bb.paths())
	}
}

func TestIMessageCapabilityRetryStopsWhenHelperReady(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	bb.setCapabilities(true, false)
	bb.setHelperReadyAfter(3)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	if !g.refreshBlueBubblesCapabilitiesWithRetry(5, 0) {
		t.Fatal("expected capabilities to become available")
	}
	if attempts := bb.serverInfoRequests(); attempts != 3 {
		t.Fatalf("expected 3 server info attempts, got %d", attempts)
	}
}

func TestIMessageCapabilityRetryStopsAfterLimit(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	bb.setCapabilities(true, false)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	if g.refreshBlueBubblesCapabilitiesWithRetry(5, 0) {
		t.Fatal("expected capabilities to remain unavailable")
	}
	if attempts := bb.serverInfoRequests(); attempts != 5 {
		t.Fatalf("expected 5 server info attempts, got %d", attempts)
	}
}

func TestIMessageEndpointBuilders(t *testing.T) {
	endpoint, err := buildBlueBubblesMessageEndpoint("http://bb/base/", "a/b", "pw")
	if err != nil {
		t.Fatalf("message endpoint: %v", err)
	}
	if !strings.Contains(endpoint, "/base/api/v1/message/a%252Fb") || !strings.Contains(endpoint, "guid=pw") || !strings.Contains(endpoint, "with=") {
		t.Fatalf("unexpected message endpoint %q", endpoint)
	}
	attachment, err := buildBlueBubblesAttachmentEndpoint("http://bb", "att-1", "pw")
	if err != nil {
		t.Fatalf("attachment endpoint: %v", err)
	}
	if !strings.Contains(attachment, "/api/v1/attachment/att-1/download") || !strings.Contains(attachment, "original=true") {
		t.Fatalf("unexpected attachment endpoint %q", attachment)
	}
	chatAction, err := buildBlueBubblesChatActionEndpoint("http://bb/base/", "chat;+;group", "typing", "pw")
	if err != nil {
		t.Fatalf("chat action endpoint: %v", err)
	}
	if !strings.Contains(chatAction, "/base/api/v1/chat/chat%3B+%3Bgroup/typing") || !strings.Contains(chatAction, "guid=pw") {
		t.Fatalf("unexpected chat action endpoint %q", chatAction)
	}
}

type imFakeChatter struct {
	mu       sync.Mutex
	requests []llm.ChatRequest
}

func (f *imFakeChatter) Chat(_ context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if req.Format == "json_object" {
		return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: `{"session_updates":{"summary":"","open_threads":[],"decisions":[],"user_goals":[]},"memory_candidates":[]}`}}, nil
	}
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "imessage response"}}, nil
}

func primaryIMessageRequests(requests []llm.ChatRequest) []llm.ChatRequest {
	out := make([]llm.ChatRequest, 0, len(requests))
	for _, req := range requests {
		if req.Format != "json_object" {
			out = append(out, req)
		}
	}
	return out
}

type fakeBlueBubbles struct {
	server           *httptest.Server
	mu               sync.Mutex
	sent             []sendTextRequest
	seenPaths        []string
	privateAPI       bool
	helperConnected  bool
	serverInfoCount  int
	helperReadyAfter int
}

func newFakeBlueBubbles(t *testing.T) *fakeBlueBubbles {
	t.Helper()
	bb := &fakeBlueBubbles{}
	bb.privateAPI = true
	bb.helperConnected = true
	bb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		bb.mu.Lock()
		bb.seenPaths = append(bb.seenPaths, r.URL.EscapedPath())
		privateAPI := bb.privateAPI
		helperConnected := bb.helperConnected
		if r.URL.Path == "/api/v1/server/info" {
			bb.serverInfoCount++
			if bb.helperReadyAfter > 0 && bb.serverInfoCount >= bb.helperReadyAfter {
				bb.helperConnected = true
				helperConnected = true
			}
		}
		bb.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/server/info":
			_ = json.NewEncoder(w).Encode(serverInfoResponse{Data: struct {
				PrivateAPI      bool `json:"private_api"`
				HelperConnected bool `json:"helper_connected"`
			}{PrivateAPI: privateAPI, HelperConnected: helperConnected}})
		case r.URL.Path == "/api/v1/contact/query":
			_, _ = w.Write([]byte(`{"data":[{"displayName":"Alice"}]}`))
		case strings.Contains(r.URL.Path, "/typing"):
			_, _ = w.Write([]byte(`{"data":{"guid":"typing"}}`))
		case r.URL.Path == "/api/v1/message/text":
			var req sendTextRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode send request: %v", err)
			}
			bb.mu.Lock()
			bb.sent = append(bb.sent, req)
			bb.mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"guid":"sent-1"}}`))
		case r.URL.Path == "/api/v1/message/query":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"guid":"ok"}}`))
		}
	}))
	return bb
}

func (bb *fakeBlueBubbles) setCapabilities(privateAPI, helperConnected bool) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	bb.privateAPI = privateAPI
	bb.helperConnected = helperConnected
}

func (bb *fakeBlueBubbles) setHelperReadyAfter(attempt int) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	bb.helperReadyAfter = attempt
}

func (bb *fakeBlueBubbles) serverInfoRequests() int {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	return bb.serverInfoCount
}

func (bb *fakeBlueBubbles) paths() []string {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	return append([]string(nil), bb.seenPaths...)
}

func (bb *fakeBlueBubbles) waitForPath(path string) bool {
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, seen := range bb.paths() {
			if seen == path {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func (bb *fakeBlueBubbles) sentMessages() []sendTextRequest {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	return append([]sendTextRequest(nil), bb.sent...)
}

func (bb *fakeBlueBubbles) sentMessage() string {
	sent := bb.sentMessages()
	if len(sent) == 0 {
		return ""
	}
	return sent[len(sent)-1].Message
}

func newIMessageTestGateway(t *testing.T, blueBubblesURL string) (*Gateway, *broker.Broker, *imFakeChatter) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	links := accountlinking.NewService(filepath.Join(dir, "oswald.db"), memories, log)
	soulStore := soul.NewStore(filepath.Join(dir, "soul.md"), log)
	if err := soulStore.Write("You are Oswald."); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	chat := &imFakeChatter{}
	ai := agent.NewAgent(chat, registry.New(log), "test-model", soulStore, memories, promptbudget.ContextBudget{PromptLimit: 100000}, 3, time.Minute, log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	commandService, err := commands.NewService()
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	g := &Gateway{
		BlueBubblesURL:      blueBubblesURL,
		BlueBubblesPassword: "pw",
		Links:               links,
		Runtime:             gatewayruntime.Dependencies{Commands: commandService, Log: log},
		Log:                 log,
		Broker:              b,
		messageIndex:        make(map[string]messageContext),
		contactNames:        make(map[string]contactNameCacheEntry),
	}
	return g, b, chat
}
