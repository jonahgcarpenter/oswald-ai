package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

type fakeChatter struct {
	mu        sync.Mutex
	responses []*llm.ChatResponse
	calls     int
	err       error
	wait      chan struct{}
}

func (f *fakeChatter) Chat(ctx context.Context, _ llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	if f.wait != nil {
		select {
		case <-f.wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.responses) == 0 {
		return &llm.ChatResponse{Model: "fake", Message: llm.ChatMessage{Role: "assistant", Content: "synthetic done"}}, nil
	}
	return f.responses[0], nil
}

type fakeBroker struct {
	mu   sync.Mutex
	sent []*broker.Request
	fail bool
	busy bool
}

func (f *fakeBroker) Submit(req *broker.Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errFakeBrokerDown
	}
	f.sent = append(f.sent, req)
	if req.ResponseChan != nil {
		select {
		case req.ResponseChan <- broker.Result{ExecutionComplete: true}:
		default:
		}
	}
	return nil
}

func (f *fakeBroker) LaneBusy(_ identity.Principal, _ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy
}

func (f *fakeBroker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

var errFakeBrokerDown = &fakeError{"fake broker down"}

type fakeProcs struct{}

func (fakeProcs) List() ([]process.Info, error)         { return nil, nil }
func (fakeProcs) Kill(id string) (process.Info, error)  { return process.Info{}, nil }
func (fakeProcs) Tail(id string, n int) (string, error) { return "", nil }

func testPrincipal() identity.Principal {
	return identity.Principal{
		CanonicalUserID: "alice",
		Gateway:         "openai",
		ExternalID:      identity.LocalOpenAIIdentifier,
		Assurance:       identity.AssuranceLocalLoopback,
	}
}

func testManager(t *testing.T, chat *fakeChatter, br *fakeBroker) (*Manager, *memory.ProfileStore, func()) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SOUL.md"), []byte("synthetic soul"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := memory.NewProfileStore(context.Background(), root, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		LLMGatewayModel:    "fake/model",
		LLMGatewayProvider: "fake",
		LLMGatewayURL:      "http://localhost:9999/v1",
		LLMGatewayAPIKey:   "synthetic-key",
	}
	log := config.NewLogger(config.LevelError)
	m := NewManager(Deps{
		Profile:     "alice",
		ProfileRoot: root,
		Log:         log,
		Store:       store,
		Broker:      br,
		Processes:   fakeProcs{},
		Parent:      chat,
		NewClient: func(url, key, virtualKey string, log *config.Logger) llm.Chatter {
			return chat
		},
		Registry: registry.New(log),
		Soul:     soul.NewProfileStore(root, "alice", ""),
		Window:   32768,
		Config:   cfg,
	})
	m.pool = make(chan struct{}, 10)
	return m, store, func() { store.Close() }
}

