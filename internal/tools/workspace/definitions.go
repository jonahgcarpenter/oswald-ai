package workspace

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// Tool names exposed to the model.
const (
	ReadFileName      = "read_file"
	WriteFileName     = "write_file"
	PatchName         = "patch"
	SearchFilesName   = "search_files"
	TerminalName      = "terminal"
	ExecuteCodeName   = "execute_code"
	ProcessManageName = "process_manage"
	CronjobManageName = "cronjob_manage"
)

var ocrAvailable bool

// SetOCRAvailable toggles the hosted-OCR description override. It is a
// process-wide capability flag set once at startup; tests reset it.
func SetOCRAvailable(available bool) { ocrAvailable = available }

// OCRAvailable reports the hosted-OCR capability flag.
func OCRAvailable() bool { return ocrAvailable }

// DynamicDescription returns the per-request description for a workspace
// tool, applying capability overrides to the static definition text.
func DynamicDescription(name, base string) string {
	if name == ReadFileName && ocrAvailable {
		return replaceFirst(base, "PDF (text layer)", "PDF (scanned or text)")
	}
	return base
}

func replaceFirst(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}

// Definitions returns the workspace tool contracts in stable order.
func Definitions() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		ReadFileDefinition(),
		WriteFileDefinition(),
		PatchDefinition(),
		SearchFilesDefinition(),
		TerminalDefinition(),
		ExecuteCodeDefinition(),
		ProcessManageDefinition(),
		CronjobManageDefinition(),
	}
}

// ReadFileDefinition returns the paginated text-file reader contract.
func ReadFileDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        ReadFileName,
		Description: "Read a text file with line numbers and pagination. Use this instead of cat/head/tail in terminal. Output format: 'LINE_NUM|CONTENT'. Suggests similar filenames if not found. Use offset and limit for large files. Reads exceeding ~100K characters are truncated on a line boundary and return a next_offset; continue with offset to read the rest. Documents auto-extract to readable text: .ipynb, Office (.docx/.xlsx/.pptx and legacy .doc/.ppt/.xls), PDF (text layer), OpenDocument, RTF, EPUB, SQLite (.db/.sqlite: schema, row counts, first rows). Cannot read images/binary — use vision_analyze for images.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"path":   {Type: "string", Description: "Path to the file to read (absolute, relative, or ~/path)"},
			"offset": {Type: "integer", Description: "Line number to start reading from (1-indexed, default: 1)", Default: 1, Minimum: float64Ptr(1)},
			"limit":  {Type: "integer", Description: "Maximum number of lines to read (default: 2000, max: 2000). Reads are additionally capped at a ~100K-character budget with a next_offset continuation.", Default: 2000, Minimum: float64Ptr(1), Maximum: float64Ptr(2000)},
		}, Required: []string{"path"}, AdditionalProperties: boolPtr(false)},
	}
}

// WriteFileDefinition returns the whole-file writer contract.
func WriteFileDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        WriteFileName,
		Description: "Write content to a file, completely replacing existing content. Use this instead of echo/cat heredoc in terminal. Creates parent directories automatically. OVERWRITES the entire file — use 'patch' for targeted edits. For an EXISTING file, call read_file first: write_file refuses (file untouched) when this task has no current full read/write of the file or the file changed on disk since; on refusal, read_file, merge, then retry. Auto-runs syntax checks on .py/.json/.yaml/.toml and other linted languages; only NEW errors introduced by this write are surfaced (pre-existing errors are filtered out). The result's verified:true means the on-disk content hash was confirmed — do NOT re-read the file to check the write landed.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"path":    {Type: "string", Description: "Path to the file to write (will be created if it doesn't exist, overwritten if it does)"},
			"content": {Type: "string", Description: "Complete content to write to the file"},
		}, Required: []string{"path", "content"}, AdditionalProperties: boolPtr(false)},
	}
}

