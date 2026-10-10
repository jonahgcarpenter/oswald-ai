package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func telemetryLogger(t *testing.T) (*config.Logger, func() []map[string]any) {
	return telemetryLoggerAtLevel(t, config.LevelDebug)
}

func telemetryLoggerAtLevel(t *testing.T, level config.Level) (*config.Logger, func() []map[string]any) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telemetry.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	old := os.Stderr
	os.Stderr = f
	log := config.NewLogger(level)
	os.Stderr = old
	return log, func() []map[string]any {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{"private-session-canary", "private-chat-canary", "external-private-canary", "private-input-canary", "private-error-canary"} {
			if strings.Contains(string(data), canary) {
				t.Fatalf("log leaked %s", canary)
			}
		}
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatal(err)
			}
			records = append(records, record)
		}
		return records
	}
}

func logDetails(t *testing.T, record map[string]any) map[string]any {
	t.Helper()
	details, ok := record["details"].(map[string]any)
	if !ok {
		t.Fatalf("missing details object: %#v", record)
	}
	return details
}

// checkStatus asserts a details status: success is implicit (absent), every
// other status is emitted verbatim.
func checkStatus(t *testing.T, details map[string]any, want string) {
	t.Helper()
	checkImplicitDetail(t, details, "status", want, "ok")
}

// checkImplicitDetail asserts a details value that is omitted when it equals
// the implicit success value.
func checkImplicitDetail(t *testing.T, details map[string]any, key, want, implicit string) {
	t.Helper()
	got, exists := details[key]
	if want == implicit {
		if exists {
			t.Fatalf("%s emitted for implicit %q: %#v", key, want, details)
		}
		return
	}
	if got != want {
		t.Fatalf("%s=%v, want %q", key, got, want)
	}
}

type telemetryProcessor struct {
	err             error
	unreportedCalls int
}

func (p telemetryProcessor) Process(ctx context.Context, req agent.Request) (*agent.Response, error) {
	m := requestctx.MetadataFromContext(ctx)
	principal, _ := requestctx.PrincipalFromContext(ctx)
	if m.RequestID != req.RequestID || m.Workload != "foreground" || principal.CanonicalUserID != req.Principal.CanonicalUserID {
		return nil, errors.New("missing request metadata")
	}
	collector := requestctx.UsageCollectorFromContext(ctx)
	if collector == nil {
		return nil, errors.New("missing collector")
	}
	collector.Record(requestctx.ModelUsage{Submitted: true, Status: "ok", UsageReported: true, PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10})
	for i := 0; i < p.unreportedCalls; i++ {
		collector.Record(requestctx.ModelUsage{Submitted: true, Status: "ok"})
	}
	return &agent.Response{Response: "answer", Model: "configured-model", ToolExecutionCount: 2, ToolBlockedCount: 1}, p.err
}

func TestRequestTelemetryClassificationAndUsage(t *testing.T) {
	for _, tc := range []struct {
		name, text, promptType string
		images                 []llm.InputImage
		unsupported            []string
		reply                  *routing.ReplyContext
	}{
		{name: "text", text: "private-input-canary", promptType: "text"},
		{name: "image", images: []llm.InputImage{{}}, promptType: "image"},
		{name: "mixed", text: "private-input-canary", images: []llm.InputImage{{}}, promptType: "text_image"},
		{name: "unsupported", unsupported: []string{"file"}, promptType: "unsupported"},
		{name: "empty", promptType: "empty"},
		{name: "reply-image", text: "private-input-canary", reply: &routing.ReplyContext{Images: []llm.InputImage{{MimeType: "image/png", Data: "synthetic"}}}, promptType: "text"},
		{name: "reply-text", images: []llm.InputImage{{}}, reply: &routing.ReplyContext{Text: "quoted"}, promptType: "image"},
		{name: "unknown-command", text: "/private-input-canary", promptType: "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, records := telemetryLogger(t)
			b := broker.NewBroker(telemetryProcessor{}, 1, log)
			b.Start()
			defer b.Shutdown()
			p := testPrincipal("canonical-user")
			p.ExternalID = "external-private-canary"
			out := Execute(Request{RequestID: "request", Principal: p, ChatID: "private-chat-canary", SessionKey: "private-session-canary", Text: tc.text, Images: tc.images, Unsupported: tc.unsupported, Reply: tc.reply, ReceivedAt: time.Now().Add(-time.Second)}, Dependencies{Log: log, Broker: b}, &fakeResponder{})
			var complete map[string]any
			received, completed := 0, 0
			for _, record := range records() {
				switch record["event"] {
				case "gateway.request.received":
					received++
					if logDetails(t, record)["is_admitted"] != true {
						t.Fatalf("receipt = %v", record)
					}
				case "gateway.request.complete":
					completed++
					complete = record
				}
			}
			cd := logDetails(t, complete)
			if completed != 1 || cd["prompt_type"] != tc.promptType || cd["duration_ms"].(float64) < 1000 {
				t.Fatalf("bad terminal telemetry: %v", complete)
			}
			if cd["image_count"] != float64(len(tc.images)) {
				t.Fatalf("enrichment changed input image count: %v", complete)
			}
			if tc.name == "reply-image" && cd["model_image_count"] != float64(1) {
				t.Fatalf("missing enriched image count: %v", complete)
			}
			wantReceived := 1
			if out.Action == routing.ActionGatewayFallback {
				wantReceived = 0
			}
			if received != wantReceived {
				t.Fatalf("received count = %d", received)
			}
			if cd["is_admitted"] != (wantReceived == 1) {
				t.Fatalf("summary = %v", complete)
			}
			if out.Action == routing.ActionLLM {
				if cd["model"] != "configured-model" || cd["request_tool_execution_count"] != float64(2) || cd["request_tool_blocked_count"] != float64(1) || cd["is_request_usage_reported"] != true || cd["request_usage_unknown_call_count"] != float64(0) {
					t.Fatalf("summary counters = %v", complete)
				}
			}
			if out.Action == routing.ActionLLM && (cd["request_model_call_count"] != float64(1) || cd["request_total_tokens"] != float64(10)) {
				t.Fatalf("usage not propagated exactly once: %v", complete)
			}
		})
	}
}

func TestRequestTelemetryQueueFullAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		log, records := telemetryLogger(t)
		b := broker.NewBroker(nil, 1, log)
		if shutdown {
			b.Shutdown()
		} else {
			for i := 0; i < b.Snapshot().Capacity; i++ {
				if err := b.Submit(&broker.Request{Principal: testPrincipal("user")}); err != nil {
					t.Fatal(err)
				}
			}
		}
		r := &fakeResponder{}
		Execute(Request{RequestID: "rejected", Principal: testPrincipal("user"), Text: "hello"}, Dependencies{Broker: b, Log: log}, r)
		b.Shutdown()
		count := 0
		for _, record := range records() {
			if record["event"] != "gateway.request.complete" {
				continue
			}
			count++
			want := "rejected"
			if shutdown {
				want = "canceled"
			}
			if logDetails(t, record)["execution_status"] != want {
				t.Fatalf("shutdown=%v terminal=%v", shutdown, record)
			}
		}
		if count != 1 {
			t.Fatalf("terminal count = %d", count)
		}
		if shutdown && (!r.canceled || r.agentErr != "" || r.agent != nil) {
			t.Fatalf("shutdown sent generic failure: %+v", r)
		}
	}
}

func TestRequestTerminalCancellationAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status string
		errors int
	}{
		{"cancel", context.Canceled, "ok", 0},
		{"stop", broker.ErrAgentWorkCanceled, "ok", 0},
		{"error", errors.New("private-error-canary"), "error", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, records := telemetryLogger(t)
			b := broker.NewBroker(telemetryProcessor{err: tc.err}, 1, log)
			b.Start()
			defer b.Shutdown()
			r := &fakeResponder{}
			Execute(Request{RequestID: "req", Principal: testPrincipal("canonical-user"), Text: "hello"}, Dependencies{Broker: b, Log: log}, r)
			completed, errorCount := 0, 0
			for _, record := range records() {
				if record["event"] == "gateway.request.failed" && record["level"] == "warn" {
					errorCount++
				}
				if record["event"] == "gateway.request.complete" {
					completed++
					d := logDetails(t, record)
					checkStatus(t, d, tc.status)
					if tc.name == "stop" && d["reason_code"] != "stop" {
						t.Fatalf("stop reason = %v", record)
					}
					if tc.name == "cancel" && d["reason_code"] != "shutdown" {
						t.Fatalf("shutdown reason = %v", record)
					}
				}
			}
			if completed != 1 || errorCount != tc.errors {
				t.Fatalf("complete=%d errors=%d", completed, errorCount)
			}
			if tc.status == "ok" && (r.agentErr != "" || !r.canceled) {
				t.Fatalf("cancellation sent failure: %+v", r)
			}
		})
	}
}

