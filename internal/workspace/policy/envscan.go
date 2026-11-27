package workspace

import (
	"regexp"
	"strings"
)

// OSWALD_* environment names anchor the oswald CLI's profile scope: every
// `oswald` subcommand resolves its configuration root and profile from
// OSWALD_HOME. Sandboxed children are pinned to the acting profile and the
// passthrough channel refuses the names outright (process/sandboxenv.go),
// but command text can still assign them inline, which re-points the CLI at
// another profile's root. The scan below denies the assign and clear forms;
// reads stay allowed so scripts can inspect their pinned scope.

// envScopeReason is the fixed denial sentence for the model.
const envScopeReason = "OSWALD_* environment variables are fixed to the acting profile and cannot be assigned, exported, unset, or cleared"

// envClearReason covers a cleared environment, which drops the pinned anchor.
const envClearReason = "clearing the environment would re-scope the oswald CLI away from the acting profile"

var (
	// reScopeAssign catches NAME=value assignments of any OSWALD_* name,
	// including inline prefixes, export/declare/readonly forms, and
	// runtime spellings such as process.env.OSWALD_HOME = '...'.
	reScopeAssign = regexp.MustCompile(`\bOSWALD_[A-Z0-9_]*\s*=(?:[^=]|$)|process\.env(?:\.OSWALD_[A-Z0-9_]*|\[\s*['"]OSWALD_[A-Z0-9_]*['"]\s*\])\s*=(?:[^=]|$)`)
	// reScopeUnset catches shell unsets and `env -u NAME`.
	reScopeUnset = regexp.MustCompile(`\b(?:unset|export\s+-n)\s+(?:-\S+\s+)*OSWALD_[A-Z0-9_]*\b|\benv\s+(?:-\S+\s+)*-u\s+OSWALD_[A-Z0-9_]*\b`)
	// reScopeClear catches a cleared environment (`env -i`,
	// `env --ignore-environment`).
	reScopeClear = regexp.MustCompile(`\benv\s+(?:-\S+\s+)*(?:-i\b|--ignore-environment\b)`)
	// reOswaldCLI reports a scope-reading CLI invocation.
	reOswaldCLI = regexp.MustCompile(`\boswald(?:-server)?\b`)
	// rePythonScopeMutate catches os.environ mutations for scope names.
	rePythonScopeMutate = regexp.MustCompile(`os\.environ\s*\[\s*['"]OSWALD_[A-Z0-9_]*['"]\s*\]\s*=|del\s+os\.environ\s*\[\s*['"]OSWALD_[A-Z0-9_]*['"]\s*\]|os\.(?:putenv|unsetenv)\s*\(\s*['"]OSWALD_[A-Z0-9_]*['"]|os\.environ\.(?:pop|setdefault|update)\s*\([^)\n]*['"]OSWALD_[A-Z0-9_]*['"]`)
)

// ScanEnvRetarget reports command text or code that assigns or clears an
// OSWALD_* profile-scope variable. shell selects shell normalization
// (echo/printf operands stay prose) versus raw text scanning for Python and
// stdin payloads. Reads are never denied here.
func ScanEnvRetarget(text string, shell bool) (string, bool) {
	scan := text
	if shell {
		scan = proseBlanked(normalizeCommand(text))
	}
	if strings.TrimSpace(scan) == "" {
		return "", false
	}
	if reScopeAssign.MatchString(scan) || reScopeUnset.MatchString(scan) || rePythonScopeMutate.MatchString(scan) {
		return envScopeReason, true
	}
	if reScopeClear.MatchString(scan) && reOswaldCLI.MatchString(scan) {
		return envClearReason, true
	}
	return "", false
}
