package imessage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway/routing"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway/runtime"
)

// TestIssue258SelfAuthoredPredecessorAdmitted pins the dedicated-Apple-ID
// contract that superseded the original issue-258 report: a threaded reply
// whose predecessor is a clean self-authored (isFromMe) image is admitted
// even with no outbound-cache entry and no live turn, because no other
// device shares the bot's Apple ID. Shared-ID deployments are unsupported.
func TestIssue258SelfAuthoredPredecessorAdmitted(t *testing.T) {
	root := replyFixture("root", 1, false)
	image := replyFixture("image", 2, true)
	image.Text = ""
	image.Attachments = []attachment{{GUID: "image-attachment", MimeType: "image/png", TransferName: "image.png"}}
	anchor := replyFixture("incoming", 10, false)
	image.ThreadOriginatorGUID, anchor.ThreadOriginatorGUID = "root", "root"
	image.ThreadOriginatorPart, anchor.ThreadOriginatorPart = "0:0", "0:0"
	dates := map[string]int64{"root": 100, "image": 200, "incoming": 300}

	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	bb.mu.Lock()
	bb.messageLookup = sqliteReplyLookup(t, []messageLookupData{root, image, anchor}, dates)
	bb.mu.Unlock()
	g, b, model := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	// No outbound-cache entry for the image and no live turn: under the
	// dedicated Apple ID the clean self-authored record is authoritative,
	// so the reply is admitted via the BlueBubbles lookup alone.
	g.processIncomingMessage(threadIncoming())
	if len(model.primaryRequests()) != 1 {
		t.Fatalf("self-authored predecessor was not admitted, requests=%d", len(model.primaryRequests()))
	}
}

// TestIssue258CompletedBotReplyStillAdmitted proves a threaded reply whose
// bot predecessor was sent by a finished turn is still admitted, via the
// fast-path cache or — past TTL, eviction, or restart — the authoritative
// BlueBubbles record.
func TestIssue258CompletedBotReplyStillAdmitted(t *testing.T) {
	root, bot, anchor := replyFixture("root", 1, false), replyFixture("bot", 2, true), replyFixture("incoming", 10, false)
	bot.ThreadOriginatorGUID, anchor.ThreadOriginatorGUID = "root", "root"
	bot.ThreadOriginatorPart, anchor.ThreadOriginatorPart = "0:0", "0:0"
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	bb.mu.Lock()
	bb.messageLookup = sqliteReplyLookup(t, []messageLookupData{root, bot, anchor}, map[string]int64{"root": 100, "bot": 200, "incoming": 300})
	bb.mu.Unlock()
	g, b, model := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	g.rememberBotMessage(bot.GUID, "unused", root.Chats[0].GUID, "imessage:self", bot.Text)
	g.processIncomingMessage(threadIncoming())
	if len(model.primaryRequests()) != 1 {
		t.Fatalf("cached bot predecessor was not admitted, requests=%d", len(model.primaryRequests()))
	}
}

// TestIssue258MentionBypassesReplyTarget proves a real mention still invokes
// regardless of the reply target, even when the predecessor is human and no
// turn is live.
func TestIssue258MentionBypassesReplyTarget(t *testing.T) {
	root, human, anchor := replyFixture("root", 1, false), replyFixture("human", 2, false), replyFixture("incoming", 10, false)
	human.ThreadOriginatorGUID, anchor.ThreadOriginatorGUID = "root", "root"
	human.ThreadOriginatorPart, anchor.ThreadOriginatorPart = "0:0", "0:0"
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	bb.mu.Lock()
	bb.messageLookup = sqliteReplyLookup(t, []messageLookupData{root, human, anchor}, map[string]int64{"root": 100, "human": 200, "incoming": 300})
	bb.mu.Unlock()
	g, b, model := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	msg := threadIncoming()
	msg.Text = "@Oswald please help"
	g.processIncomingMessage(msg)
	if len(model.primaryRequests()) != 1 {
		t.Fatalf("mention-bearing follow-up was not admitted, requests=%d", len(model.primaryRequests()))
	}
}

// blockingChatter holds the first agent call until released so a threaded
// follow-up can arrive while the invoking turn is live.
type blockingChatter struct {
	mu       sync.Mutex
	requests []llm.ChatRequest
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (f *blockingChatter) Chat(_ context.Context, req llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	f.once.Do(func() { close(f.started) })
	select {
	case <-f.release:
	case <-time.After(10 * time.Second):
	}
	if req.Format == "json_object" {
		return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: `{"session_updates":{"summary":"","open_threads":[],"decisions":[],"user_goals":[]},"memory_candidates":[]}`}}, nil
	}
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "live response"}}, nil
}

func (f *blockingChatter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if req.Format != "json_object" {
			n++
		}
	}
	return n
}

