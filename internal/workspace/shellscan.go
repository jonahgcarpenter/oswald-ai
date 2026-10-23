package workspace

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// Verdicts for the shell semantic layer.
const (
	shellAllow    = "allow"
	shellDeny     = "deny"
	shellEscalate = "escalate"
)

// normalizeCommand folds evasion spellings into canonical form before
// matching: line continuations, $IFS and $'\n' splits, fullwidth unicode,
// and home spelled as an absolute path. Newlines are preserved so
// heredoc structure survives for the position tagger.
func normalizeCommand(command string) string {
	s := strings.ReplaceAll(command, "\\\r\n", " ")
	s = strings.ReplaceAll(s, "\\\n", " ")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		line = strings.ReplaceAll(line, "$'\\n'", " ")
		line = strings.ReplaceAll(line, `$"\n"`, " ")
		line = strings.ReplaceAll(line, "$IFS", " ")
		line = foldFullwidth(line)
		lines[i] = strings.Join(strings.Fields(line), " ")
	}
	s = strings.Join(lines, "\n")
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" && home != "/" {
		s = strings.ReplaceAll(s, home, "~")
	}
	s = strings.ReplaceAll(s, "$HOME", "~")
	s = strings.ReplaceAll(s, "${HOME}", "~")
	return s
}

// foldFullwidth maps fullwidth ASCII (U+FF01–U+FF5E) and the ideographic
// space (U+3000) to their ASCII counterparts.
func foldFullwidth(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0x3000:
			return ' '
		case r >= 0xFF01 && r <= 0xFF5E:
			return r - 0xFEE0
		default:
			return r
		}
	}, s)
}

// detectHiddenUnicode reports bidi overrides, zero-width characters, and
// other invisible smuggling codepoints in the original command text.
func detectHiddenUnicode(original string) bool {
	for _, r := range original {
		switch {
		case r >= 0x202A && r <= 0x202E:
			return true
		case r == 0x200B || r == 0x200C || r == 0x200D || r == 0xFEFF:
			return true
		case r >= 0x2066 && r <= 0x2069:
			return true
		case r == 0x00AD:
			return true
		}
	}
	return false
}

var liveSinkQuoted = []*regexp.Regexp{
	// sh -c "..." / bash -c '...' execute their quoted operand.
	regexp.MustCompile(`(?i)\b(?:sh|bash|dash|zsh|fish)\s+(?:-c|--command)\s+("[^"]*"|'[^']*')`),
	// eval runs its operand.
	regexp.MustCompile(`(?i)\beval\s+("[^"]*"|'[^']*'|\$\([^)]*\)|` + "`[^`]*`" + `)`),
}

var commandSubstParen = regexp.MustCompile(`\$\(([^()]*)\)`)
var commandSubstBacktick = regexp.MustCompile("`([^`]*)`")

// liveShellText blanks prose (notably echo/printf operands and inert
// quotes) while keeping executed payloads: command substitutions,
// backticks, live-sink quoted operands, and bash-heredoc bodies.
func liveShellText(command string) string {
	var live strings.Builder
	blanked := command
	// Command substitutions execute: keep the inner text as live.
	for _, re := range []*regexp.Regexp{commandSubstParen, commandSubstBacktick} {
		for _, match := range re.FindAllStringSubmatch(blanked, -1) {
			if len(match) > 1 {
				live.WriteString("\n" + match[1])
			}
		}
		blanked = re.ReplaceAllString(blanked, " ")
	}
	// Live-sink quoted operands execute: keep the inner text as live.
	for _, re := range liveSinkQuoted {
		for _, match := range re.FindAllStringSubmatch(blanked, -1) {
			if len(match) > 1 {
				inner := strings.TrimSpace(match[1])
				if len(inner) >= 2 {
					live.WriteString("\n" + inner[1:len(inner)-1])
				}
			}
		}
		blanked = re.ReplaceAllString(blanked, " ")
	}
	// Heredoc bodies execute only when fed to a shell.
	blanked = splitHeredocs(blanked, &live)
	// Blank every remaining quoted span as prose.
	var out strings.Builder
	inSingle, inDouble := false, false
	for _, r := range blanked {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
			out.WriteRune(' ')
		case r == '"' && !inSingle:
			inDouble = !inDouble
			out.WriteRune(' ')
		case inSingle || inDouble:
			out.WriteRune(' ')
		default:
			out.WriteRune(r)
		}
	}
	out.WriteString(live.String())
	return out.String()
}