func TestCommandMutationTelemetryAndContext(t *testing.T) {
	log, records := telemetryLogger(t)
	svc, err := commands.NewServiceWithCommands(commands.Command{Handler: commands.HandlerFunc{
		DefinitionValue: commands.Definition{Name: "mutate"},
		ExecuteFunc: func(ctx context.Context, _ commands.Request) (commands.Result, error) {
			if requestctx.MetadataFromContext(ctx).RequestID != "req" || requestctx.UsageCollectorFromContext(ctx) == nil {
				t.Error("command lost telemetry context")
			}
			return commands.Result{Outcome: commands.Outcome{Operation: "test.mutate", IsChanged: true, AffectedCount: 1}}, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	Execute(Request{RequestID: "req", Principal: testPrincipal("user"), Text: "/mutate"}, Dependencies{Log: log, Commands: svc}, &fakeResponder{})
	count := 0
	for _, record := range records() {
		if record["event"] == "gateway.command.mutation.complete" {
			count++
			d := logDetails(t, record)
			if d["affected_count"] != float64(1) {
				t.Fatalf("mutation=%v", record)
			}
			checkStatus(t, d, "ok")
		}
	}
	if count != 1 {
		t.Fatalf("mutation count = %d", count)
	}
}

func TestRejectedAdmissionHasOnlyTerminalSummary(t *testing.T) {
	for _, text := range []string{"hello", "/ping"} {
		for _, invalidGateway := range []bool{false, true} {
			log, records := telemetryLogger(t)
			p := testPrincipal("user")
			if invalidGateway {
				p.CanonicalUserID = "private-input-canary"
				p.Gateway = "private-error-canary"
			} else {
				p.Assurance = identity.AssuranceSelfAsserted
			}
			Execute(Request{Principal: p, Text: text}, Dependencies{Log: log}, &fakeResponder{})
			received, completed := 0, 0
			for _, record := range records() {
				if record["event"] == "gateway.request.received" {
					received++
				}
				if record["event"] == "gateway.request.complete" {
					completed++
					d := logDetails(t, record)
					if _, ok := d["user_id"]; ok {
						t.Fatalf("untrusted user field: %v", record)
					}
					if d["is_admitted"] != false || d["status"] != "rejected" {
						t.Fatalf("rejection = %v", record)
					}
				}
			}
			if received != 0 || completed != 1 {
				t.Fatalf("received=%d completed=%d", received, completed)
			}
		}
	}
}

func TestProviderErrorHasSafeRootDiagnostic(t *testing.T) {
	log, records := telemetryLogger(t)
	b := broker.NewBroker(responseRuntimeProcessor{response: &agent.Response{Model: "configured-model", Error: "private-error-canary"}}, 1, log)
	b.Start()
	defer b.Shutdown()
	Execute(Request{Principal: testPrincipal("user"), Text: "hello"}, Dependencies{Log: log, Broker: b}, &fakeResponder{})
	count := 0
	for _, record := range records() {
		if record["level"] == "warn" {
			count++
			if record["event"] != "gateway.request.failed" || logDetails(t, record)["error_code"] != "model_failure" {
				t.Fatalf("diagnostic = %v", record)
			}
		}
		if record["event"] == "gateway.request.complete" {
			d := logDetails(t, record)
			if d["is_request_usage_reported"] != false || d["model"] != "configured-model" {
				t.Fatalf("summary = %v", record)
			}
		}
	}
	if count != 1 {
		t.Fatalf("error count = %d", count)
	}
}

func TestRequestUsageReportsObservedRatherThanCompleteTotals(t *testing.T) {
	log, records := telemetryLogger(t)
	b := broker.NewBroker(telemetryProcessor{unreportedCalls: 2}, 1, log)
	b.Start()
	defer b.Shutdown()
	Execute(Request{Principal: testPrincipal("user"), Text: "hello"}, Dependencies{Log: log, Broker: b}, &fakeResponder{})
	count := 0
	for _, record := range records() {
		if record["event"] != "gateway.request.complete" {
			continue
		}
		count++
		d := logDetails(t, record)
		if d["is_request_usage_reported"] != true || d["request_usage_unknown_call_count"] != float64(2) || d["request_usage_reported_call_count"] != float64(1) || d["request_model_submission_count"] != float64(3) || d["request_total_tokens"] != float64(10) {
			t.Fatalf("usage summary = %v", record)
		}
	}
	if count != 1 {
		t.Fatalf("summary count = %d", count)
	}
}

type lateUsageProcessor struct {
	started, release chan struct{}
	failure          error
}

func (p lateUsageProcessor) Process(ctx context.Context, _ agent.Request) (*agent.Response, error) {
	u := requestctx.UsageCollectorFromContext(ctx)
	u.Record(requestctx.ModelUsage{Submitted: true, Status: "ok", UsageReported: true, UsageComplete: true, TotalTokens: 10})
	u.SetExecution(requestctx.ExecutionSnapshot{Model: "configured-model", ToolExecutionCount: 1, PersistenceStatus: "unknown"})
	close(p.started)
	<-p.release
	u.Record(requestctx.ModelUsage{Submitted: true, Status: "ok", UsageReported: true, UsageComplete: true, TotalTokens: 20})
	if p.failure != nil {
		u.SetExecution(requestctx.ExecutionSnapshot{Model: "configured-model", ToolExecutionCount: 1, PersistenceStatus: "failed", ResponseKind: "error", IsComplete: true})
		return nil, p.failure
	}
	u.SetExecution(requestctx.ExecutionSnapshot{Model: "configured-model", ToolExecutionCount: 1, PersistenceStatus: "not_attempted", ResponseKind: "canceled", IsComplete: true})
	return nil, ctx.Err()
}

func TestImmediateStopSummaryAndLateExecutionAccounting(t *testing.T) {
	storageErr := sqlite3.Error{Code: sqlite3.ErrIoErr}
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{name: "cancellation_only"},
		{name: "late_storage_error", failure: storageErr},
		{name: "storage_joined_with_cancellation", failure: errors.Join(context.Canceled, storageErr)},
		{name: "storage_joined_with_stop", failure: errors.Join(broker.ErrAgentWorkCanceled, storageErr)},
		{name: "independent_error", failure: errors.New("private-error-canary")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, records := telemetryLogger(t)
			p := lateUsageProcessor{started: make(chan struct{}), release: make(chan struct{}), failure: tc.failure}
			b := broker.NewBroker(p, 1, log)
			b.Start()
			defer func() {
				select {
				case <-p.release:
				default:
					close(p.release)
				}
				b.Shutdown()
			}()
			r := &fakeResponder{}
			done := make(chan Outcome, 1)
			go func() {
				done <- Execute(Request{RequestID: "late", Principal: testPrincipal("user"), Text: "hello"}, Dependencies{Log: log, Broker: b}, r)
			}()
			<-p.started
			b.CancelAllAgentWork()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("stop waited on stalled provider")
			}
			if !r.canceled {
				t.Fatal("transport cleanup not immediate")
			}
			gatewayCount, brokerCount := 0, 0
			for _, record := range records() {
				if record["event"] == "broker.request.execution.complete" {
					brokerCount++
				}
				if record["event"] == "gateway.request.complete" {
					gatewayCount++
					d := logDetails(t, record)
					if d["outcome"] != "canceled" {
						t.Fatalf("gateway cancellation = %v", record)
					}
					checkStatus(t, d, "ok")
					if d["is_execution_complete"] != false || d["is_request_usage_complete"] != false || d["persistence_status"] != "unknown" || d["request_total_tokens"] != float64(10) || d["request_tool_execution_count"] != float64(1) {
						t.Fatalf("early summary = %v", record)
					}
				}
			}
			if gatewayCount != 1 || brokerCount != 0 {
				t.Fatalf("early events: gateway=%d broker=%d", gatewayCount, brokerCount)
			}
			close(p.release)
			b.Shutdown()
			for _, record := range records() {
				if record["event"] != "broker.request.execution.complete" {
					continue
				}
				brokerCount++
				d := logDetails(t, record)
				if d["is_execution_complete"] != true || d["is_execution_usage_complete"] != true || d["execution_total_tokens"] != float64(30) || d["execution_model_call_count"] != float64(2) || d["execution_tool_count"] != float64(1) {
					t.Fatalf("final execution = %v", record)
				}
				if tc.failure != nil {
					if d["status"] != "error" || d["outcome"] != "error" || d["persistence_status"] != "failed" || d["error_code"] != config.ErrorCode(tc.failure) {
						t.Fatalf("late failure masked by cancellation: %v", record)
					}
				} else if d["outcome"] != "canceled" {
					t.Fatalf("cancellation-only execution = %v", record)
				} else {
					checkStatus(t, d, "ok")
				}
			}
			if brokerCount != 1 {
				t.Fatalf("final execution count = %d", brokerCount)
			}
		})
	}
}

type failedExecutionProcessor struct{}

func (failedExecutionProcessor) Process(ctx context.Context, _ agent.Request) (*agent.Response, error) {
	requestctx.UsageCollectorFromContext(ctx).SetExecution(requestctx.ExecutionSnapshot{Model: "configured-model", ToolExecutionCount: 1, BlockedCount: 2, PersistenceStatus: "failed", ResponseKind: "error", IsComplete: true})
	return nil, errors.New("private-error-canary")
}

func TestRequestFailureRetainsExecutionStatsWithoutResponse(t *testing.T) {
	log, records := telemetryLogger(t)
	b := broker.NewBroker(failedExecutionProcessor{}, 1, log)
	b.Start()
	defer b.Shutdown()
	Execute(Request{Principal: testPrincipal("user"), Text: "hello"}, Dependencies{Log: log, Broker: b}, &fakeResponder{})
	count := 0
	for _, record := range records() {
		if record["event"] != "gateway.request.complete" {
			continue
		}
		count++
		d := logDetails(t, record)
		if d["request_tool_execution_count"] != float64(1) || d["request_tool_blocked_count"] != float64(2) || d["persistence_status"] != "failed" || d["is_execution_complete"] != true || d["model"] != "configured-model" {
			t.Fatalf("failure summary = %v", record)
		}
	}
	if count != 1 {
		t.Fatalf("summary count = %d", count)
	}
}

func TestCommittedCommandFailureStillAuditsMutation(t *testing.T) {
	log, records := telemetryLogger(t)
	svc, err := commands.NewServiceWithCommands(commands.Command{Handler: commands.HandlerFunc{
		DefinitionValue: commands.Definition{Name: "mutate"},
		ExecuteFunc: func(context.Context, commands.Request) (commands.Result, error) {
			return commands.Result{Outcome: commands.Outcome{Operation: "test.mutate", IsChanged: true, AffectedCount: 1}}, errors.New("private-error-canary")
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	Execute(Request{Principal: testPrincipal("user"), Text: "/mutate"}, Dependencies{Log: log, Commands: svc}, &fakeResponder{})
	count := 0
	for _, record := range records() {
		if record["event"] == "gateway.command.mutation.complete" {
			count++
			d := logDetails(t, record)
			if d["status"] != "error" || d["is_changed"] != true {
				t.Fatalf("mutation = %v", record)
			}
		}
	}
	if count != 1 {
		t.Fatalf("mutation count = %d", count)
	}
}
