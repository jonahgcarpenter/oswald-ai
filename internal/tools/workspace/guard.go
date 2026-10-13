// Package workspace terminal guards implement the pre-execution security
// chain: foreground-background guidance refusals, workdir allowlists,
// gateway-lifecycle protection, self-repo mutation blocks, and dangerous
// command detection. All checks fail closed with rule names only; commands,
// paths, and script contents are never logged.
package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	detachLeader = regexp.MustCompile(`(?:^|[;&|]\s*|&&\s*|\|\|\s*|\$\(\s*)(?:nohup|disown|setsid)\b`)
	trailingAmp  = regexp.MustCompile(`\s&\s*(?:#.*)?$`)
	middleAmp    = regexp.MustCompile(`\s&\s`)
	serverBin    = regexp.MustCompile(`\b(npm|pnpm|yarn|bun)(\s+run)?\s+(dev|start|serve|watch)\b`)
	serverSingle = regexp.MustCompile(`\b(docker\s+compose\s+up|next\s+dev|vite|nodemon|uvicorn|gunicorn|pm2\s+start)\b`)
	httpServer   = regexp.MustCompile(`\bpython[23]?\s+-m\s+http\.server\b`)
	helpFlag     = regexp.MustCompile(`(?:^|\s)(--help|-h|--version|-v)(?:\s|$)`)
)

var allowedWorkdir = regexp.MustCompile(`^[A-Za-z0-9/\\:_\-.~ +@=,]+$`)

var dangerousPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`\brm\s+-rf?\s+/(?:\s|$)`), "recursive root delete"},
	{regexp.MustCompile(`\brm\s+-rf?\s+~(?:\s|/|$)`), "recursive home delete"},
	{regexp.MustCompile(`\bmkfs\b`), "filesystem format"},
	{regexp.MustCompile(`\bdd\s+.*of=/dev/`), "raw device write"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\|\:&\s*\}\s*;\s*:`), "fork bomb"},
	{regexp.MustCompile(`\bsudo\b`), "privilege escalation"},
	{regexp.MustCompile(`\bchmod\s+-R\s+777\s+/\b`), "permission destroy"},
	{regexp.MustCompile(`\bcurl\b.*\|\s*(?:sh|bash)\b`), "remote code pipe"},
	{regexp.MustCompile(`\bwget\b.*\|\s*(?:sh|bash)\b`), "remote code pipe"},
}

var gatewayLifecycle = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)systemctl\s+(restart|stop)\s+.*oswald`), "service lifecycle"},
	{regexp.MustCompile(`(?i)service\s+oswald\s+(restart|stop)`), "service lifecycle"},
	{regexp.MustCompile(`(?i)docker\s+(restart|stop|rm).*oswald`), "service lifecycle"},
	{regexp.MustCompile(`(?i)\boswald(-server)?\b.*\b(bootstrap|submit)\b`), "gateway bootstrap"},
}

// stripInert is the single choke point for shell matching: it normalizes
// evasion spellings (line continuations, $IFS splits, fullwidth unicode,
// home-as-absolute-path) and blanks prose (notably echo/printf operands and
// inert quotes) while keeping executed payloads — command substitutions,
// backticks, live-sink quoted operands, and shell-fed heredoc bodies — so
// detachment/lifecycle/destructive regexes match live shell structure.
// `echo "rm -rf /"` stays prose; `sh -c "rm -rf /"` stays live.
func stripInert(command string) string {
	return liveShellText(normalizeCommand(command))
}

// RefuseDetachableForeground rejects long-lived server patterns on the
// foreground path with a rewrite hint.
func RefuseDetachableForeground(command string) error {
	if helpFlag.MatchString(command) {
		return nil
	}
	live := stripInert(command)
	if detachLeader.MatchString(live) {
		return errors.New("background detachment is not allowed on foreground calls; rerun with background=true and poll with process_manage")
	}
	if trailingAmp.MatchString(live) || middleAmp.MatchString(live) {
		return errors.New("shell backgrounding (&) is not allowed; rerun with background=true and poll with process_manage")
	}
	if serverBin.MatchString(live) || serverSingle.MatchString(live) || httpServer.MatchString(live) {
		return errors.New("long-lived servers must run with background=true and a separate health check via process_manage")
	}
	return nil
}

// ValidateWorkdirString enforces the unicode allowlist for workdir inputs.
func ValidateWorkdirString(raw string) error {
	if strings.ContainsAny(raw, "\x00\n\r") {
		return errors.New("working directory contains blocked characters")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return errors.New("working directory contains blocked characters")
		}
	}
	if !allowedWorkdir.MatchString(raw) {
		return errors.New("working directory contains blocked characters")
	}
	if strings.Contains(raw, "$(") || strings.Contains(raw, "`") {
		return errors.New("working directory contains blocked characters")
	}
	return nil
}

// DetectDangerous reports whether a command matches a destructive pattern.
func DetectDangerous(command string) (bool, string) {
	live := stripInert(command)
	for _, p := range dangerousPatterns {
		if p.re.MatchString(live) {
			return true, p.reason
		}
	}
	return false, ""
}

// ScanLifecycle blocks supervised-gateway lifecycle operations. Referenced
// local scripts (first .sh token) are read up to 1MiB with NUL rejection;
// unscannable references fail closed.
func ScanLifecycle(command, cwd string) error {
	live := stripInert(command)
	for _, p := range gatewayLifecycle {
		if p.re.MatchString(live) {
			return errors.New("blocked: service lifecycle targeting the agent gateway (" + p.reason + ")")
		}
	}
	for _, token := range strings.Fields(live) {
		if !strings.HasSuffix(token, ".sh") {
			continue
		}
		script := strings.Trim(token, "\"'();")
		if script == "" {
			continue
		}
		var abs string
		if filepath.IsAbs(script) {
			abs = script
		} else {
			abs = filepath.Join(cwd, script)
		}
		body, err := readScriptHead(abs)
		if err != nil {
			return errors.New("blocked: referenced script cannot be scanned")
		}
		for _, p := range gatewayLifecycle {
			if p.re.MatchString(body) {
				return errors.New("blocked: referenced script targets the agent gateway (" + p.reason + ")")
			}
		}
	}
	return nil
}

func readScriptHead(abs string) (string, error) {
	info, err := os.Lstat(abs)
	if err != nil {
		// Missing files are not lifecycle operations.
		return "", nil
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", errors.New("unscannable")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", errors.New("unscannable")
	}
	for _, b := range data {
		if b == 0 {
			return "", errors.New("unscannable")
		}
	}
	return string(data), nil
}

// BlockSelfRepo rejects git mutations against the live checkout.
func BlockSelfRepo(command, cwd string) error {
	live := stripInert(command)
	if !strings.Contains(live, "git ") {
		return nil
	}
	mutating := []string{"git checkout", "git reset --hard", "git clean -fd", "git pull", "git stash", "git rebase", "git merge"}
	for _, m := range mutating {
		if strings.Contains(live, m) {
			// Only blocks when the workdir is inside a git checkout.
			if _, err := os.Stat(filepath.Join(cwd, ".git")); err == nil {
				return errors.New("blocked: git mutation of the live checkout")
			}
		}
	}
	return nil
}
