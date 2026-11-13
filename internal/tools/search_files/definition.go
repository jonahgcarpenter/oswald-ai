package searchfiles

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Name is the model-facing tool name. The value is the persisted contract;
// only the owning package moved.
const Name = "search_files"

// SearchFilesDefinition returns the ripgrep-backed search contract.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Search file contents or find files by name. Use this instead of grep/rg/find/ls in terminal. Ripgrep-backed, faster than shell equivalents. On macOS, broad searches above the user home automatically skip TCC-protected folders (Desktop, Documents, Downloads, Library, Movies, Music, Pictures); target one directly when access is intentional.\n\nContent search (target='content'): Regex search inside files. Output modes: full matches with line numbers, file paths only, or match counts.\n\nFile search (target='files'): Find files by glob pattern (e.g., '*.py', '*config*'). Also use this instead of ls. Discovery order is the fast bounded default; exact global newest-first order is an explicit opt-in and may scan the full tree.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"pattern":     {Type: "string", Description: "Regex pattern for content search, or glob pattern (e.g., '*.py') for file search"},
			"target":      {Type: "string", Description: "'content' searches inside file contents, 'files' searches for files by name", Enum: []string{"content", "files"}, Default: "content"},
			"path":        {Type: "string", Description: "Directory or file to search in (default: current working directory)", Default: "."},
			"file_glob":   {Type: "string", Description: "Filter files by pattern in grep mode (e.g., '*.py' to only search Python files)"},
			"limit":       {Type: "integer", Description: "Maximum number of results to return (default: 50)", Default: 50, Minimum: float64Ptr(1)},
			"offset":      {Type: "integer", Description: "Skip first N results for pagination (default: 0)", Default: 0, Minimum: float64Ptr(0)},
			"order":       {Type: "string", Description: "File-search order: 'discovery' is fast bounded traversal order; 'modified' is exact global newest-first and may scan the full tree; ignored for content", Enum: []string{"discovery", "modified"}, Default: "discovery"},
			"output_mode": {Type: "string", Description: "Output format for grep mode: 'content' shows matching lines with line numbers, 'files_only' lists file paths, 'count' shows match counts per file", Enum: []string{"content", "files_only", "count"}, Default: "content"},
			"context":     {Type: "integer", Description: "Number of context lines before and after each match (grep mode only)", Default: 0, Minimum: float64Ptr(0)},
		}, Required: []string{"pattern"}, AdditionalProperties: boolPtr(false)},
	}
}

func float64Ptr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }
