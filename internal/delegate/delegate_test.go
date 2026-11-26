package delegate

import (
	"context"
	"encoding/json"
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
	requests  []llm.ChatRequest
	calls     int
	err       error
	wait      chan struct{}
}

func (f *fakeChatter) Chat(ctx context.Context, req llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
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
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.responses) == 0 {
		return &llm.ChatResponse{Model: "fake", Message: llm.ChatMessage{Role: "assistant", Content: "synthetic done"}}, nil
	}
	return f.responses[0], nil
}

func (f *fakeChatter) captured() []llm.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.ChatRequest(nil), f.requests...)
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
	cfg := &config.Config{
		LLMGatewayModel:    "fake/model",
		LLMGatewayProvider: "fake",
		LLMGatewayURL:      "http://localhost:9999/v1",
		LLMGatewayAPIKey:   "synthetic-key",
	}
	return testManagerWith(t, chat, br, cfg, nil)
}

// testManagerWith builds a manager over a caller-supplied config and client
// seam. A nil seam returns the shared fake chatter for every endpoint.
func testManagerWith(t *testing.T, chat *fakeChatter, br *fakeBroker, cfg *config.Config, newClient func(provider string, endpoint config.ProviderEndpoint, virtualKey string, log *config.Logger) llm.Chatter) (*Manager, *memory.ProfileStore, func()) {
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
	log := config.NewLogger(config.LevelError)
	if newClient == nil {
		newClient = func(string, config.ProviderEndpoint, string, *config.Logger) llm.Chatter { return chat }
	}
	m := NewManager(Deps{
		Profile:     "alice",
		ProfileRoot: root,
		Log:         log,
		Store:       store,
		Broker:      br,
		Processes:   fakeProcs{},
		Parent:      chat,
		NewClient:   newClient,
		Registry:    registry.New(log),
		Soul:        soul.NewProfileStore(root, "alice", ""),
		Window:      32768,
		Config:      cfg,
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

func TestSwapConfigUpdatesResolveTarget(t *testing.T) {
	chat := &fakeChatter{}
	br := &fakeBroker{}
	m, _, cleanup := testManager(t, chat, br)
	defer cleanup()

	model, provider, url, client, err := m.resolveTarget(context.Background())
	if err != nil || model != "fake/model" || provider != "fake" || url != "http://localhost:9999/v1" || client != chat {
		t.Fatalf("startup routing = %q %q %q client=%v err=%v", model, provider, url, client, err)
	}

	next := &config.Config{
		LLMGatewayModel:    "fake/parent-next",
		LLMGatewayProvider: "fake",
		LLMGatewayURL:      "http://localhost:9999/v1",
		LLMGatewayAPIKey:   "synthetic-key",
		DelegationModel:    "fake/delegate-next",
		DelegationProvider: "alt",
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"alt": {URL: "http://localhost:7777/v1", APIKey: "synthetic-alt-key"},
		},
	}
	m.SwapConfig(next)

	model, provider, url, _, err = m.resolveTarget(context.Background())
	if err != nil || model != "fake/delegate-next" || provider != "alt" || url != "http://localhost:7777/v1" {
		t.Fatalf("swapped routing = %q %q %q err=%v", model, provider, url, err)
	}

	m.SwapConfig(&config.Config{LLMGatewayModel: "fake/parent-next", LLMGatewayProvider: "fake", LLMGatewayURL: "http://localhost:9999/v1", LLMGatewayAPIKey: "synthetic-key"})
	model, provider, _, _, err = m.resolveTarget(context.Background())
	if err != nil || model != "fake/parent-next" || provider != "fake" {
		t.Fatalf("mirrored routing = %q %q err=%v", model, provider, err)
	}
}

func TestSwapConfigConcurrentResolveTarget(t *testing.T) {
	chat := &fakeChatter{}
	br := &fakeBroker{}
	m, _, cleanup := testManager(t, chat, br)
	defer cleanup()

	configs := []*config.Config{
		{LLMGatewayModel: "fake/alpha", LLMGatewayProvider: "fake", LLMGatewayURL: "http://localhost:9999/v1", LLMGatewayAPIKey: "synthetic-key"},
		{LLMGatewayModel: "fake/beta", LLMGatewayProvider: "fake", LLMGatewayURL: "http://localhost:9999/v1", LLMGatewayAPIKey: "synthetic-key"},
	}
	m.SwapConfig(configs[0])
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for index := 1; ; index++ {
			select {
			case <-stop:
				return
			default:
				m.SwapConfig(configs[index%len(configs)])
			}
		}
	}()
	for range 200 {
		model, _, _, _, err := m.resolveTarget(context.Background())
		if err != nil || (model != "fake/alpha" && model != "fake/beta") {
			t.Errorf("concurrent resolve = %q err=%v", model, err)
			break
		}
	}
	close(stop)
	wg.Wait()
}

func TestSwapConfigNilSafety(t *testing.T) {
	var manager *Manager
	manager.SwapConfig(&config.Config{LLMGatewayModel: "fake/model"})
	if manager.config() != nil {
		t.Fatal("nil manager returned a configuration")
	}
	if _, _, _, _, err := manager.resolveTarget(context.Background()); err == nil {
		t.Fatal("nil manager resolved a target")
	}

	chat := &fakeChatter{}
	br := &fakeBroker{}
	m, _, cleanup := testManager(t, chat, br)
	defer cleanup()
	m.SwapConfig(nil)
	if got := m.config(); got == nil || got.LLMGatewayModel != "fake/model" {
		t.Fatal("nil swap displaced the live configuration")
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

// Synthetic endpoint URLs for the parent-model routing fixtures.
const (
	parentDefaultURL  = "http://localhost:9999/v1"
	parentAltURL      = "http://localhost:7777/v1"
	parentKeylessURL  = "http://localhost:8888/v1"
	parentExplicitURL = "http://localhost:6666/v1"
)

// parentModelConfig places the router-visible providers in the delegate's own
// endpoint map, exactly as a same-profile manager sees them.
func parentModelConfig() *config.Config {
	return &config.Config{
		LLMGatewayModel:    "fake/default-model",
		LLMGatewayProvider: "fake",
		LLMGatewayURL:      parentDefaultURL,
		LLMGatewayAPIKey:   "synthetic-key",
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"fake":    {URL: parentDefaultURL, APIKey: "synthetic-key"},
			"alt":     {URL: parentAltURL, APIKey: "synthetic-alt-key"},
			"keyless": {URL: parentKeylessURL},
		},
	}
}

// endpointClients is a NewClient seam that returns one fake chatter per URL and
// records which endpoints were constructed.
type endpointClients struct {
	mu    sync.Mutex
	urls  []string
	byURL map[string]*fakeChatter
}

func (c *endpointClients) newClient(provider string, endpoint config.ProviderEndpoint, virtualKey string, log *config.Logger) llm.Chatter {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.urls = append(c.urls, endpoint.URL)
	return c.byURL[endpoint.URL]
}

func (c *endpointClients) constructed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.urls...)
}

