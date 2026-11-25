package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Synthetic routed-turn fixtures. The session key and pin values are obviously
// fake; no state from an operator profile is touched.
const (
	pinnedTurnSessionKey = "discord:dm:55501"
	pinnedTurnModel      = "pinned-model"
	pinnedTurnProvider   = "alt-provider"
)

// newPinnedTurnAgent installs a router whose conversation pin routes the
// synthetic session to the alternate transport. The default client must never
// be called on a successful pinned turn.
func newPinnedTurnAgent(t *testing.T) (*Agent, *agentMemoryFixture, *fakeChatter, *fakeChatter) {
	t.Helper()
	defaultChat := &fakeChatter{responses: []*llm.ChatResponse{{
		Model:   "default-model",
		Message: llm.ChatMessage{Role: "assistant", Content: "default answer"},
	}}}
	pinnedChat := &fakeChatter{responses: []*llm.ChatResponse{{
		PromptTokens: 7, CompletionTokens: 2,
		Message: llm.ChatMessage{Role: "assistant", Content: "pinned answer"},
	}}}
	agent, fixture := newTestAgent(t, defaultChat, nil, nil)
	pins := newRouterPinStore()
	pins.set("user-1", pinnedTurnSessionKey, pinnedTurnProvider, pinnedTurnModel)
	router := NewModelRouter(routerTestView(), defaultChat, func(url, apiKey, virtualKey string) llm.Chatter {
		if url != routerAlternateURL || apiKey != routerAlternateKey {
			t.Errorf("router constructed client for %q/%q, want the alternate transport", url, apiKey)
		}
		return pinnedChat
	}, nil, pins)
	agent.SetModelRouter(router)
	return agent, fixture, defaultChat, pinnedChat
}

// captureAgentLog redirects a fresh logger to a temp file. The returned
// function reads every emitted record.
func captureAgentLog(t *testing.T, level config.Level) (*config.Logger, func() []map[string]any) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	old := os.Stderr
	os.Stderr = file
	log := config.NewLogger(level)
	os.Stderr = old
	return log, func() []map[string]any {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("decode log record: %v", err)
			}
			records = append(records, record)
		}
		return records
	}
}

func TestModelRouteTurnResolvesConversationPin(t *testing.T) {
	agent, fixture, defaultChat, pinnedChat := newPinnedTurnAgent(t)

	response, err := processAgent(agent, "req-pinned", "discord", pinnedTurnSessionKey, "user-1", "Test User", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if response.Model != pinnedTurnModel {
		t.Fatalf("response model = %q, want %q", response.Model, pinnedTurnModel)
	}
	if len(defaultChat.requests) != 0 {
		t.Fatalf("default client received %d call(s), want 0", len(defaultChat.requests))
	}
	if len(pinnedChat.requests) != 1 || pinnedChat.requests[0].Model != pinnedTurnModel {
		t.Fatalf("pinned requests = %+v", pinnedChat.requests)
	}

	usage, err := fixture.stores["user-1"].SessionModelUsage(context.Background(), "user-1", pinnedTurnSessionKey, 1)
	if err != nil || len(usage) != 1 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	row := usage[0]
	if row.Model != pinnedTurnModel || row.BillingProvider != pinnedTurnProvider || row.BillingBaseURL != routerAlternateURL {
		t.Fatalf("usage attribution = %+v", row)
	}
	if row.ApiCalls != 1 || row.PromptTokens != 7 || row.CompletionTokens != 2 {
		t.Fatalf("usage counters = %+v", row)
	}

	sessionID, err := fixture.stores["user-1"].ActiveSessionID(context.Background(), "user-1", pinnedTurnSessionKey, 1)
	if err != nil {
		t.Fatal(err)
	}
	var model, provider, baseURL string
	if err := fixture.sql.QueryRow(`SELECT model,billing_provider,billing_base_url FROM sessions WHERE id=?`, sessionID).Scan(&model, &provider, &baseURL); err != nil {
		t.Fatal(err)
	}
	if model != pinnedTurnModel || provider != pinnedTurnProvider || baseURL != routerAlternateURL {
		t.Fatalf("session attribution = %q %q %q", model, provider, baseURL)
	}
}

func TestModelRouteTurnPersistsPinnedModelConfig(t *testing.T) {
	agent, fixture, _, _ := newPinnedTurnAgent(t)

	if _, err := processAgent(agent, "req-config", "discord", pinnedTurnSessionKey, "user-1", "Test User", "question", nil, nil); err != nil {
		t.Fatalf("process: %v", err)
	}
	sessionID, err := fixture.stores["user-1"].ActiveSessionID(context.Background(), "user-1", pinnedTurnSessionKey, 1)
	if err != nil {
		t.Fatal(err)
	}
	var modelConfig string
	if err := fixture.sql.QueryRow(`SELECT model_config FROM sessions WHERE id=?`, sessionID).Scan(&modelConfig); err != nil {
		t.Fatal(err)
	}
	wantConfig := `{"gateway_runtime":{"api_mode":"chat_completions","base_url":"` + routerAlternateURL + `","provider":"` + pinnedTurnProvider + `"}}`
	if modelConfig != wantConfig {
		t.Fatalf("model_config = %q, want %q", modelConfig, wantConfig)
	}

	var encoded string
	if err := fixture.sql.QueryRow(`SELECT value FROM state_meta WHERE key LIKE ?`, "oswald:v1:turn:"+sessionID+":%").Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var state struct {
		Pressure struct {
			Version string `json:"version"`
		} `json:"pressure"`
	}
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state.Pressure.Version, pinnedTurnModel) {
		t.Fatalf("pressure version = %q, want the pinned model", state.Pressure.Version)
	}
}

func TestModelRouteTurnUnreadablePinWarnsOnce(t *testing.T) {
	defaultChat := &fakeChatter{responses: []*llm.ChatResponse{{
		Model:   "default-model",
		Message: llm.ChatMessage{Role: "assistant", Content: "default answer"},
	}}}
	agent, _ := newTestAgent(t, defaultChat, nil, nil)
	log, readLog := captureAgentLog(t, config.LevelWarn)
	agent.log = log
	pins := newRouterPinStore()
	pins.fail(routerUnreadablePinError{})
	router := NewModelRouter(routerTestView(), defaultChat, func(url, apiKey, virtualKey string) llm.Chatter {
		t.Fatalf("unreadable pin constructed a client for %q", url)
		return nil
	}, nil, pins)
	agent.SetModelRouter(router)

	response, err := processAgent(agent, "req-unreadable", "discord", pinnedTurnSessionKey, "user-1", "Test User", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if response.Model != "default-model" {
		t.Fatalf("response model = %q, want default", response.Model)
	}
	if len(defaultChat.requests) != 1 || defaultChat.requests[0].Model != "default-model" {
		t.Fatalf("default requests = %+v", defaultChat.requests)
	}

	warnings := 0
	for _, record := range readLog() {
		if record["event"] != "agent.model.pin_invalid" {
			continue
		}
		warnings++
		if record["level"] != "warn" {
			t.Fatalf("degradation level = %v", record["level"])
		}
		details := logDetails(t, record)
		if details["link"] != "pin" || details["reason"] != "unreadable" {
			t.Fatalf("degradation fields = %v", details)
		}
	}
	if warnings != 1 {
		t.Fatalf("agent.model.pin_invalid warnings = %d, want 1", warnings)
	}
}

// swapBlockingChatter blocks inside Chat until released so a view swap lands
// while the turn is mid-flight.
type swapBlockingChatter struct {
	started  chan struct{}
	release  chan struct{}
	startOne sync.Once
	mu       sync.Mutex
	requests []llm.ChatRequest
}

func newSwapBlockingChatter() *swapBlockingChatter {
	return &swapBlockingChatter{started: make(chan struct{}), release: make(chan struct{})}
}

func (c *swapBlockingChatter) Chat(ctx context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	c.startOne.Do(func() { close(c.started) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "stable answer"}}, nil
}

func (c *swapBlockingChatter) captured() []llm.ChatRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]llm.ChatRequest(nil), c.requests...)
}

func TestModelRouteTurnSwapViewIsolation(t *testing.T) {
	blocking := newSwapBlockingChatter()
	agent, _ := newTestAgent(t, blocking, nil, nil)
	var constructed atomic.Int32
	router := NewModelRouter(routerTestView(), blocking, func(url, apiKey, virtualKey string) llm.Chatter {
		constructed.Add(1)
		return &fakeChatter{responses: []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "rotated answer"}}}}
	}, nil, newRouterPinStore())
	agent.SetModelRouter(router)

	type turnResult struct {
		response *Response
		err      error
	}
	done := make(chan turnResult, 1)
	go func() {
		response, err := processAgent(agent, "req-swap", "discord", pinnedTurnSessionKey, "user-1", "Test User", "question", nil, nil)
		done <- turnResult{response: response, err: err}
	}()

	<-blocking.started
	rotated := routerTestView()
	rotated.Default = DefaultTarget{
		Model:      "rotated-model",
		Provider:   "rotated-provider",
		URL:        routerRotatedURL,
		APIKey:     routerRotatedKey,
		VirtualKey: routerVirtualKey,
	}
	rotated.Endpoints["rotated-provider"] = config.ProviderEndpoint{URL: routerRotatedURL, APIKey: routerRotatedKey}
	router.SwapView(rotated)
	close(blocking.release)

	result := <-done
	if result.err != nil {
		t.Fatalf("process: %v", result.err)
	}
	if result.response.Model != "default-model" {
		t.Fatalf("response model = %q, want the model resolved at turn start", result.response.Model)
	}
	requests := blocking.captured()
	if len(requests) != 1 || requests[0].Model != "default-model" {
		t.Fatalf("in-flight requests = %+v", requests)
	}
	if got := constructed.Load(); got != 0 {
		t.Fatalf("rotated default constructed %d client(s) during an in-flight turn", got)
	}
}

// TestModelRouteTurnCarriesParentModelToTools proves the context handed to
// tool execution carries the turn's resolved routing, which is exactly what
// delegated children mirror. A pinned turn must expose the pinned provider and
// model, not the construction-time defaults.
func TestModelRouteTurnCarriesParentModelToTools(t *testing.T) {
	defaultChat := &fakeChatter{responses: []*llm.ChatResponse{{
		Model:   "default-model",
		Message: llm.ChatMessage{Role: "assistant", Content: "default answer"},
	}}}
	pinnedChat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: pinnedTurnModel, Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "capture", Function: llm.ToolFunction{Name: "test.parent_model", Arguments: map[string]interface{}{}},
		}}}},
		{Model: pinnedTurnModel, Message: llm.ChatMessage{Role: "assistant", Content: "captured routing"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	agent, _ := newTestAgent(t, defaultChat, nil, reg)
	var got requestctx.ParentModel
	var attached bool
	if err := registerTestTool(t, reg, testToolSpec{Name: "test.parent_model", Schema: &llm.ToolParameters{Type: "object"}}, testToolPolicy(), func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
		got, attached = requestctx.ParentModelFromContext(ctx)
		return productiveResult("captured"), nil
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	pins := newRouterPinStore()
	pins.set("user-1", pinnedTurnSessionKey, pinnedTurnProvider, pinnedTurnModel)
	router := NewModelRouter(routerTestView(), defaultChat, func(url, apiKey, virtualKey string) llm.Chatter {
		if url != routerAlternateURL || apiKey != routerAlternateKey {
			t.Errorf("router constructed client for %q/%q, want the alternate transport", url, apiKey)
		}
		return pinnedChat
	}, nil, pins)
	agent.SetModelRouter(router)

	response, err := processAgent(agent, "req-parent-model", "discord", pinnedTurnSessionKey, "user-1", "Test User", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if response.Model != pinnedTurnModel {
		t.Fatalf("response model = %q, want %q", response.Model, pinnedTurnModel)
	}
	if len(defaultChat.requests) != 0 {
		t.Fatalf("default client received %d call(s), want 0", len(defaultChat.requests))
	}
	if !attached || got.Model != pinnedTurnModel || got.Provider != pinnedTurnProvider {
		t.Fatalf("tool ctx parent model = %+v attached=%v", got, attached)
	}
}