// routingPreflightForTest mirrors the group-thread preflight inputs for an
// unmentioned follow-up: only a verified reply-to-bot may admit it.
func routingPreflightForTest(isReplyToBot bool) string {
	d := routing.Preflight(routing.PreflightInput{IsGroup: true, IsMention: false, IsReplyToBot: isReplyToBot, Text: "actually do it faster"})
	return string(d.Action)
}

// TestIssue258LiveTurnSteerAdmitted proves a threaded reply whose target is
// the in-flight invoking message is admitted as a steer even though the
// predecessor is not a bot message and the reply lookup fails.
func TestIssue258LiveTurnSteerAdmitted(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	// Force the reply lookup to fail so admission can only come from the
	// broker in-flight set, never from predecessor authorship.
	bb.mu.Lock()
	bb.messageLookup = sqliteReplyLookup(t, nil, nil)
	bb.mu.Unlock()

	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	memories, err := memory.NewProfileStore(context.Background(), dir, "default", log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memories.Close() })
	links := imessageProfileDirectory(t, dir, log)
	soulPath := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(soulPath, []byte("You are Oswald."), 0o600); err != nil {
		t.Fatal(err)
	}
	soulStore := soul.NewProfileStore(dir, "default", soulPath)
	chat := &blockingChatter{started: make(chan struct{}), release: make(chan struct{})}
	ai := agent.NewAgent(chat, registry.New(log), "test-model", "test-provider", soulStore, memories, budget.ContextBudget{PromptLimit: 100000}, governance.GlobalPolicy{MaxExecutions: 12, MaxToolIterations: 8}, log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	defer b.Shutdown()
	commandService, err := commands.NewServiceWithCommands()
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{
		BlueBubblesURL:      bb.server.URL,
		BlueBubblesPassword: "pw",
		Links:               links,
		Runtime:             gatewayruntime.Dependencies{Commands: commandService, Log: log},
		Log:                 log,
		Broker:              b,
		messageIndex:        make(map[string]messageContext),
		contactNames:        make(map[string]contactNameCacheEntry),
		coalesceQuiet:       20 * time.Millisecond,
		coalesceMaxHold:     100 * time.Millisecond,
	}

	chatGUID := "private-chat;+;group"
	sender := messageHandle{Address: "+15551234567"}
	hostDone := make(chan struct{})
	go func() {
		defer close(hostDone)
		g.processIncomingMessage(webhookMessage{
			GUID:   "live-root",
			Text:   "@Oswald do the thing",
			Handle: sender,
			Chats:  []messageChat{{GUID: chatGUID, Style: chatStyleGroup}},
		})
	}()
	select {
	case <-chat.started:
	case <-time.After(5 * time.Second):
		t.Fatal("live turn never started")
	}
	if !b.IsInflight(chatGUID, "live-root") {
		t.Fatal("live turn was not tracked as in-flight")
	}
	// Decide admission for the threaded follow-up exactly as inbound does:
	// resolve the reply, then fall back to the broker in-flight set. The
	// lookup is forced to fail, so only the live-turn match can admit.
	followMsg := webhookMessage{
		GUID:                 "live-follow",
		Text:                 "actually do it faster",
		ThreadOriginatorGUID: "live-root",
		Handle:               sender,
		Chats:                []messageChat{{GUID: chatGUID, Style: chatStyleGroup}},
	}
	resolved, found := g.resolveReply(context.Background(), followMsg, true, "req-test")
	isReplyToBot := found && resolved.IsFromBot
	if !isReplyToBot && b.IsInflight(chatGUID, followMsg.replyTargetGUID()) {
		isReplyToBot = true
	}
	if !isReplyToBot {
		t.Fatal("threaded reply to the live turn was not admitted via the in-flight set")
	}
	// The same reply with no live turn must stay ignored.
	if b.IsInflight(chatGUID, "missing-guid") {
		t.Fatal("in-flight lookup matched an unknown message")
	}
	decision := routingPreflightForTest(isReplyToBot)
	if decision == "ignore" {
		t.Fatal("live-turn steer was rejected by preflight")
	}
	close(chat.release)
	select {
	case <-hostDone:
	case <-time.After(10 * time.Second):
		t.Fatal("live turn never finished")
	}
	if chat.count() < 1 {
		t.Fatalf("live turn fired %d model calls, want at least 1", chat.count())
	}
	sent := bb.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("live turn sent %d messages, want 1", len(sent))
	}
	// The live registration must be gone once the turn settles.
	deadline := time.Now().Add(2 * time.Second)
	for b.IsInflight(chatGUID, "live-root") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.IsInflight(chatGUID, "live-root") {
		t.Fatal("live turn was not released after settlement")
	}
	if !strings.Contains(sent[0].Message, "live response") {
		t.Fatalf("unexpected sent message: %+v", sent[0])
	}
}
