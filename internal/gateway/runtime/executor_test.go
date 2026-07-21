package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/privacyruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
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
	if cmd.Action != routing.ActionCommand || cmdResponder.command.Text != "pong:user:/ping" {
		t.Fatalf("unexpected command outcome=%+v responder=%+v", cmd, cmdResponder)
	}

	unknownCmdResponder := &fakeResponder{}
	unknownCmd := Execute(Request{Principal: testPrincipal("user"), Text: "/unknown arg"}, deps, unknownCmdResponder)
	if unknownCmd.Action != routing.ActionCommand || unknownCmdResponder.command.Text != "Unknown command: /unknown" || unknownCmdResponder.started || unknownCmdResponder.agent != nil {
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
	if cmd.Reason != "user_banned" || cmdResponder.fallback != "You are banned from using Oswald.\nReason: spam" || cmdResponder.command.Text != "" {
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

func TestExecuteValidatesCommandAttachmentBeforeDelivery(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	commandService, err := commands.NewService(commands.HandlerFunc{
		DefinitionValue: commands.Definition{Name: "export"},
		ExecuteFunc: func(context.Context, commands.Request) (commands.Result, error) {
			return commands.Result{Text: "export", Attachment: &commands.Attachment{
				Filename: "../private.json", MIMEType: "application/json", Data: []byte("private-content"),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	responder := &fakeResponder{}
	outcome := Execute(Request{RequestID: "req", Principal: testPrincipal("user"), Text: "/export"}, Dependencies{Commands: commandService, Log: log}, responder)
	if outcome.Action != routing.ActionCommand || responder.command.Attachment != nil {
		t.Fatalf("invalid attachment reached responder: outcome=%+v result=%+v", outcome, responder.command)
	}
	if !strings.Contains(responder.command.Text, "filename must be a base name") {
		t.Fatalf("unexpected validation response %q", responder.command.Text)
	}
}

func TestExecuteRejectsDuplicateCommandAttachmentNames(t *testing.T) {
	commandService, err := commands.NewService(commands.HandlerFunc{
		DefinitionValue: commands.Definition{Name: "export"},
		ExecuteFunc: func(context.Context, commands.Request) (commands.Result, error) {
			return commands.Result{Attachments: []commands.Attachment{
				{Filename: "same.json", MIMEType: "application/json", Data: []byte("one")},
				{Filename: "same.json", MIMEType: "application/json", Data: []byte("two")},
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	responder := &fakeResponder{}
	Execute(Request{RequestID: "req", Principal: testPrincipal("user"), Text: "/export"}, Dependencies{Commands: commandService, Log: config.NewLogger(config.LevelError)}, responder)
	if len(responder.command.OrderedAttachments()) != 0 || !strings.Contains(responder.command.Text, "duplicate filename") {
		t.Fatalf("duplicate attachments reached responder: %+v", responder.command)
	}
}

func TestExecuteAttachmentLogsExcludeContent(t *testing.T) {
	oldStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	defer func() { os.Stderr = oldStderr }()
	log := config.NewLogger(config.LevelDebug)
	const privateContent = "attachment-private-marker"
	commandService, err := commands.NewService(commands.HandlerFunc{
		DefinitionValue: commands.Definition{Name: "export"},
		ExecuteFunc: func(context.Context, commands.Request) (commands.Result, error) {
			return commands.Result{Attachments: []commands.Attachment{{Filename: "safe.json", MIMEType: "application/json", Data: []byte(privateContent)}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	Execute(Request{RequestID: "req", Principal: testPrincipal("user"), Text: "/export"}, Dependencies{Commands: commandService, Log: log}, &fakeResponder{})
	_ = writer.Close()
	output, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), privateContent) || !strings.Contains(string(output), "safe.json") || !strings.Contains(string(output), "attachment_bytes") {
		t.Fatalf("unsafe or incomplete attachment log: %s", output)
	}
}

func TestExecutePublishesCommandInvalidationAfterEveryDeliveryAttempt(t *testing.T) {
	event := privacyruntime.Event{ExternalIdentities: []string{"websocket:subject"}, SessionIDs: []string{"session"}, CloseConnections: true}
	service, err := commands.NewService(commands.HandlerFunc{
		DefinitionValue: commands.Definition{Name: "erase", UserExclusive: true},
		ExecuteFunc: func(context.Context, commands.Request) (commands.Result, error) {
			return commands.Result{Text: "deleted", Invalidation: &event}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bus := privacyruntime.NewBus()
	responder := &fakeResponder{}
	published := 0
	bus.Subscribe(func(got privacyruntime.Event) {
		published++
		if responder.command.Text != "deleted" || !got.CloseConnections {
			t.Fatalf("invalidation published before delivery or with wrong event: response=%+v event=%+v", responder.command, got)
		}
	})
	Execute(Request{Principal: testPrincipal("user"), SessionKey: "session", Text: "/erase"}, Dependencies{Commands: service, Log: config.NewLogger(config.LevelError), PrivacyBus: bus}, responder)
	if published != 1 {
		t.Fatalf("published=%d want 1", published)
	}
	responder.sendErr = errors.New("offline")
	outcome := Execute(Request{Principal: testPrincipal("user"), SessionKey: "session", Text: "/erase"}, Dependencies{Commands: service, Log: config.NewLogger(config.LevelError), PrivacyBus: bus}, responder)
	if published != 2 || !errors.Is(outcome.Err, responder.sendErr) {
		t.Fatalf("failed delivery invalidation count=%d outcome=%+v", published, outcome)
	}
}

func TestExecuteEnqueuesFormationOnlyAfterResponseDelivery(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	deps, shutdown := testDependencies(t, log)
	defer shutdown()
	responder := &fakeResponder{}
	enqueuer := &fakeFormationEnqueuer{responder: responder}
	deps.Formation = enqueuer
	outcome := Execute(Request{RequestID: "req", Principal: testPrincipal("user"), ChatID: "chat", SessionKey: "session", IsDirect: true, Text: "hello"}, deps, responder)
	if outcome.Err != nil || !enqueuer.called || enqueuer.userID != "user" || enqueuer.source.TurnID <= 0 {
		t.Fatalf("outcome=%+v enqueuer=%+v", outcome, enqueuer)
	}
	if !enqueuer.responseDelivered {
		t.Fatal("formation was enqueued before response delivery")
	}
}

func TestExecutePublishesGlobalMemoryOnlyAfterSuccessfulDelivery(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	processor := responseRuntimeProcessor{response: &agent.AgentResponse{Model: "model", Response: "answer", SourceTurnID: 77}}
	b := broker.NewBroker(processor, 1, log)
	b.Start()
	defer b.Shutdown()
	responder := &fakeResponder{}
	publisher := &fakeGlobalMemoryPublisher{responder: responder}
	deps := Dependencies{Broker: b, Log: log, GlobalMemory: publisher}
	Execute(Request{RequestID: "req", Principal: testPrincipal("user"), SessionKey: "session", IsDirect: true, Text: "hello"}, deps, responder)
	if !publisher.called || !publisher.responseDelivered || publisher.requestID != "req" || publisher.turnID != 77 {
		t.Fatalf("publisher=%+v", publisher)
	}
	responder = &fakeResponder{sendErr: errors.New("offline")}
	publisher = &fakeGlobalMemoryPublisher{responder: responder}
	deps.GlobalMemory = publisher
	Execute(Request{RequestID: "failed", Principal: testPrincipal("user"), SessionKey: "session", IsDirect: true, Text: "hello"}, deps, responder)
	if publisher.called {
		t.Fatal("global memory published after failed delivery")
	}
	if !publisher.discarded {
		t.Fatal("undelivered global memory was not discarded")
	}
}

func TestExecuteEnqueuesCompactionOnlyAfterSuccessfulDelivery(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	processor := responseRuntimeProcessor{response: &agent.AgentResponse{Model: "model", Response: "answer", SourceTurnID: 77, SessionGeneration: 3}}
	b := broker.NewBroker(processor, 1, log)
	b.Start()
	defer b.Shutdown()
	responder := &fakeResponder{}
	compaction := &fakeCompactionEnqueuer{responder: responder}
	deps := Dependencies{Broker: b, Log: log, Compaction: compaction}
	Execute(Request{RequestID: "req", Principal: testPrincipal("user"), SessionKey: "session", IsDirect: true, Text: "hello"}, deps, responder)
	if !compaction.enqueueCalled || !compaction.responseDelivered || compaction.source.TurnID != 77 || compaction.source.SessionGeneration != 3 {
		t.Fatalf("compaction enqueue=%+v", compaction)
	}

	failedResponder := &fakeResponder{sendErr: errors.New("offline")}
	failed := &fakeCompactionEnqueuer{responder: failedResponder}
	deps.Compaction = failed
	Execute(Request{RequestID: "req-failed", Principal: testPrincipal("user"), SessionKey: "session", IsDirect: true, Text: "hello"}, deps, failedResponder)
	if failed.enqueueCalled || !failed.failureMarked || failed.source.TurnID != 77 {
		t.Fatalf("failed delivery bookkeeping = %+v", failed)
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
	if outcome.Reason != "invalid_principal" || responder.agentErr == "" || responder.command.Text != "" || access.userID != "" {
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
	if outcome := <-commandDone; outcome.Err != nil || commandResponder.command.Text != "pong:user:/ping" {
		t.Fatalf("command outcome=%+v responder=%+v", outcome, commandResponder)
	}
}

func TestExecuteHoldsResolvedUserFencesThroughDeliveryAndInvalidation(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	b := broker.NewBroker(nil, 4, log)
	b.Start()
	defer b.Shutdown()
	target := testPrincipal("target")
	actor := testPrincipal("actor")
	activeStarted := make(chan struct{})
	releaseActive := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), target, "active", func() error {
			close(activeStarted)
			<-releaseActive
			return nil
		})
	}()
	<-activeStarted

	event := privacyruntime.Event{SessionIDs: []string{"target-session"}}
	commandStarted := make(chan struct{})
	service, err := commands.NewService(commands.HandlerFunc{
		DefinitionValue: commands.Definition{Name: "deleteuser", UserExclusive: true},
		ResolveFenceTargetsFunc: func(_ context.Context, req commands.Request) ([]string, error) {
			return []string{req.Args[0]}, nil
		},
		ExecuteFunc: func(context.Context, commands.Request) (commands.Result, error) {
			close(commandStarted)
			return commands.Result{Text: "deleted", Invalidation: &event}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	laterTargetStarted := make(chan struct{})
	responder := &fenceCheckingResponder{
		fakeResponder: &fakeResponder{}, broker: b, target: target,
		laterTargetStarted: laterTargetStarted,
	}
	bus := privacyruntime.NewBus()
	heldAtBus := false
	bus.Subscribe(func(privacyruntime.Event) {
		select {
		case <-laterTargetStarted:
		case <-time.After(30 * time.Millisecond):
			heldAtBus = true
		}
	})
	done := make(chan Outcome, 1)
	go func() {
		done <- Execute(Request{Principal: actor, SessionKey: "admin-session", Text: "/deleteuser target"}, Dependencies{Broker: b, Commands: service, Log: log, PrivacyBus: bus}, responder)
	}()
	select {
	case <-commandStarted:
		t.Fatal("target command overlapped active target work")
	case <-time.After(50 * time.Millisecond):
	}
	otherStarted := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), testPrincipal("other"), "other", func() error { close(otherStarted); return nil })
	}()
	select {
	case <-otherStarted:
	case <-time.After(time.Second):
		t.Fatal("target fence blocked unrelated user work")
	}
	close(releaseActive)
	select {
	case outcome := <-done:
		if outcome.Err != nil {
			t.Fatal(outcome.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("target command did not complete")
	}
	if !responder.heldAtSend || !heldAtBus {
		t.Fatalf("fence held at send=%t bus=%t", responder.heldAtSend, heldAtBus)
	}
	select {
	case <-laterTargetStarted:
	case <-time.After(time.Second):
		t.Fatal("target work did not resume after invalidation")
	}
}

type fakeResponder struct {
	started  bool
	cleaned  bool
	fallback string
	command  commands.Result
	agent    *agent.AgentResponse
	agentErr string
	sendErr  error
}

type fenceCheckingResponder struct {
	*fakeResponder
	broker             *broker.Broker
	target             identity.Principal
	laterTargetStarted chan struct{}
	heldAtSend         bool
}

func (r *fenceCheckingResponder) SendCommandResponse(result commands.Result) error {
	r.command = result
	go func() {
		_ = r.broker.RunInLane(context.Background(), r.target, "later", func() error {
			close(r.laterTargetStarted)
			return nil
		})
	}()
	select {
	case <-r.laterTargetStarted:
	case <-time.After(30 * time.Millisecond):
		r.heldAtSend = true
	}
	return r.sendErr
}

func (r *fakeResponder) StartProcessing() (func(), error) {
	r.started = true
	return func() { r.cleaned = true }, nil
}

func (r *fakeResponder) SendFallback(text string) error {
	r.fallback = text
	return nil
}

func (r *fakeResponder) SendCommandResponse(result commands.Result) error {
	r.command = result
	return r.sendErr
}

func (r *fakeResponder) SendAgentResponse(response *agent.AgentResponse) error {
	r.agent = response
	return r.sendErr
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

type fakeFormationEnqueuer struct {
	responder         *fakeResponder
	called            bool
	responseDelivered bool
	userID            string
	source            usermemory.FormationSource
}

type fakeCompactionEnqueuer struct {
	responder         *fakeResponder
	enqueueCalled     bool
	failureMarked     bool
	responseDelivered bool
	userID            string
	source            usermemory.FormationSource
}

type fakeGlobalMemoryPublisher struct {
	responder         *fakeResponder
	called            bool
	responseDelivered bool
	userID            string
	requestID         string
	turnID            int64
	discarded         bool
}

func (f *fakeGlobalMemoryPublisher) DiscardGlobalMemories(_ context.Context, userID, requestID string) error {
	f.discarded = true
	f.userID, f.requestID = userID, requestID
	return nil
}

func (f *fakeGlobalMemoryPublisher) PublishGlobalMemories(_ context.Context, userID, requestID string, turnID int64) (int, error) {
	f.called = true
	f.responseDelivered = f.responder.agent != nil && f.responder.sendErr == nil
	f.userID, f.requestID, f.turnID = userID, requestID, turnID
	return 1, nil
}

func (f *fakeCompactionEnqueuer) Enqueue(_ context.Context, userID string, source usermemory.FormationSource) error {
	f.enqueueCalled = true
	f.responseDelivered = f.responder.agent != nil && f.responder.sendErr == nil
	f.userID = userID
	f.source = source
	return nil
}

func (f *fakeCompactionEnqueuer) MarkDeliveryFailed(_ context.Context, userID string, turnID int64) error {
	f.failureMarked = true
	f.responseDelivered = false
	f.userID = userID
	f.source.TurnID = turnID
	return nil
}

func (f *fakeFormationEnqueuer) Enqueue(_ context.Context, userID string, source usermemory.FormationSource) error {
	f.called = true
	f.responseDelivered = f.responder.agent != nil
	f.userID = userID
	f.source = source
	return nil
}

type responseRuntimeProcessor struct{ response *agent.AgentResponse }

func (p responseRuntimeProcessor) Process(agent.Request) (*agent.AgentResponse, error) {
	return p.response, nil
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
	soulPath := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(soulPath, []byte("You are Oswald."), 0o600); err != nil {
		t.Fatalf("write soul fixture: %v", err)
	}
	soulStore := soul.NewStore(soulPath)
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
	ai := agent.NewAgent(runtimeFakeChatter{}, registry.New(log), "test-model", soulStore, memory, nil, promptbudget.ContextBudget{PromptLimit: 100000}, 3, time.Minute, log)
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