var heredocOpen = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z0-9_]+)['"]?`)

// splitHeredocs moves heredoc bodies into live when fed to a shell,
// otherwise blanks them.
func splitHeredocs(blanked string, live *strings.Builder) string {
	lines := strings.Split(blanked, "\n")
	var out []string
	i := 0
	for i < len(lines) {
		line := lines[i]
		open := heredocOpen.FindStringSubmatch(line)
		if open == nil {
			out = append(out, line)
			i++
			continue
		}
		head := line[:strings.Index(line, "<<")]
		terminator := open[1]
		isShell := shellHead.MatchString(head)
		out = append(out, head+" ")
		i++
		var body []string
		for i < len(lines) && strings.TrimSpace(lines[i]) != terminator {
			body = append(body, lines[i])
			i++
		}
		if i < len(lines) {
			i++ // consume the terminator
		}
		if isShell {
			live.WriteString("\n" + strings.Join(body, "\n"))
		}
	}
	return strings.Join(out, "\n")
}

var shellHead = regexp.MustCompile(`(?i)(?:^|[;&|]\s*|&&\s*|\|\|\s*)\s*(?:sh|bash|dash|zsh)\b`)

// protectedRoots are never wiped, even with override.
var protectedRoots = []string{"/", "/etc", "/usr", "/bin", "/sbin", "/boot", "/var", "/opt", "/lib", "/lib64", "/root", "~"}

var (
	reFindDelete   = regexp.MustCompile(`\bfind\s+\S+\s+[^;&|]*-delete\b`)
	reMassKill     = regexp.MustCompile(`\bkill\s+-9\s+-1\b|\bkillall\s+-9\b|\bpkill\s+-9\s+-u\b`)
	rePower        = regexp.MustCompile(`\b(poweroff|reboot|shutdown|halt)\b|\bsystemctl\s+(poweroff|reboot|halt|suspend|hibernate)\b`)
	reDropDatabase = regexp.MustCompile(`(?i)\bDROP\s+(DATABASE|SCHEMA)\b|\bTRUNCATE\b`)
)

// ScanUnrecoverable reports commands that never run, even with reviewer or
// user approval. It operates on live-position text.
func ScanUnrecoverable(live string) (string, bool) {
	for _, root := range protectedRoots {
		if wipeMatches(live, root) {
			return "wipe of " + root, true
		}
		if reFindDelete.MatchString(live) && touchesRoot(live, root) {
			return "recursive delete of " + root, true
		}
	}
	if reMassKill.MatchString(live) {
		return "mass process kill", true
	}
	if rePower.MatchString(live) {
		return "power control", true
	}
	if reDropDatabase.MatchString(live) {
		return "database destruction", true
	}
	return "", false
}

var reRmRf = regexp.MustCompile(`\brm\s+(?:-[a-zA-Z]*r[a-zA-Z]*f[a-zA-Z]*\s+|-[a-zA-Z]*f[a-zA-Z]*r[a-zA-Z]*\s+|--recursive\s+--force\s+|--force\s+--recursive\s+)(.+)`)

func wipeMatches(live, root string) bool {
	match := reRmRf.FindStringSubmatch(live)
	if match == nil {
		return false
	}
	for _, target := range strings.Fields(match[1]) {
		target = strings.Trim(target, "\"'();")
		if target == root || strings.HasPrefix(target, root+"/") {
			return true
		}
		if root == "/" && (target == "/" || target == "/*") {
			return true
		}
	}
	return false
}

func touchesRoot(live, root string) bool {
	for _, field := range strings.Fields(live) {
		field = strings.Trim(field, "\"'();")
		if field == root || strings.HasPrefix(field, root+"/") {
			return true
		}
	}
	return false
}

var (
	reChmodOpen  = regexp.MustCompile(`\bchmod\s+-R\s+(?:777|666|a\+rwx)\b`)
	reDecodePipe = regexp.MustCompile(`\b(?:base64\s+(?:--decode|-d\b)|openssl\s+enc\s+-d\b)[^|]*\|\s*(?:sh|bash|dash|zsh)\b`)
	reGitForce   = regexp.MustCompile(`\bgit\s+push\s+(?:--force|--force-with-lease)\b|\bgit\s+(?:reset\s+--hard|clean\s+-fd)\b`)
	reDaemonOut  = regexp.MustCompile(`/dev/tcp/|\bssh\s+-[LR]\b|\bngrok\b|\bsocat\b.*TCP-LISTEN`)
)

// ScanRecoverable reports suspicious-but-reviewable operations for the LLM
// reviewer path. It operates on live-position text.
func ScanRecoverable(live string) (string, bool) {
	switch {
	case reChmodOpen.MatchString(live):
		return "permission open-up", true
	case reDecodePipe.MatchString(live):
		return "decode-pipe to shell", true
	case reGitForce.MatchString(live):
		return "force git rewrite", true
	case reDaemonOut.MatchString(live):
		return "remote daemon redirect", true
	}
	return "", false
}

var deniedStoreBases = []string{"state.db", "state.db-wal", "state.db-shm", "state.db-journal"}

func tokenHitsStore(token string) bool {
	base := filepath.Base(strings.Trim(token, "\"'();"))
	for _, denied := range deniedStoreBases {
		if base == denied {
			return true
		}
	}
	return false
}

var reSQLite = regexp.MustCompile(`\bsqlite3?\b`)

// ScanStoreAccess reports direct database-file handling in shell for
// approval instead of a hard jail. It operates on live-position text.
func ScanStoreAccess(live string) bool {
	if !strings.Contains(live, "state.db") {
		return false
	}
	if reSQLite.MatchString(live) {
		return true
	}
	for _, token := range strings.Fields(live) {
		if tokenHitsStore(token) {
			return true
		}
	}
	return strings.Contains(live, "<") || strings.Contains(live, ">")
}

// deniedResult renders a hard shell denial. The envelope is returned as a
// successful unproductive result so the model sees the exact contract.
func deniedResult(reason string) governance.Result {
	return governance.Result{Content: BlockEnvelope(reason), Outcome: governance.OutcomeUnproductive}
}

// ScanSecretRead reports live-position reads of secret-bearing paths and
// environment dumps for review. Bare search words without path shape (for
// example grep for the word "credentials") do not fire.
func ScanSecretRead(live string) bool {
	for _, segment := range splitSegments(live) {
		trimmed := strings.TrimSpace(segment)
		if trimmed == "env" || trimmed == "printenv" || trimmed == "set" || trimmed == "export" || trimmed == "export -p" {
			return true
		}
		for _, token := range strings.Fields(segment) {
			token = strings.Trim(token, "\"'();")
			if strings.Contains(token, ".env") {
				return true
			}
			if strings.Contains(token, "credentials") && (strings.ContainsAny(token, "/*.") || strings.HasPrefix(token, "~")) {
				return true
			}
			base := filepath.Base(token)
			if base == "id_rsa" || base == "id_ed25519" {
				return true
			}
			if strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
				return true
			}
		}
	}
	return false
}

