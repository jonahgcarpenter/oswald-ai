package workspace

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// Tool names exposed to the model.
const (
	ReadFileName    = "read_file"
	WriteFileName   = "write_file"
	PatchName       = "patch"
	SearchFilesName = "search_files"
	TerminalName    = "terminal"
	ExecuteCodeName = "execute_code"
)

// Definitions returns the six workspace tool contracts in stable order.
func Definitions() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		ReadFileDefinition(),
		WriteFileDefinition(),
		PatchDefinition(),
		SearchFilesDefinition(),
		TerminalDefinition(),
		ExecuteCodeDefinition(),
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
		Description: "Run a shell command in the profile working directory (CWD). Commands run with /bin/sh -c, a minimal environment (PATH, HOME, LANG, TMPDIR pointing at the profile scratch directory), and the resolved workdir as cwd. Use foreground with a generous timeout for builds/tests; output is truncated past ~100KB with a truncation flag. background/pty/notify/heartbeat/persist_on_release are accepted for compatibility but background execution is not scheduled here: rerun the command foreground with a higher timeout (max 600s). Never access paths outside CWD with ../ or absolute escapes; use read_file/write_file/patch/search_files for file work.",
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
		Description: "Run Python that calls Hermes tools programmatically. Use when you need 3+ tool calls with logic between them: filtering/reducing large outputs before they enter context, branching, or loops (N pages/files, retry on failure). Use normal tool calls for single calls, results you must reason over in full, or anything needing user interaction.\n\nCalls run in a persistent session kernel: variables, imports, and loaded data survive across execute_code calls, so build on earlier work instead of re-loading it. A timed-out or interrupted call loses that state.\n\nAvailable via `from hermes_tools import ...`:\n\n  web_search(query: str, limit: int = 5) -> dict\n    Returns {\"data\": {\"web\": [{\"url\", \"title\", \"description\"}, ...]}}\n  web_extract(urls: list[str], char_limit: int = None) -> dict\n    Returns {\"results\": [{\"url\", \"title\", \"content\", \"error\"}, ...]} where content is markdown.\n    No LLM summarization. Pages over char_limit (default 15000) are head+tail truncated; full text stored on disk (path in the content footer).\n  read_file(path: str, offset: int = 1, limit: int = 2000) -> dict\n    Lines are 1-indexed. Returns {\"content\": \"...\", \"total_lines\": N}\n  write_file(path: str, content: str) -> dict\n    Always overwrites the entire file; an existing file must be read_file'd first or the write is refused.\n  search_files(pattern: str, target=\"content\", path=\".\", file_glob=None, limit=50, order=\"discovery\") -> dict\n    target: \"content\" (search inside files) or \"files\" (find files by name). Returns {\"matches\": [...]}\n  patch(path: str, old_string: str, new_string: str, replace_all: bool = False) -> dict\n    Replaces old_string with new_string in the file.\n  terminal(command: str, timeout=None, workdir=None) -> dict\n    Foreground only (no background/pty). Returns {\"output\": \"...\", \"exit_code\": N}\n\nLimits: 5-minute timeout, max 50 tool calls per call. Stdout over 50KB shows head/tail inline; the FULL text is auto-saved to a file whose path rides in the result.\n\nScripts run in the session's working directory. Interpreter: the project's activated venv/conda python when one is active (VIRTUAL_ENV/CONDA_PREFIX — matches terminal()); otherwise Hermes's own python (the common case — stdlib plus Hermes's deps; check `import x` before relying on project packages).\n\nHelpers require imports: `from hermes_tools import json_parse, shell_quote, retry`. json_parse(text) — tolerant json.loads for terminal() output; shell_quote(s) — shlex.quote for dynamic shell args; retry(fn, max_attempts=3, delay=2) — exponential backoff.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"code":  {Type: "string", Description: "Python code to execute. Runs stateless with python3 in the profile working directory; state does not persist across calls unless written to workspace files."},
			"reset": {Type: "boolean", Description: "Discard persisted helper state before running. Stateless runner: clears scratch __pycache__ entries.", Default: false},
		}, Required: []string{"code"}, AdditionalProperties: boolPtr(false)},
	}
}

func float64Ptr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }
