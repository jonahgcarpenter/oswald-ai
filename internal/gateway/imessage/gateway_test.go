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
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
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
	server *httptest.Server
	mu     sync.Mutex
	sent   []sendTextRequest
}

func newFakeBlueBubbles(t *testing.T) *fakeBlueBubbles {
	t.Helper()
	bb := &fakeBlueBubbles{}
	bb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
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
	ai := agent.NewAgent(chat, nil, registry.New(log), "test-model", "", soulStore, memories, memory.ContextBudget{PromptLimit: 100000}, 3, time.Minute, memory.NewStore(memory.Options{}, log), log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	g := &Gateway{
		BlueBubblesURL:      blueBubblesURL,
		BlueBubblesPassword: "pw",
		Links:               links,
		Commands:            commands.NewRouter(),
		Log:                 log,
		Broker:              b,
		messageIndex:        make(map[string]messageContext),
		contactNames:        make(map[string]contactNameCacheEntry),
	}
	return g, b, chat
}