func splitSegments(live string) []string {
	var segments []string
	for _, part := range strings.Split(live, "\n") {
		for _, sep := range []string{"&&", "||"} {
			part = strings.ReplaceAll(part, sep, ";")
		}
		segments = append(segments, strings.Split(part, ";")...)
	}
	return segments
}

// proseBlanked blanks quoted spans that are operands of echo/printf only,
// leaving every other quote (flag operands, paths, URLs, live sinks)
// visible for path- and data-sensitive scans. `echo "cat .env"` stays
// prose; `cat ".env"` and `curl -o "/tmp/x"` stay visible.
func proseBlanked(normalized string) string {
	var sb strings.Builder
	inSingle, inDouble := false, false
	var word strings.Builder
	head := ""
	headSet := false
	flushWord := func() {
		w := word.String()
		word.Reset()
		if w == "" {
			return
		}
		if !headSet && !strings.Contains(w, "=") {
			head = strings.ToLower(w)
			headSet = true
		}
		sb.WriteString(w)
	}
	separator := func() {
		flushWord()
		head, headSet = "", false
	}
	for _, r := range normalized {
		switch {
		case r == '\'' && !inDouble:
			flushWord()
			inSingle = !inSingle
			sb.WriteRune(' ')
		case r == '"' && !inSingle:
			flushWord()
			inDouble = !inDouble
			sb.WriteRune(' ')
		case inSingle || inDouble:
			if head == "echo" || head == "printf" {
				sb.WriteRune(' ')
			} else {
				sb.WriteRune(r)
			}
		case r == ';' || r == '|' || r == '&' || r == '\n':
			separator()
			sb.WriteRune(r)
		case r == ' ' || r == '\t':
			flushWord()
			sb.WriteRune(r)
		default:
			word.WriteRune(r)
		}
	}
	flushWord()
	return sb.String()
}

// BlockEnvelope renders the model-visible denial envelope for shell blocks.

// Wording is fixed: rewording, quoting, splitting, or encoding never clears
// a normalized-operation match.
func BlockEnvelope(reason string) string {
	return `{"output":"","exit_code":1,"error":"Blocked: ` + reason + `. Do NOT retry with rewording, quoting, splitting, or encoding — matching is on normalized operations, not wording. Stop the current workflow that needs this operation and report the blocker.","status":"blocked"}`
}
