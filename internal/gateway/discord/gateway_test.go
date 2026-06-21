package discord

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

func TestDiscordHandleDirectMessageSendsReply(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()

	dg.handleMessage(discordMessage("msg-1", "channel-1", "", "123", "Alice", "hello discord"))

	if len(chat.requests) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(chat.requests))
	}
	last := chat.requests[0].Messages[len(chat.requests[0].Messages)-1]
	if last.Content != "hello discord" {
		t.Fatalf("unexpected prompt %q", last.Content)
	}
	if rest.lastMessageContent() != "discord response" {
		t.Fatalf("unexpected sent messages: %+v", rest.sentMessages())
	}
	if _, ok := dg.lookupReply("sent-1"); !ok {
		t.Fatal("expected sent bot reply remembered")
	}
}

func TestDiscordIgnoresUnmentionedGuildMessage(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()

	dg.handleMessage(discordMessage("msg-1", "channel-1", "guild-1", "123", "Alice", "hello guild"))

	if len(chat.requests) != 0 {
		t.Fatalf("expected no LLM request, got %d", len(chat.requests))
	}
	if len(rest.sentMessages()) != 0 {
		t.Fatalf("expected no sent messages, got %+v", rest.sentMessages())
	}
}

func TestDiscordMentionedGuildMessageStripsMentionAndResolvesMentions(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()

	msg := discordMessage("msg-1", "channel-1", "guild-1", "123", "Alice", "<@bot-1> hello <@456>")
	msg.Mentions = []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}{{ID: "456", Username: "Bob"}}
	dg.handleMessage(msg)

	if len(chat.requests) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(chat.requests))
	}
	prompt := chat.requests[0].Messages[len(chat.requests[0].Messages)-1].Content
	if prompt != "hello @Bob" {
		t.Fatalf("unexpected prompt %q", prompt)
	}
}

func TestDiscordReplyToBotIncludesReplyContext(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()
	dg.rememberReply("bot-msg", replyContext{SessionKey: "discord:channel-1:123", ChannelID: "channel-1", DisplayName: "Oswald", Text: "prior answer", IsFromBot: true, CreatedAt: time.Now()})

	msg := discordMessage("msg-2", "channel-1", "guild-1", "123", "Alice", "follow up")
	msg.ReferencedMessage = &struct {
		ID          string       `json:"id"`
		Content     string       `json:"content"`
		Attachments []Attachment `json:"attachments,omitempty"`
		Embeds      []Embed      `json:"embeds,omitempty"`
		Author      struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"author"`
	}{ID: "bot-msg"}
	msg.ReferencedMessage.Author.ID = "bot-1"
	msg.ReferencedMessage.Author.Username = "Oswald"
	dg.handleMessage(msg)

	if len(chat.requests) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(chat.requests))
	}
	prompt := chat.requests[0].Messages[len(chat.requests[0].Messages)-1].Content
	if !strings.Contains(prompt, "[Replying to Oswald: \"prior answer\"]") || !strings.Contains(prompt, "follow up") {
		t.Fatalf("unexpected prompt %q", prompt)
	}
}

func TestDiscordHelpers(t *testing.T) {
	chunks := splitMessage("one. two three\nfour", 10)
	if len(chunks) != 3 || chunks[0] != "one." || chunks[1] != "two three" || chunks[2] != "four" {
		t.Fatalf("unexpected chunks: %+v", chunks)
	}
	mentions := []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}{{ID: "123", Username: "Alice"}}
	if got := resolveMentions("hi <@123> and <@!999>", mentions); got != "hi @Alice and <@!999>" {
		t.Fatalf("unexpected resolved mentions %q", got)
	}
	if resolveGatewayURL("gateway.discord.gg") != "wss://gateway.discord.gg/?v=10&encoding=json" {
		t.Fatal("unexpected resolved gateway URL")
	}
}

type discordFakeChatter struct {
	mu       sync.Mutex
	requests []llm.ChatRequest
}

func (f *discordFakeChatter) Chat(_ context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "discord response"}}, nil
}

type fakeDiscordREST struct {
	server *httptest.Server
	mu     sync.Mutex
	sent   []map[string]interface{}
}

func newFakeDiscordREST(t *testing.T) *fakeDiscordREST {
	t.Helper()
	rest := &fakeDiscordREST{}
	rest.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode discord message payload: %v", err)
			}
			rest.mu.Lock()
			rest.sent = append(rest.sent, payload)
			rest.mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"sent-1"}`))
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/messages/") {
			_, _ = w.Write([]byte(`{"id":"fetched","content":"fetched reply","author":{"id":"bot-1","username":"Oswald"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	return rest
}

func (r *fakeDiscordREST) sentMessages() []map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]interface{}(nil), r.sent...)
}

func (r *fakeDiscordREST) lastMessageContent() string {
	sent := r.sentMessages()
	if len(sent) == 0 {
		return ""
	}
	content, _ := sent[len(sent)-1]["content"].(string)
	return content
}

func newDiscordTestGateway(t *testing.T, apiBaseURL string) (*Gateway, *broker.Broker, *discordFakeChatter) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	links := accountlinking.NewService(filepath.Join(dir, "oswald.db"), memories, log)
	soulStore := soul.NewStore(filepath.Join(dir, "soul.md"), log)
	if err := soulStore.Write("You are Oswald."); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	chat := &discordFakeChatter{}
	ai := agent.NewAgent(chat, nil, registry.New(log), "test-model", "", soulStore, memories, memory.ContextBudget{PromptLimit: 100000}, 3, time.Minute, memory.NewStore(memory.Options{}, log), log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	dg := &Gateway{
		Token:      "token",
		BotID:      "bot-1",
		Broker:     b,
		Links:      links,
		Commands:   commands.NewRouter(),
		Log:        log,
		APIBaseURL: apiBaseURL,
		replyIndex: make(map[string]replyContext),
		hbAcked:    true,
	}
	return dg, b, chat
}

func discordMessage(id, channelID, guildID, authorID, username, content string) MessageCreate {
	var msg MessageCreate
	msg.ID = id
	msg.ChannelID = channelID
	msg.GuildID = guildID
	msg.Content = content
	msg.Author.ID = authorID
	msg.Author.Username = username
	return msg
}
