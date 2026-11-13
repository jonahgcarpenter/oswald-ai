package workspace

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Tool names exposed to the model.
const (
	TerminalName      = "terminal"
	ExecuteCodeName   = "execute_code"
	CronjobManageName = "cronjob_manage"
)

// Workspace tool contracts are composed in stable registration order by
// tools.BuiltinDefinitions; each Definition lives next to its handler.

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
