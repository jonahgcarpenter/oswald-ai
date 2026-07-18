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
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestExecuteHandlesIgnoreFallbackCommandAndLLM(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	deps, shutdown := testDependencies(t, log)
	defer shutdown()

	ignored := Execute(Request{IsGroup: true, Text: "hello"}, deps, &fakeResponder{})
	if ignored.Action != routing.ActionIgnore || ignored.Reason != "group_message_without_invocation" {
		t.Fatalf("unexpected ignore outcome: %+v", ignored)
	}

	fallbackResponder := &fakeResponder{}
	fallback := Execute(Request{Text: " "}, deps, fallbackResponder)
	if fallback.Action != routing.ActionGatewayFallback || fallbackResponder.fallback != "What do you want idiot." {
		t.Fatalf("unexpected fallback outcome=%+v responder=%+v", fallback, fallbackResponder)
	}

	cmdResponder := &fakeResponder{}
	cmd := Execute(Request{Principal: testPrincipal("user"), Text: "/ping"}, deps, cmdResponder)
	if cmd.Action != routing.ActionCommand || cmdResponder.command != "pong:user:/ping" {
		t.Fatalf("unexpected command outcome=%+v responder=%+v", cmd, cmdResponder)
	}

	unknownCmdResponder := &fakeResponder{}
	unknownCmd := Execute(Request{Principal: testPrincipal("user"), Text: "/unknown arg"}, deps, unknownCmdResponder)
	if unknownCmd.Action != routing.ActionCommand || unknownCmdResponder.command != "Unknown command: /unknown" || unknownCmdResponder.started || unknownCmdResponder.agent != nil {
		t.Fatalf("unexpected unknown command outcome=%+v responder=%+v", unknownCmd, unknownCmdResponder)
	}

	llmResponder := &fakeResponder{}
	outcome := Execute(Request{RequestID: "req", Principal: testPrincipal("user"), ChatID: "chat", SessionKey: "session", IsMention: true, Text: "hello"}, deps, llmResponder)
	if outcome.Action != routing.ActionLLM || llmResponder.agent == nil || llmResponder.agent.Response != "agent response" {
		t.Fatalf("unexpected llm outcome=%+v responder=%+v", outcome, llmResponder)
	}
	if !llmResponder.started || !llmResponder.cleaned {
		t.Fatalf("expected processing cleanup, responder=%+v", llmResponder)
	}
}

func TestExecuteRejectsBannedUsersBeforeCommandOrLLM(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	deps, shutdown := testDependencies(t, log)
	defer shutdown()
	deps.Access = &fakeAccess{banned: true, reason: "spam"}

	cmdResponder := &fakeResponder{}
	cmd := Execute(Request{RequestID: "req", Principal: testPrincipal("user"), Text: "/ping"}, deps, cmdResponder)
	if cmd.Reason != "user_banned" || cmdResponder.fallback != "You are banned from using Oswald.\nReason: spam" || cmdResponder.command != "" {
		t.Fatalf("unexpected banned command outcome=%+v responder=%+v", cmd, cmdResponder)
	}

	deps.Access = &fakeAccess{banned: true}
	llmResponder := &fakeResponder{}
	llm := Execute(Request{RequestID: "req", Principal: testPrincipal("user"), IsMention: true, Text: "hello"}, deps, llmResponder)
	if llm.Reason != "user_banned" || llmResponder.fallback != "You are banned from using Oswald.\nReason: No reason provided." || llmResponder.started || llmResponder.agent != nil {
		t.Fatalf("unexpected banned llm outcome=%+v responder=%+v", llm, llmResponder)
	}
}

func TestExecuteUsesPrincipalCanonicalUserForAccess(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	deps, shutdown := testDependencies(t, log)
	defer shutdown()
	access := &fakeAccess{}
	deps.Access = access

	Execute(Request{RequestID: "req", Principal: testPrincipal("canonical-user"), Text: "/ping"}, deps, &fakeResponder{})
	if access.userID != "canonical-user" {
		t.Fatalf("access user ID = %q, want canonical-user", access.userID)
	}
}

func TestExecuteRejectsInvalidPrincipalBeforeOwnedOperations(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	deps, shutdown := testDependencies(t, log)
	defer shutdown()
	access := &fakeAccess{}
	deps.Access = access
	responder := &fakeResponder{}

	outcome := Execute(Request{RequestID: "req", Text: "/ping"}, deps, responder)
	if outcome.Reason != "invalid_principal" || responder.agentErr == "" || responder.command != "" || access.userID != "" {
		t.Fatalf("unexpected invalid principal outcome=%+v responder=%+v access=%+v", outcome, responder, access)
	}
}

func TestExecuteSerializesCommandBehindAgentRequest(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	processor := &blockingRuntimeProcessor{started: make(chan struct{}), release: make(chan struct{})}
	b := broker.NewBroker(processor, 2, log)
	b.Start()
	defer b.Shutdown()
	commandService, err := commands.NewService(pingHandler{})
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{Broker: b, Commands: commandService, Log: log}
	principal := testPrincipal("user")
	llmDone := make(chan Outcome, 1)
	go func() {
		llmDone <- Execute(Request{RequestID: "llm", Principal: principal, SessionKey: "session", IsMention: true, Text: "hello"}, deps, &fakeResponder{})
	}()
	<-processor.started
	commandResponder := &fakeResponder{}
	commandDone := make(chan Outcome, 1)
	go func() {
		commandDone <- Execute(Request{RequestID: "command", Principal: principal, SessionKey: "session", Text: "/ping"}, deps, commandResponder)
	}()
	select {
	case <-commandDone:
		t.Fatal("command overtook active same-session agent request")
	case <-time.After(50 * time.Millisecond):
	}
	close(processor.release)
	if outcome := <-llmDone; outcome.Err != nil {
		t.Fatalf("agent outcome: %+v", outcome)
	}
	if outcome := <-commandDone; outcome.Err != nil || commandResponder.command != "pong:user:/ping" {
		t.Fatalf("command outcome=%+v responder=%+v", outcome, commandResponder)
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

func (pingHandler) Definition() commands.Definition {
	return commands.Definition{Name: "ping"}
}

func (pingHandler) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	return commands.Result{Text: "pong:" + req.Principal.CanonicalUserID + ":" + req.Raw}, nil
}

type fakeAccess struct {
	banned bool
	reason string
	userID string
}

func (a *fakeAccess) BanStatus(userID string) (bool, string, error) {
	a.userID = userID
	return a.banned, a.reason, nil
}

func testPrincipal(userID string) identity.Principal {
	return identity.Principal{CanonicalUserID: userID, Gateway: "websocket", ExternalID: "external-" + userID, Assurance: identity.AssuranceSelfAsserted}
}

type runtimeFakeChatter struct{}

func (runtimeFakeChatter) Chat(context.Context, llm.ChatRequest, func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "agent response"}}, nil
}

type blockingRuntimeProcessor struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingRuntimeProcessor) Process(agent.Request) (*agent.AgentResponse, error) {
	close(p.started)
	<-p.release
	return &agent.AgentResponse{Response: "agent response"}, nil
}

func testDependencies(t *testing.T, log *config.Logger) (Dependencies, func()) {
	t.Helper()
	dir := t.TempDir()
	soulStore := soul.NewStore(filepath.Join(dir, "soul.md"), log)
	if err := soulStore.Write("You are Oswald."); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	dbPath := filepath.Join(dir, "users")
	db, err := database.Open(dbPath, log)
	if err != nil {
		t.Fatalf("open account database: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL().Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, "user", now, now); err != nil {
		db.Close() // nolint:errcheck
		t.Fatalf("seed account user: %v", err)
	}
	db.Close() // nolint:errcheck
	memory := usermemory.NewStore(dbPath, log)
	ai := agent.NewAgent(runtimeFakeChatter{}, registry.New(log), "test-model", soulStore, memory, promptbudget.ContextBudget{PromptLimit: 100000}, 3, time.Minute, log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	commandService, err := commands.NewService(pingHandler{})
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	return Dependencies{Broker: b, Commands: commandService, Log: log}, func() {
		b.Shutdown()
		memory.Close() // nolint:errcheck
	}
}
