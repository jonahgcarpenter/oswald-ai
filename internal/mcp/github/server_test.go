package githubmcp

import (
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestIsReadOnlyToolFiltersNilAndMutatingTools(t *testing.T) {
	if IsReadOnlyTool(nil) {
		t.Fatal("nil tool considered read-only")
	}
	if !IsReadOnlyTool(&gomcp.Tool{Name: "get_issue", Description: "Read an issue"}) {
		t.Fatal("get_issue should be read-only")
	}
	mutating := []string{"create_issue", "update_pull_request", "delete_file", "merge_pull_request", "add_comment"}
	for _, name := range mutating {
		if IsReadOnlyTool(&gomcp.Tool{Name: name}) {
			t.Fatalf("%s considered read-only", name)
		}
	}
	if IsReadOnlyTool(&gomcp.Tool{Name: "workflow", Description: "Trigger workflow run"}) {
		t.Fatal("trigger workflow tool considered read-only")
	}
}