// PatchDefinition returns the targeted-edit contract.
func PatchDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        PatchName,
		Description: "Targeted find-and-replace edits in files. Use this instead of sed/awk in terminal. Uses fuzzy matching (9 strategies) so minor whitespace/indentation differences won't break it. Returns a unified diff. Auto-runs syntax checks after editing. Finds a unique string and replaces it.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"path":        {Type: "string", Description: "File path to edit."},
			"old_string":  {Type: "string", Description: "Exact text to find and replace. Must be unique in the file unless replace_all=true. Include surrounding context lines to ensure uniqueness."},
			"new_string":  {Type: "string", Description: "Changed replacement text; it must differ from old_string. Pass empty string '' to delete the matched text."},
			"replace_all": {Type: "boolean", Description: "Replace all occurrences instead of requiring a unique match (default: false)", Default: false},
		}, Required: []string{"path", "old_string", "new_string"}, AdditionalProperties: boolPtr(false)},
	}
}

// SearchFilesDefinition returns the ripgrep-backed search contract.
func SearchFilesDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        SearchFilesName,
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

// TerminalDefinition returns the sandboxed shell contract. Foreground
// execution is fully supported; background/pty/notify/heartbeat/persist
// parameters are accepted for compatibility but background execution is not
// scheduled in this deployment — rerun foreground with a higher timeout.
func TerminalDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        TerminalName,
		Description: "Run a shell command in the profile working directory (CWD) for the stated OS/shell. Filesystem, cwd, and env persist per profile. Do not use cat/grep/sed/echo for file reads/writes — reserve terminal for builds, git, process, network, and package commands. Output auto-truncates past ~100KB with secrets redacted. Foreground returns instantly on finish; use background=true only for processes that must outlive the call. Verify servers with a health check in a separate call. The result cwd is authoritative after cd.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"command":            {Type: "string", Description: "The shell command to execute"},
			"background":         {Type: "boolean", Description: "Run in the background, returning a session_id. Not scheduled in this deployment; rerun foreground instead.", Default: false},
			"timeout":            {Type: "integer", Description: "Max seconds to wait (default: 180, foreground max: 600).", Default: 180, Minimum: float64Ptr(1)},
			"workdir":            {Type: "string", Description: "Working directory for this command (absolute path). Defaults to the session working directory."},
			"pty":                {Type: "boolean", Description: "Run in a pseudo-terminal for interactive CLI tools. Not supported here.", Default: false},
			"notify":             {Type: "boolean", Description: "Notify when a background process exits. Background execution is not scheduled here."},
			"heartbeat":          {Type: "integer", Description: "Periodic progress notices for background jobs. Not supported here.", Minimum: float64Ptr(0)},
			"persist_on_release": {Type: "boolean", Description: "Keep the process alive across agent lifecycle cleanup. Not supported here.", Default: false},
		}, Required: []string{"command"}, AdditionalProperties: boolPtr(false)},
	}
}

// ExecuteCodeDefinition returns the stateless Python runner contract. Each
// call runs fresh with python3 in the profile working directory; variables,
// imports, and loaded data do not survive across calls. Persist state in
// workspace files when needed.
func ExecuteCodeDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        ExecuteCodeName,
		Description: "Run Python code statelessly with python3 in the profile working directory (CWD). Each call runs fresh from a temp file; variables, imports, and loaded data do not survive across calls — persist state in workspace files when needed. Returns exit_code with combined stdout/stderr, truncated past ~50KB with secrets redacted. 300s timeout, 200KB code limit. Use for computation and data transforms; use read_file/write_file/patch/search_files/terminal for workspace operations.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"code":  {Type: "string", Description: "Python code to execute. Runs stateless with python3 in the profile working directory; state does not persist across calls unless written to workspace files."},
			"reset": {Type: "boolean", Description: "Discard persisted helper state before running. Stateless runner: clears scratch __pycache__ entries.", Default: false},
		}, Required: []string{"code"}, AdditionalProperties: boolPtr(false)},
	}
}

func float64Ptr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }
