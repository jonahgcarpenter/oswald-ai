package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestExecuteHandlesIgnoreFallbackCommandAndLLM(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	deps, shutdown := testDependencies(t, log)
	defer shutdown()

	ignored := Execute(Request{Gateway: "test", IsGroup: true, Text: "hello"}, deps, &fakeResponder{})
	if ignored.Action != routing.ActionIgnore || ignored.Reason != "group_message_without_invocation" {
		t.Fatalf("unexpected ignore outcome: %+v", ignored)
	}

	fallbackResponder := &fakeResponder{}
	fallback := Execute(Request{Gateway: "test", Text: " "}, deps, fallbackResponder)
	if fallback.Action != routing.ActionGatewayFallback || fallbackResponder.fallback != "What do you want idiot." {
		t.Fatalf("unexpected fallback outcome=%+v responder=%+v", fallback, fallbackResponder)
	}

	cmdResponder := &fakeResponder{}
	cmd := Execute(Request{Gateway: "test", SenderID: "user", IsCommand: true, Text: "/ping"}, deps, cmdResponder)
	if cmd.Action != routing.ActionCommand || cmdResponder.command != "pong:user:/ping" {
		t.Fatalf("unexpected command outcome=%+v responder=%+v", cmd, cmdResponder)
	}

	llmResponder := &fakeResponder{}
	outcome := Execute(Request{RequestID: "req", Gateway: "test", ChatID: "chat", SenderID: "user", SessionKey: "session", IsMention: true, Text: "hello"}, deps, llmResponder)
	if outcome.Action != routing.ActionLLM || llmResponder.agent == nil || llmResponder.agent.Response != "agent response" {
		t.Fatalf("unexpected llm outcome=%+v responder=%+v", outcome, llmResponder)
	}
	if !llmResponder.started || !llmResponder.cleaned {
		t.Fatalf("expected processing cleanup, responder=%+v", llmResponder)
	}
}

type fakeResponder struct {
	started  bool
	cleaned  bool
	fallback string
	command  string
	agent    *agent.AgentResponse
	agentErr string
}

func (r *fakeResponder) StartProcessing() (func(), error) {
	r.started = true
	return func() { r.cleaned = true }, nil
}

func (r *fakeResponder) SendFallback(text string) error {
	r.fallback = text
	return nil
}

func (r *fakeResponder) SendCommandResponse(text string) error {
	r.command = text
	return nil
}

func (r *fakeResponder) SendAgentResponse(response *agent.AgentResponse) error {
	r.agent = response
	return nil
}

func (r *fakeResponder) SendAgentError(text string) error {
	r.agentErr = text
	return nil
}

type pingHandler struct{}

func (pingHandler) CanHandle(input string) bool { return input == "/ping" }

func (pingHandler) Handle(userID, input string) (string, bool, error) {
	return "pong:" + userID + ":" + input, true, nil
}

type runtimeFakeChatter struct{}

func (runtimeFakeChatter) Chat(context.Context, llm.ChatRequest, func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "agent response"}}, nil
}

func testDependencies(t *testing.T, log *config.Logger) (Dependencies, func()) {
	t.Helper()
	dir := t.TempDir()
	soulStore := soul.NewStore(filepath.Join(dir, "soul.md"), log)
	if err := soulStore.Write("You are Oswald."); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	ai := agent.NewAgent(runtimeFakeChatter{}, nil, registry.New(log), "test-model", "", soulStore, usermemory.NewStore(filepath.Join(dir, "users"), log), memory.ContextBudget{PromptLimit: 100000}, 3, time.Minute, memory.NewStore(memory.Options{}, log), log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	return Dependencies{Broker: b, Commands: commands.NewRouter(pingHandler{}), Log: log}, b.Shutdown
}
