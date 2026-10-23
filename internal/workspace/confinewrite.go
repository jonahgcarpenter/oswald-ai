package workspace

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Confined writes for shell and Python execution.
//
// File tools (read_file/write_file/patch/search_files) resolve every path
// through the file groups: protected names are sealed, system paths are
// read-only, agent paths are fully open, and outside reads resolve under
// Table 3. Shell and Python do not resolve paths: the command runs with the
// workspace root as its working directory (execute_code) or a jailed
// subdir (terminal), but the text can name write targets anywhere. The scan
// below closes that gap by resolving write-sink shapes lexically and
// denying anything outside agent scope with its file-group message.
//
// Reads stay open under the file groups (secret reads still escalate
// through ScanSecretRead); only writes are denied here. /dev/null and the
// standard /dev/fd aliases are exempt: discarding output is not a write.

var (
	reCdTarget = regexp.MustCompile(`(?:^|[;&|\n]\s*)\s*cd\s+([^\s;&|]+)`)
	// reAbsPath finds absolute (or ~-rooted) path shapes anywhere in text,
	// including inside quotes and parens. Query strings are cut at '?'.
	reAbsPath = regexp.MustCompile(`(?:^|[\s'"` + "`" + `(:,;=()])((?:~?/[^\s'"` + "`" + `;,|&<>()$!?]+))`)
	// reRelPath finds ..-anchored relative shapes: .., ../x, ../../x.
	reRelPath    = regexp.MustCompile(`(?:^|[\s'"` + "`" + `(;,=])(\.\.(?:/[^\s'"` + "`" + `;,|&<>()$!?]+)?)`)
	reDdOf       = regexp.MustCompile(`\bof=([^\s;&|]+)`)
	reTeeTarget  = regexp.MustCompile(`\btee\s+(?:-[a-zA-Z]*\s+)*(\S+)`)
	reRedirectOp = regexp.MustCompile(`>>?\s*(\S+)`)
	reSedInPlace = regexp.MustCompile(`\bsed\s+[^;&|]*-i\b`)
)

// devNullAliases are discard/stdio devices, never real writes.
var devNullAliases = map[string]bool{
	"/dev/null": true, "/dev/stdin": true, "/dev/stdout": true, "/dev/stderr": true,
}

// ScanConfinedWrite reports write-sink path shapes resolving outside
// agent-owned scope, with the file-group denial as reason. shell selects
// prose-blanked shell scanning (cd-aware) versus raw Python scanning with
// write-call detection. Reads are never denied here.
func ScanConfinedWrite(command, workdir, root string, shell bool) (string, bool) {
	text := command
	if shell {
		text = proseBlanked(normalizeCommand(command))
	}
	root = filepath.Clean(root)
	if strings.TrimSpace(text) == "" || root == "" || root == "." || root == "/" {
		return "", false
	}
	workdir = filepath.Clean(workdir)
	bases := []string{workdir}
	if shell {
		for _, m := range reCdTarget.FindAllStringSubmatch(text, -1) {
			if len(m) < 2 || len(bases) >= 8 {
				continue
			}
			if target := resolveCandidate(m[1], workdir); target != "" {
				bases = append(bases, target)
			}
		}
	}
	check := func(shape, base string) (string, bool) {
		resolved := resolveWriteTarget(shape, base)
		if resolved == "" {
			return "", false
		}
		if devNullAliases[resolved] {
			return "", false
		}
		group := Classify(resolved, root)
		if group == GroupAgent {
			return "", false
		}
		return DenyMessage(displayPath(resolved, root), group, true), true
	}
	// Explicit write sinks: redirections, tee operands, cp/mv destinations,
	// dd of=.
	for _, re := range []*regexp.Regexp{reRedirectOp, reTeeTarget} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if len(m) < 2 {
				continue
			}
			for _, base := range bases {
				if reason, blocked := check(m[1], base); blocked {
					return reason, true
				}
			}
		}
	}
	for _, m := range reDdOf.FindAllStringSubmatch(text, -1) {
		for _, base := range bases {
			if reason, blocked := check(m[1], base); blocked {
				return reason, true
			}
		}
	}
	for _, dest := range copyDests(text) {
		for _, base := range bases {
			if reason, blocked := check(dest, base); blocked {
				return reason, true
			}
		}
	}
	// Per-segment operands: sed -i (whole segment writes), rm/rmdir/touch/
	// mkdir operands, and Python write calls.
	for _, seg := range splitSegments(text) {
		if reSedInPlace.MatchString(seg) {
			for _, token := range strings.Fields(seg) {
				for _, base := range bases {
					if resolved := resolveWriteTarget(token, base); resolved != "" {
						if reason, blocked := checkToken(resolved, root); blocked {
							return reason, true
						}
					}
				}
			}
			continue
		}
		fields := strings.Fields(seg)
		for i, tok := range fields {
			if !isDestructiveOperandCommand(tok) {
				continue
			}
			for _, operand := range fields[i+1:] {
				if strings.HasPrefix(operand, "-") || strings.ContainsAny(operand, "|;<>") {
					continue
				}
				for _, base := range bases {
					if resolved := resolveWriteTarget(operand, base); resolved != "" {
						if reason, blocked := checkToken(resolved, root); blocked {
							return reason, true
						}
					}
				}
			}
		}
		if !shell {
			for _, lit := range pyPathLiterals(seg) {
				for _, base := range bases {
					resolved := resolveShape(lit, base)
					if resolved == "" || devNullAliases[resolved] {
						continue
					}
					group := Classify(resolved, root)
					switch {
					case group == GroupProtected:
						return DenyMessage(displayPath(resolved, root), group, true), true
					case group == GroupSystem && pyWriteCall(seg, lit):
						return DenyMessage(displayPath(resolved, root), group, true), true
					}
				}
			}
		}
	}
	return "", false
}