func TestSyncDispatchUsesParentModelRouting(t *testing.T) {
	parentChat := &fakeChatter{}
	altChat := &fakeChatter{}
	clients := &endpointClients{byURL: map[string]*fakeChatter{parentAltURL: altChat}}
	m, _, cleanup := testManagerWith(t, parentChat, &fakeBroker{}, parentModelConfig(), clients.newClient)
	defer cleanup()

	ctx := dispatchContext(testPrincipal(), "discord:dm:21")
	ctx = requestctx.WithParentModel(ctx, "  pinned-parent-model  ", "alt")

	model, provider, url, client, err := m.resolveTarget(ctx)
	if err != nil || model != "pinned-parent-model" || provider != "alt" || url != parentAltURL || client != altChat {
		t.Fatalf("parent routing = %q %q %q client=%v err=%v", model, provider, url, client, err)
	}

	result, err := m.Execute(ctx, map[string]interface{}{"goal": "Synthetic pinned parent task"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(result.Content, "synthetic done") {
		t.Fatalf("unexpected sync result: %q", result.Content)
	}
	if requests := parentChat.captured(); len(requests) != 0 {
		t.Fatalf("default transport received %d child call(s), want 0", len(requests))
	}
	requests := altChat.captured()
	if len(requests) != 1 || requests[0].Model != "pinned-parent-model" {
		t.Fatalf("parent transport requests = %+v", requests)
	}
}

func TestBackgroundDispatchFreezesParentModelRouting(t *testing.T) {
	parentChat := &fakeChatter{}
	altChat := &fakeChatter{}
	clients := &endpointClients{byURL: map[string]*fakeChatter{parentAltURL: altChat}}
	m, store, cleanup := testManagerWith(t, parentChat, &fakeBroker{}, parentModelConfig(), clients.newClient)
	defer cleanup()

	ctx := dispatchContext(testPrincipal(), "discord:dm:22")
	ctx = requestctx.WithParentModel(ctx, "pinned-parent-model", "alt")
	if _, err := m.Execute(ctx, map[string]interface{}{"goal": "Synthetic pinned background task", "background": true}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var rows []memory.DelegationRow
	waitFor(t, 10*time.Second, func() bool {
		due, err := store.ListDueDeliveries(context.Background(), 10)
		rows = due
		return err == nil && len(due) == 1
	}, "unit never reached terminal state")
	var env dispatchEnvelope
	if err := json.Unmarshal([]byte(rows[0].TaskJSON), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Model != "pinned-parent-model" || env.Provider != "alt" || env.BaseURL != parentAltURL {
		t.Fatalf("frozen routing = %q %q %q", env.Model, env.Provider, env.BaseURL)
	}
}

func TestExplicitDelegationWinsOverParentModel(t *testing.T) {
	parentChat := &fakeChatter{}
	explicitChat := &fakeChatter{}
	clients := &endpointClients{byURL: map[string]*fakeChatter{parentExplicitURL: explicitChat}}
	cfg := parentModelConfig()
	cfg.DelegationModel = "explicit-delegate-model"
	cfg.DelegationProvider = "explicit"
	cfg.ProviderEndpoints["explicit"] = config.ProviderEndpoint{URL: parentExplicitURL, APIKey: "synthetic-explicit-key"}
	m, _, cleanup := testManagerWith(t, parentChat, &fakeBroker{}, cfg, clients.newClient)
	defer cleanup()

	ctx := requestctx.WithParentModel(dispatchContext(testPrincipal(), "discord:dm:23"), "pinned-parent-model", "alt")
	model, provider, url, client, err := m.resolveTarget(ctx)
	if err != nil || model != "explicit-delegate-model" || provider != "explicit" || url != parentExplicitURL || client != explicitChat {
		t.Fatalf("explicit routing = %q %q %q client=%v err=%v", model, provider, url, client, err)
	}
}

func TestParentModelUnknownProviderFailsClosed(t *testing.T) {
	m, _, cleanup := testManagerWith(t, &fakeChatter{}, &fakeBroker{}, parentModelConfig(), nil)
	defer cleanup()

	ctx := requestctx.WithParentModel(dispatchContext(testPrincipal(), "discord:dm:24"), "pinned-parent-model", "ghost")
	if _, _, _, _, err := m.resolveTarget(ctx); err == nil || err.Error() != `delegate: provider "ghost" has no usable endpoint` {
		t.Fatalf("unknown parent provider err = %v", err)
	}
}

func TestResolveTargetWithoutParentModelMirrorsGateway(t *testing.T) {
	parentChat := &fakeChatter{}
	m, _, cleanup := testManagerWith(t, parentChat, &fakeBroker{}, parentModelConfig(), nil)
	defer cleanup()

	model, provider, url, client, err := m.resolveTarget(context.Background())
	if err != nil || model != "fake/default-model" || provider != "fake" || url != parentDefaultURL || client != parentChat {
		t.Fatalf("default routing = %q %q %q client=%v err=%v", model, provider, url, client, err)
	}
}

func TestParentModelKeylessProviderResolves(t *testing.T) {
	keylessChat := &fakeChatter{}
	clients := &endpointClients{byURL: map[string]*fakeChatter{parentKeylessURL: keylessChat}}
	m, _, cleanup := testManagerWith(t, &fakeChatter{}, &fakeBroker{}, parentModelConfig(), clients.newClient)
	defer cleanup()

	ctx := requestctx.WithParentModel(context.Background(), "pinned-keyless-model", "keyless")
	for attempt := 0; attempt < 2; attempt++ {
		model, provider, url, client, err := m.resolveTarget(ctx)
		if err != nil || model != "pinned-keyless-model" || provider != "keyless" || url != parentKeylessURL || client != keylessChat {
			t.Fatalf("keyless routing (%d) = %q %q %q client=%v err=%v", attempt, model, provider, url, client, err)
		}
	}
	if constructed := clients.constructed(); len(constructed) != 1 || constructed[0] != parentKeylessURL {
		t.Fatalf("keyless client constructions = %v", constructed)
	}
}

func TestParentModelConcurrentDispatches(t *testing.T) {
	parentChat := &fakeChatter{}
	altChat := &fakeChatter{}
	keylessChat := &fakeChatter{}
	clients := &endpointClients{byURL: map[string]*fakeChatter{
		parentAltURL:     altChat,
		parentKeylessURL: keylessChat,
	}}
	m, _, cleanup := testManagerWith(t, parentChat, &fakeBroker{}, parentModelConfig(), clients.newClient)
	defer cleanup()

	contexts := []context.Context{
		requestctx.WithParentModel(dispatchContext(testPrincipal(), "discord:dm:31"), "pinned-alt-model", "alt"),
		requestctx.WithParentModel(dispatchContext(testPrincipal(), "discord:dm:32"), "pinned-keyless-model", "keyless"),
	}
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Execute(contexts[i%len(contexts)], map[string]interface{}{"goal": "Concurrent synthetic parent task"})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}
	if requests := parentChat.captured(); len(requests) != 0 {
		t.Fatalf("default transport received %d child call(s), want 0", len(requests))
	}
	if got := len(altChat.captured()) + len(keylessChat.captured()); got != len(errs) {
		t.Fatalf("parent-transport child calls = %d, want %d", got, len(errs))
	}
}
