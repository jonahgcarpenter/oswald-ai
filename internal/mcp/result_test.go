package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSafeMCPRequestErrorCategoriesDoNotLeakRemoteText(t *testing.T) {
	for _, test := range []struct {
		err      error
		category string
	}{
		{context.Canceled, "category: canceled"},
		{context.DeadlineExceeded, "category: timeout"},
		{fmt.Errorf("remote secret detail"), "category: transport"},
	} {
		got := safeMCPRequestError(test.err).Error()
		if !strings.Contains(got, test.category) || strings.Contains(got, "remote secret") {
			t.Fatalf("unsafe categorized error: %q", got)
		}
	}
}

func TestFlattenToolResultTextStructuredErrorAndEmpty(t *testing.T) {
	text, err := flattenToolResult(&gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: " hello "}}})
	if err != nil {
		t.Fatalf("flatten text returned error: %v", err)
	}
	if text.Outcome != governance.OutcomeProductive {
		t.Fatalf("flatten text outcome = %q, want productive", text.Outcome)
	}
	if text.Content != "hello" {
		t.Fatalf("flatten text = %q", text.Content)
	}

	structured, err := flattenToolResult(&gomcp.CallToolResult{StructuredContent: map[string]interface{}{"ok": true}})
	if err != nil {
		t.Fatalf("flatten structured returned error: %v", err)
	}
	if structured.Outcome != governance.OutcomeProductive {
		t.Fatalf("flatten structured outcome = %q, want productive", structured.Outcome)
	}
	if structured.Content != `{"ok":true}` {
		t.Fatalf("flatten structured = %q", structured.Content)
	}

	toolErr, err := flattenToolResult(&gomcp.CallToolResult{IsError: true})
	if err == nil || err.Error() != "MCP tool reported failure (category: remote_tool). Review the arguments before retrying" || toolErr.Content != "" || toolErr.Outcome != "" {
		t.Fatalf("flatten error result = %+v, %v", toolErr, err)
	}
	remoteErr, err := flattenToolResult(&gomcp.CallToolResult{IsError: true, Content: []gomcp.Content{&gomcp.TextContent{Text: "remote secret failure detail"}}})
	if err == nil || strings.Contains(err.Error(), "remote secret") || remoteErr.Content != "" || remoteErr.Outcome != "" {
		t.Fatalf("remote error leaked: result=%+v err=%v", remoteErr, err)
	}

	empty, err := flattenToolResult(&gomcp.CallToolResult{})
	if err != nil {
		t.Fatalf("flatten empty returned error: %v", err)
	}
	if empty.Outcome != governance.OutcomeUnproductive {
		t.Fatalf("flatten empty outcome = %q, want unproductive", empty.Outcome)
	}
	if empty.Content != "MCP tool returned no content." {
		t.Fatalf("flatten empty = %q", empty.Content)
	}

	if _, err := flattenToolResult(nil); err == nil || !strings.Contains(err.Error(), "category: result_format") {
		t.Fatal("flatten nil result returned nil error")
	}
}

func TestFlattenToolResultTruncatesLongText(t *testing.T) {
	text, err := flattenToolResult(&gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: strings.Repeat("x", maxToolResultChars+10)}}})
	if err != nil {
		t.Fatalf("flatten long text returned error: %v", err)
	}
	if text.Outcome != governance.OutcomeProductive {
		t.Fatalf("flatten long text outcome = %q, want productive", text.Outcome)
	}
	if len([]rune(text.Content)) > maxToolResultChars {
		t.Fatalf("truncated length = %d", len([]rune(text.Content)))
	}
	if !strings.HasSuffix(text.Content, "…[truncated]") {
		t.Fatalf("truncation marker missing")
	}
}
