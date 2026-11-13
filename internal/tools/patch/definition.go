package patch

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Name is the model-facing tool name. The value is the persisted contract;
// only the owning package moved.
const Name = "patch"

// Definition returns the targeted-edit contract.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Targeted find-and-replace edits in files. Use this instead of sed/awk in terminal. Uses fuzzy matching (9 strategies) so minor whitespace/indentation differences won't break it. Returns a unified diff. Auto-runs syntax checks after editing. Finds a unique string and replaces it.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"path":        {Type: "string", Description: "File path to edit."},
			"old_string":  {Type: "string", Description: "Exact text to find and replace. Must be unique in the file unless replace_all=true. Include surrounding context lines to ensure uniqueness."},
			"new_string":  {Type: "string", Description: "Changed replacement text; it must differ from old_string. Pass empty string '' to delete the matched text."},
			"replace_all": {Type: "boolean", Description: "Replace all occurrences instead of requiring a unique match (default: false)", Default: false},
		}, Required: []string{"path", "old_string", "new_string"}, AdditionalProperties: boolPtr(false)},
	}
}

func float64Ptr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }
