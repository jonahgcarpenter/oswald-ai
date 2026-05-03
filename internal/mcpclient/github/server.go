package githubmcp

import (
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func IsReadOnlyTool(tool *gomcp.Tool) bool {
	if tool == nil {
		return false
	}
	joined := strings.ToLower(tool.Name + " " + tool.Description + " " + tool.Title)
	unsafeTokens := []string{
		"create", "update", "delete", "remove", "set ", "set_", "request", "assign", "trigger",
		"run_", "run ", "rerun", "cancel", "dismiss", "submit", "merge", "close", "reopen",
		"comment", "fork", "star", "unstar", "lock", "unlock", "pin", "unpin", "transfer",
		"archive", "unarchive", "add ", "add_", "edit", "write", "upload", "download_workflow_run_artifact",
	}
	for _, token := range unsafeTokens {
		if strings.Contains(joined, token) {
			return false
		}
	}
	return true
}
