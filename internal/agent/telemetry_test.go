package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

type meteredTestChatter struct {
	fakeChatter
	t *testing.T
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
	got, exists := details["status"]
	if want == "ok" {
		if exists {
			t.Fatalf("success status emitted: %#v", details)
		}
		return
	}
	if got != want {
		t.Fatalf("status=%v, want %q", got, want)
	}
}

func (c *meteredTestChatter) Chat(ctx context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	meta := requestctx.MetadataFromContext(ctx)
	if meta.OperationID != "operation" || meta.ParentOperationID != "parent" || meta.Workload != "foreground" {
		c.t.Error("agent replaced inherited metadata")
	}
	requestctx.UsageCollectorFromContext(ctx).Record(requestctx.ModelUsage{Submitted: true, Status: "ok", UsageReported: true, TotalTokens: 10})
	return c.fakeChatter.Chat(ctx, req, cb)
}

func TestToolAndGenerationTelemetryDoesNotDoubleMeter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stderr
	os.Stderr = f
	log := config.NewLogger(config.LevelDebug)
	os.Stderr = old
	chat := &meteredTestChatter{t: t, fakeChatter: fakeChatter{responses: []*llm.ChatResponse{
		{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "private-call-id", Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{}}},
			{ID: "private-call-id-2", Function: llm.ToolFunction{Name: "private-tool-canary", Arguments: map[string]interface{}{}}},
		}}},
		{Message: llm.ChatMessage{Role: "assistant", Content: "private-response-canary"}},
	}}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := registerTestTool(t, reg, testToolSpec{Name: "test.lookup", Description: "Lookup"}, testToolPolicy(), func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
		m := requestctx.MetadataFromContext(ctx)
		if m.OperationID == "" || m.OperationID == "operation" || m.ParentOperationID != "operation" {
			t.Error("tool operation correlation missing")
		}
		return productiveResult("private-result-canary"), nil
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := newTestAgent(t, chat, nil, reg)
	a.log = log
	usage := requestctx.NewUsageCollector()
	ctx := requestctx.WithUsageCollector(requestctx.WithMetadata(context.Background(), requestctx.Metadata{OperationID: "operation", ParentOperationID: "parent", Workload: "foreground"}), usage)
	response, err := a.Process(ctx, Request{RequestID: "req", Principal: identity.Principal{CanonicalUserID: "user-1", ExternalID: "private-external-canary", Gateway: "imessage", Assurance: identity.AssuranceBlueBubblesWebhook}, SessionKey: "private-session-canary", Prompt: "private-input-canary"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Kind != "answer" || response.PersistenceStatus != "pending" {
		t.Fatalf("response telemetry: %+v", response)
	}
	if response.ToolExecutionCount != 1 || response.ToolBlockedCount != 1 {
		t.Fatalf("tool counters = %+v", response)
	}
	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "ToolExecutionCount") || strings.Contains(string(wire), "ToolBlockedCount") {
		t.Fatal("internal telemetry leaked into response JSON")
	}
	s := usage.Snapshot()
	if s.ModelCallCount != 2 || s.TotalTokens != 20 {
		t.Fatalf("double-counted usage: %+v", s)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-") {
		t.Fatal("private canary leaked into telemetry")
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		event := record["event"].(string)
		counts[event]++
		d := logDetails(t, record)
		if event == "agent.response.complete" && (d["iteration_count"] != float64(2) || d["tool_execution_count"] != float64(1)) {
			t.Fatalf("bad generation counters: %v", record)
		}
		if event == "tool.blocked" && d["tool_name"] != "unadvertised" {
			t.Fatalf("unsafe blocked name: %v", record)
		}
	}
	if counts["tool.completed"] != 1 || counts["tool.blocked"] != 1 || counts["agent.response.complete"] != 1 || counts["agent.tool.start"] != 1 {
		t.Fatalf("event counts: %v", counts)
	}
}

func TestToolCompletionPreservesActualOutcomeDuringCancellation(t *testing.T) {
	for _, tc := range []struct {
		name            string
		err             error
		status, outcome string
	}{
		{"success", nil, "ok", string(governance.OutcomeProductive)},
		{"failure", errors.New("private-error-canary"), "error", "error"},
		{"canceled", fmt.Errorf("private-error-canary: %w", context.Canceled), "ok", "canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "telemetry.log")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			old := os.Stderr
			os.Stderr = f
			log := config.NewLogger(config.LevelDebug)
			os.Stderr = old
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reg := registry.New(config.NewLogger(config.LevelError))
			if err := registerTestTool(t, reg, testToolSpec{Name: "test.lookup", Description: "Lookup"}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
				cancel()
				result := productiveResult("private-result-canary")
				result.ReasonCode = "test_result"
				return result, tc.err
			}); err != nil {
				t.Fatal(err)
			}
			chat := &fakeChatter{responses: []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "private-call-canary", Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{}}}}}}}}
			a, _ := newTestAgent(t, chat, nil, reg)
			a.log = log
			_, err = a.Process(ctx, Request{RequestID: "req", Principal: identity.Principal{CanonicalUserID: "user-1", ExternalID: "user-1", Gateway: "imessage", Assurance: identity.AssuranceBlueBubblesWebhook}, SessionKey: "session", Prompt: "question"})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("process err = %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "private-") {
				t.Fatal("private canary leaked")
			}
			count := 0
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["event"] != "tool.completed" {
					continue
				}
				count++
				d := logDetails(t, record)
				checkStatus(t, d, tc.status)
				if d["outcome"] != tc.outcome || d["reason_code"] != "test_result" {
					t.Fatalf("completion = %v", record)
				}
				if tc.err != nil && d["error_code"] != config.ErrorCode(tc.err) {
					t.Fatalf("error code missing: %v", record)
				}
			}
			if count != 1 {
				t.Fatalf("completion count = %d", count)
			}
		})
	}
}
