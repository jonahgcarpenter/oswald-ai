package session_search

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func TestSearchHistoryBoundsRemoteResultWithoutCuttingFraming(t *testing.T) {
	content := governance.RemoteResultText("mcp__github__create_issue", strings.Repeat("界", 2000), 16000)
	source := memory.ToolHistory{Version: 1, Batches: []memory.ToolHistoryBatch{{Calls: []memory.ToolHistoryCall{{Name: "tool_call", ResultName: "mcp__github__create_issue", Result: content, Status: "succeeded"}}}}}
	bounded, truncated := boundedSearchHistory(source, 1000)
	call := bounded.Batches[0].Calls[0]
	if !truncated || !call.ResultTruncated || call.ResultName != "mcp__github__create_issue" || !strings.HasSuffix(call.Result, "\n</untrusted_tool_result>") || source.Batches[0].Calls[0].Result != content {
		t.Fatal("search truncation changed source history or cut remote framing")
	}
}
