package execcode

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Name is the model-facing tool name. The value is the persisted contract;
// only the owning package moved.
const Name = "execute_code"

// ExecuteCodeDefinition returns the stateless Python runner contract. Each
// call runs fresh with python3 in the profile working directory; variables,
// imports, and loaded data do not survive across calls. Persist state in
// workspace files when needed.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Run Python code statelessly with python3 in the profile working directory (CWD). Each call runs fresh from a temp file; variables, imports, and loaded data do not survive across calls — persist state in workspace files when needed. Returns exit_code with combined stdout/stderr, truncated past ~50KB with secrets redacted. 300s timeout, 200KB code limit. Use for computation and data transforms; use read_file/write_file/patch/search_files/terminal for workspace operations.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"code":  {Type: "string", Description: "Python code to execute. Runs stateless with python3 in the profile working directory; state does not persist across calls unless written to workspace files."},
			"reset": {Type: "boolean", Description: "Discard persisted helper state before running. Stateless runner: clears scratch __pycache__ entries.", Default: false},
		}, Required: []string{"code"}, AdditionalProperties: boolPtr(false)},
	}
}

func float64Ptr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }
