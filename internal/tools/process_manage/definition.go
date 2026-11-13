package processmanage

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Name is the model-facing tool name. The value is the persisted contract;
// only the owning package moved.
const Name = "process_manage"

// Definition returns the background-process companion contract.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Manage tracked background terminal processes: list sessions, poll status, read logs, wait for completion, kill, write/submit stdin, close stdin, or hand off ownership to the parent scope. Session IDs accept unique prefixes.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"action":     {Type: "string", Description: "list|poll|log|wait|kill|write|submit|close|handoff"},
			"session_id": {Type: "string", Description: "Process session ID (unique prefix accepted)"},
			"data":       {Type: "string", Description: "Stdin bytes for write/submit, purpose sentence for handoff"},
			"timeout":    {Type: "integer", Description: "Wait timeout in seconds", Minimum: float64Ptr(1)},
			"offset":     {Type: "integer", Description: "Log start line (default 0)", Minimum: float64Ptr(0)},
			"limit":      {Type: "integer", Description: "Log max lines (default 200)", Minimum: float64Ptr(1)},
		}, Required: []string{"action"}, AdditionalProperties: boolPtr(false)},
	}
}

func float64Ptr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }
