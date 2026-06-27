package mcp

import (
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFlattenToolResultTextStructuredErrorAndEmpty(t *testing.T) {
	text, err := flattenToolResult(&gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: " hello "}}})
	if err != nil {
		t.Fatalf("flatten text returned error: %v", err)
	}
	if text != "hello" {
		t.Fatalf("flatten text = %q, want hello", text)
	}

	structured, err := flattenToolResult(&gomcp.CallToolResult{StructuredContent: map[string]interface{}{"ok": true}})
	if err != nil {
		t.Fatalf("flatten structured returned error: %v", err)
	}
	if structured != `{"ok":true}` {
		t.Fatalf("flatten structured = %q", structured)
	}

	toolErr, err := flattenToolResult(&gomcp.CallToolResult{IsError: true})
	if err != nil {
		t.Fatalf("flatten error result returned error: %v", err)
	}
	if toolErr != "Error: MCP tool returned an unspecified error." {
		t.Fatalf("flatten error result = %q", toolErr)
	}

	empty, err := flattenToolResult(&gomcp.CallToolResult{})
	if err != nil {
		t.Fatalf("flatten empty returned error: %v", err)
	}
	if empty != "MCP tool returned no content." {
		t.Fatalf("flatten empty = %q", empty)
	}

	if _, err := flattenToolResult(nil); err == nil {
		t.Fatal("flatten nil result returned nil error")
	}
}

func TestFlattenToolResultTruncatesLongText(t *testing.T) {
	text, err := flattenToolResult(&gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: strings.Repeat("x", maxToolResultChars+10)}}})
	if err != nil {
		t.Fatalf("flatten long text returned error: %v", err)
	}
	if len([]rune(text)) != maxToolResultChars+len("\n\n[truncated]") {
		t.Fatalf("truncated length = %d", len([]rune(text)))
	}
	if !strings.HasSuffix(text, "\n\n[truncated]") {
		t.Fatalf("truncated text missing marker")
	}
}