func checkToken(resolved, root string) (string, bool) {
	if devNullAliases[resolved] {
		return "", false
	}
	group := Classify(resolved, root)
	if group == GroupAgent {
		return "", false
	}
	return DenyMessage(displayPath(resolved, root), group, true), true
}

func isDestructiveOperandCommand(token string) bool {
	switch strings.ToLower(strings.Trim(token, "\"'();")) {
	case "rm", "rmdir", "touch", "mkdir":
		return true
	}
	return false
}

// copyDests extracts cp/mv destination operands: the last non-flag token
// of each cp/mv invocation.
func copyDests(text string) []string {
	var dests []string
	for _, seg := range splitSegments(text) {
		fields := strings.Fields(seg)
		for i, tok := range fields {
			trimmed := strings.ToLower(strings.Trim(tok, "\"'();"))
			if trimmed != "cp" && trimmed != "mv" {
				continue
			}
			var last string
			for _, operand := range fields[i+1:] {
				if strings.HasPrefix(operand, "-") || strings.ContainsAny(operand, "|;<>") {
					continue
				}
				last = operand
			}
			if last != "" {
				dests = append(dests, last)
			}
		}
	}
	return dests
}

var rePyStringLit = regexp.MustCompile(`"([^"\n]*)"|'([^'\n]*)'`)

// pyPathLiterals returns quoted string literals that carry path shape.
func pyPathLiterals(seg string) []string {
	var out []string
	for _, m := range rePyStringLit.FindAllStringSubmatch(seg, -1) {
		for _, lit := range m[1:] {
			lit = strings.TrimSpace(lit)
			if lit == "" {
				continue
			}
			if i := strings.Index(lit, "?"); i >= 0 {
				lit = lit[:i]
			}
			if !strings.Contains(lit, "/") && !strings.HasPrefix(lit, ".") && lit != ".." {
				continue
			}
			out = append(out, lit)
		}
	}
	return out
}

