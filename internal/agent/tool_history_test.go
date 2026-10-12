package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func TestBridgeHistoryPreservesInvocationAndResultNames(t *testing.T) {
	content := governance.RemoteResultText("mcp__github__create_issue", strings.Repeat("x", 2000), 16000)
	call := persistedToolCall(llm.ToolCall{ID: "call_3", Function: llm.ToolFunction{Name: "tool_call", Arguments: map[string]interface{}{"calls": []interface{}{map[string]interface{}{"name": "mcp__github__create_issue"}}}}},
		governance.HistoryPolicy{Mode: governance.HistoryFull, MaxArgumentBytes: 16000, MaxResultRunes: 1000},
		governance.Decision{Allowed: true}, governance.Result{ResultName: "mcp__github__create_issue", Content: content, Outcome: governance.OutcomeProductive}, nil, content, time.Now())
	if call.Name != "tool_call" || call.ResultName != "mcp__github__create_issue" || call.ProviderCallID != "call_3" || !call.ResultTruncated || !strings.HasSuffix(call.Result, "\n</untrusted_tool_result>") {
		t.Fatalf("history lost identity or framing: %+v", call)
	}
}