func dispatchContext(p identity.Principal, session string) context.Context {
	ctx := context.Background()
	ctx = requestctx.WithPrincipal(ctx, p)
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req-test", SessionID: session})
	return ctx
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestSyncDispatchReturnsResult(t *testing.T) {
	chat := &fakeChatter{}
	br := &fakeBroker{}
	m, store, cleanup := testManager(t, chat, br)
	defer cleanup()

	result, err := m.Execute(dispatchContext(testPrincipal(), "discord:dm:1"), map[string]interface{}{
		"goal": "Summarize synthetic topic", "context": "nothing else needed",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(result.Content, "synthetic done") {
		t.Fatalf("unexpected sync result: %q", result.Content)
	}
	if got := br.count(); got != 0 {
		t.Fatalf("sync dispatch delivered %d turns, want 0", got)
	}
	due, err := store.ListDueDeliveries(context.Background(), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("sync dispatch wrote ledger rows: %+v %v", due, err)
	}
	if n := len(m.liveForSession("alice", "discord:dm:1")); n != 0 {
		t.Fatalf("sync unit still live: %d", n)
	}
}

func TestBackgroundDispatchDeliversResult(t *testing.T) {
	chat := &fakeChatter{}
	br := &fakeBroker{}
	m, store, cleanup := testManager(t, chat, br)
	defer cleanup()

	result, err := m.Execute(dispatchContext(testPrincipal(), "discord:dm:1"), map[string]interface{}{
		"goal": "Summarize synthetic topic", "context": "nothing else needed",
		"background": true,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(result.Content, "running in the background") {
		t.Fatalf("unexpected dispatch receipt: %q", result.Content)
	}
	waitFor(t, 10*time.Second, func() bool {
		due, err := store.ListDueDeliveries(context.Background(), 10)
		return err == nil && len(due) == 1
	}, "unit never reached terminal state")

	m.deliverDue(context.Background())
	if got := br.count(); got != 1 {
		t.Fatalf("broker submissions = %d, want 1", got)
	}
	if !strings.Contains(br.sent[0].Prompt, "synthetic done") {
		t.Fatalf("delivery missing summary: %q", br.sent[0].Prompt)
	}
	if br.sent[0].SessionKey != "discord:dm:1" {
		t.Fatalf("delivery wrong session: %q", br.sent[0].SessionKey)
	}
	due, err := store.ListDueDeliveries(context.Background(), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("row not marked delivered: %+v %v", due, err)
	}
}

func TestSyncBatchReturnsConsolidatedResult(t *testing.T) {
	chat := &fakeChatter{}
	br := &fakeBroker{}
	m, _, cleanup := testManager(t, chat, br)
	defer cleanup()

	result, err := m.Execute(dispatchContext(testPrincipal(), "discord:dm:4"), map[string]interface{}{
		"tasks": []interface{}{
			map[string]interface{}{"goal": "First synthetic task"},
			map[string]interface{}{"goal": "Second synthetic task"},
		},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(result.Content, "TASK 1/2") || !strings.Contains(result.Content, "TASK 2/2") {
		t.Fatalf("batch missing TASK labels: %q", result.Content)
	}
	if got := br.count(); got != 0 {
		t.Fatalf("sync batch delivered %d turns, want 0", got)
	}
}

func TestBackgroundDefersWhileLaneBusy(t *testing.T) {
	chat := &fakeChatter{}
	br := &fakeBroker{busy: true}
	m, store, cleanup := testManager(t, chat, br)
	defer cleanup()

	ctx := dispatchContext(testPrincipal(), "discord:dm:5")
	ctx = requestctx.WithTransportRouting(ctx, requestctx.TransportRouting{ChatID: "chan-5", TransportProfile: "default"})
	if _, err := m.Execute(ctx, map[string]interface{}{
		"goal": "Background routing task", "background": true,
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		due, err := store.ListDueDeliveries(context.Background(), 10)
		return err == nil && len(due) == 1
	}, "unit never reached terminal state")

	m.deliverDue(context.Background())
	if got := br.count(); got != 0 {
		t.Fatalf("busy lane delivered %d turns, want 0", got)
	}
	br.mu.Lock()
	br.busy = false
	br.mu.Unlock()
	m.deliverDue(context.Background())
	if got := br.count(); got != 1 {
		t.Fatalf("broker submissions = %d, want 1", got)
	}
	if br.sent[0].ChatID != "chan-5" {
		t.Fatalf("delivery missing chat routing: %q", br.sent[0].ChatID)
	}
	if br.sent[0].SessionKey != "discord:dm:5" {
		t.Fatalf("delivery wrong session: %q", br.sent[0].SessionKey)
	}
}

func TestSteerStopList(t *testing.T) {
	chat := &fakeChatter{wait: make(chan struct{})}
	br := &fakeBroker{}
	m, _, cleanup := testManager(t, chat, br)
	defer cleanup()
	defer close(chat.wait)
	p := testPrincipal()
	ctx := dispatchContext(p, "discord:dm:2")

	if _, err := m.Execute(ctx, map[string]interface{}{"goal": "slow synthetic task", "background": true}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return len(m.liveForSession("alice", "discord:dm:2")) == 1 }, "child never registered")
	children := m.liveForSession("alice", "discord:dm:2")
	id := children[0].id

	listed, err := m.Execute(ctx, map[string]interface{}{"action": "list"})
	if err != nil || !strings.Contains(listed.Content, id) {
		t.Fatalf("list = %+v %v", listed, err)
	}
	if _, err := m.Execute(ctx, map[string]interface{}{"action": "steer", "subagent_id": id, "message": "go left"}); err != nil {
		t.Fatalf("steer: %v", err)
	}
	if _, err := m.Execute(ctx, map[string]interface{}{"action": "steer", "subagent_id": "nope", "message": "x"}); err == nil {
		t.Fatal("steer of unknown id accepted")
	}
	if _, err := m.Execute(ctx, map[string]interface{}{"action": "stop", "subagent_id": id}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return len(m.liveForSession("alice", "discord:dm:2")) == 0 }, "child never finished after stop")
	if _, err := m.Execute(ctx, map[string]interface{}{"action": "stop", "subagent_id": id}); err == nil {
		t.Fatal("second stop of finished id accepted")
	}
}

func TestCancelSessionDropsDelivery(t *testing.T) {
	chat := &fakeChatter{wait: make(chan struct{})}
	br := &fakeBroker{}
	m, store, cleanup := testManager(t, chat, br)
	defer cleanup()
	defer close(chat.wait)

	if _, err := m.Execute(dispatchContext(testPrincipal(), "discord:dm:3"), map[string]interface{}{"goal": "cancelled task", "background": true}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return len(m.liveForSession("alice", "discord:dm:3")) == 1 }, "child never registered")
	if n := m.CancelSession("alice", "discord:dm:3"); n != 1 {
		t.Fatalf("cancelled = %d, want 1", n)
	}
	waitFor(t, 10*time.Second, func() bool {
		due, err := store.ListDueDeliveries(context.Background(), 10)
		return err == nil && len(due) == 0
	}, "cancelled unit still awaiting delivery")
	m.deliverDue(context.Background())
	if got := br.count(); got != 0 {
		t.Fatalf("cancelled unit delivered %d turns", got)
	}
}

type fakeSender struct {
	mu      sync.Mutex
	calls   int
	profile string
	session string
	text    string
}

func (f *fakeSender) Deliver(_ context.Context, profile, sessionKey, text string, _ []media.OutputAttachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.profile, f.session, f.text = profile, sessionKey, text
	return nil
}

func (f *fakeSender) DeliverMention(_ context.Context, profile, sessionKey string, _ cron.Mention, text string, _ []media.OutputAttachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.profile, f.session, f.text = profile, sessionKey, text
	return nil
}

var _ cron.Sender = (*fakeSender)(nil)

func TestBackgroundDeliverySendsAndMarksDelivered(t *testing.T) {
	chat := &fakeChatter{}
	br := &fakeBroker{}
	m, store, cleanup := testManager(t, chat, br)
	defer cleanup()
	sender := &fakeSender{}
	m.SetSenders(map[string]cron.Sender{"discord": sender})

	sess, err := store.ResolveSessionContext(context.Background(), "alice", "discord:dm:6", 24*time.Hour)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	turn, err := store.AppendPendingSessionTurn(context.Background(), memory.SessionTurnWrite{
		SessionID: "discord:dm:6", UserID: "alice", Generation: sess.Generation,
		UserText: "synthetic", AssistantText: "pending follow-up",
		Pressure: memory.SessionPromptPressure{Tokens: 10, Limit: 100, Version: "synthetic-v1"},
	})
	if err != nil {
		t.Fatalf("append turn: %v", err)
	}
	env := dispatchEnvelope{
		UnitID: "unit-1", SessionKey: "discord:dm:6",
		Principal: storedPrincipal{UserID: "alice", Gateway: "discord", ExternalID: "ext", Assurance: string(identity.AssuranceLocalLoopback)},
	}
	respCh := make(chan broker.Result, 1)
	respCh <- broker.Result{Response: &agent.Response{Response: "hello background", SourceTurnID: turn.ID}, ExecutionComplete: true}
	m.watchDelivery("unit-1", env, respCh)
	waitFor(t, 10*time.Second, func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return sender.calls == 1
	}, "sender never called")
	if sender.text != "hello background" || sender.session != "discord:dm:6" {
		t.Fatalf("sender got %q %q", sender.session, sender.text)
	}
	waitFor(t, 10*time.Second, func() bool {
		turns, err := store.RecentCompletedExchangesAfter(context.Background(), "alice", "discord:dm:6", sess.Generation, 0, 10)
		return err == nil && len(turns) == 1
	}, "session turn never marked delivered")
}

func TestOutputSchemaValidation(t *testing.T) {
	if ok, _ := validateOutput(`{"a":1}`, map[string]interface{}{"type": "object"}); !ok {
		t.Fatal("valid object rejected")
	}
	if ok, _ := validateOutput("```json\n{\"a\":1}\n```", map[string]interface{}{}); !ok {
		t.Fatal("fenced object rejected")
	}
	if ok, _ := validateOutput("[1,2]", map[string]interface{}{}); ok {
		t.Fatal("array accepted as object")
	}
	if ok, errs := validateOutput("not json", map[string]interface{}{}); ok || len(errs) == 0 {
		t.Fatal("prose accepted")
	}
}

func TestChildBlockedTools(t *testing.T) {
	kept := childBlockedTools(true)
	for _, name := range kept {
		if name == "delegate" {
			t.Fatal("delegate not withheld-capable set still contains delegate")
		}
	}
	hidden := childBlockedTools(false)
	found := false
	for _, name := range hidden {
		if name == "delegate" {
			found = true
		}
	}
	if !found {
		t.Fatal("default child catalog exposes delegate")
	}
	for _, name := range []string{"clarify", "memory", "session_search", "cronjob_manage", "image_generate"} {
		seen := false
		for _, have := range hidden {
			if have == name {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("child catalog exposes %q", name)
		}
	}
}