// pyWriteCall reports whether a path literal sits in a Python write call:
// open() with a write mode, or a known-mutating stdlib/pathlib call.
func pyWriteCall(seg, lit string) bool {
	idx := strings.Index(seg, lit)
	if idx < 0 {
		return false
	}
	head := seg[:idx]
	// Find the innermost unclosed call before the literal.
	depth := 0
	start := -1
	for i := len(head) - 1; i >= 0; i-- {
		switch head[i] {
		case ')':
			depth++
		case '(':
			if depth == 0 {
				start = i
			} else {
				depth--
			}
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		return false
	}
	call := head[:start]
	dot := strings.LastIndexAny(call, ". \t")
	ident := strings.ToLower(strings.Trim(call[dot+1:], " \t"))
	full := strings.ToLower(strings.TrimSpace(call))
	switch ident {
	case "makedirs", "mkdir", "remove", "unlink", "rmdir", "rename", "renames",
		"replace", "chmod", "chown", "truncate", "symlink", "link", "mknod",
		"mkfifo", "write_text", "write_bytes", "touch", "copy", "copy2",
		"copytree", "move", "rmtree", "symlink_to", "hardlink_to":
		return true
	case "open":
		// Write mode is the next quoted argument after the path literal:
		// open(path, "w"). The tail starts with the path's closing quote.
		tail := seg[idx+len(lit):]
		tail = strings.TrimLeft(tail, "\"'")
		tail = strings.TrimLeft(tail, " \t")
		if !strings.HasPrefix(tail, ",") {
			return false
		}
		if m := regexp.MustCompile(`^,\s*["']([^"']*)["']`).FindStringSubmatch(tail); m != nil {
			if strings.ContainsAny(m[1], "wax+") {
				return true
			}
		}
		return false
	}
	// pathlib constructor with a mutating chained call in the statement.
	if ident == "path" || full == "pathlib.path" {
		rest := seg[idx+len(lit):]
		if end := strings.IndexAny(rest, ";\n"); end >= 0 {
			rest = rest[:end]
		}
		if regexp.MustCompile(`\.(mkdir|touch|write_text|write_bytes|unlink|rmdir|rename|replace|symlink_to|hardlink_to|chmod)\s*\(`).MatchString(rest) {
			return true
		}
	}
	return false
}

// resolveWriteTarget maps a write-sink token to an absolute path. It falls
// back to joining bare filenames (no slash) that resolveShape skips, since
// every operand in a write sink names a file.
func resolveWriteTarget(shape, base string) string {
	if resolved := resolveShape(shape, base); resolved != "" {
		return resolved
	}
	trimmed := strings.Trim(shape, "\"'();,")
	if trimmed == "" || strings.HasPrefix(trimmed, "-") || !strings.Contains(trimmed, ".") {
		return ""
	}
	return filepath.Join(base, trimmed)
}

// resolveCandidate maps one whitespace token to an absolute path, or ""
// when the token carries no path shape. base is the shell working directory
// (or one of its cd targets) the token resolves against.
func resolveCandidate(token, base string) string {
	t := strings.Trim(token, "\"'();,")
	if t == "" {
		return ""
	}
	// VAR=value assignments: inspect the value.
	if !strings.HasPrefix(t, "/") && !strings.HasPrefix(t, "~") && !strings.HasPrefix(t, ".") {
		if i := strings.Index(t, "="); i >= 0 {
			rhs := strings.Trim(t[i+1:], "\"'();,")
			if strings.HasPrefix(rhs, "/") || strings.HasPrefix(rhs, "~") || strings.HasPrefix(rhs, ".") {
				t = rhs
			} else {
				return ""
			}
		} else if !strings.Contains(t, "/") {
			return ""
		}
	}
	return resolveShape(t, base)
}

// resolveShape maps a bare path shape (absolute, ~-rooted, or relative) to
// an absolute path against base, or "" when it has no path shape.
func resolveShape(shape, base string) string {
	t := strings.Trim(shape, "\"'();,")
	if t == "" {
		return ""
	}
	if t == "~" || strings.HasPrefix(t, "~/") {
		t = filepath.Join(userHome(), strings.TrimPrefix(t, "~"))
	} else if strings.HasPrefix(t, "~") {
		return ""
	} else if !filepath.IsAbs(t) && t != ".." && !strings.HasPrefix(t, "../") && !strings.HasPrefix(t, "./") && !strings.Contains(t, "/") {
		return ""
	} else if !filepath.IsAbs(t) {
		t = filepath.Join(base, t)
	}
	return filepath.Clean(t)
}
