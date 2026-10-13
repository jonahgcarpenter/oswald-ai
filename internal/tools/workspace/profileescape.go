package workspace

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Cross-profile containment for shell and Python execution.
//
// File tools (read_file/write_file/patch/search_files) resolve every path
// through Workspace.Resolve, which jails them to the caller's profile root.
// Shell and Python do not: the command runs with the profile root as its
// working directory, but the command text can name absolute paths (or ..
// traversals, or cd targets) reaching sibling profiles or global operator
// files. The scans below close that gap by resolving path-shaped tokens
// lexically against the .oswald data tree and denying anything inside the
// tree but outside the caller's root.
//
// Matching is fail-closed and hard-deny (never reviewer-escalated):
// cross-profile shell/Python access is never legitimate, mirroring the
// denyProfile outcome of the file tools.

var (
	reCdTarget = regexp.MustCompile(`(?:^|[;&|\n])\s*cd\s+([^\s;&|]+)`)
	// reAbsPath finds absolute (or ~-rooted) path shapes anywhere in text,
	// including inside quotes and parens: open('/x'), file:/x, DB=/x.
	// Query strings are cut at '?'.
	reAbsPath = regexp.MustCompile(`(?:^|[\s'"` + "`" + `(:,;=()])((?:~?/[^\s'"` + "`" + `;,|&<>()$!?]+))`)
	// reRelPath finds ..-anchored relative shapes: .., ../x, ../../x.
	reRelPath = regexp.MustCompile(`(?:^|[\s'"` + "`" + `(;,=])(\.\.(?:/[^\s'"` + "`" + `;,|&<>()$!?]+)?)`)
)

// ScanProfileEscapeShell reports shell text referencing profile data outside
// the caller's workspace. Quoted payloads stay visible (python -c bodies
// included); only echo/printf prose is blanked, matching the other
// path-sensitive scans.
func ScanProfileEscapeShell(command, workdir string) bool {
	return escapeInText(proseBlanked(normalizeCommand(command)), workdir)
}

// ScanProfileEscapeCode reports raw Python code referencing profile data
// outside the caller's workspace. execute_code runs with the profile root
// as its working directory, so relative paths resolve against workdir.
func ScanProfileEscapeCode(code, workdir string) bool {
	return escapeInText(code, workdir)
}

// dataRoot returns the .oswald data-tree ancestor of workdir. The default
// profile's root is the tree itself; a named profile's root is
// <tree>/profiles/<name>. A workdir outside any .oswald tree has no
// profile-shaped paths to protect.
func dataRoot(workdir string) (string, bool) {
	dir := filepath.Clean(workdir)
	for {
		if filepath.Base(dir) == ".oswald" {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// escapesProfile reports whether a resolved path reaches profile data the
// caller must not touch: anything inside the data tree but outside the
// caller's root. The default profile additionally loses profiles/*, which
// lives inside its own root.
func escapesProfile(resolved, root, tree string) bool {
	under := func(p, dir string) bool {
		return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
	}
	if !under(resolved, tree) {
		return false
	}
	if under(resolved, root) {
		if root == tree && under(resolved, filepath.Join(tree, "profiles")) {
			return true
		}
		return false
	}
	return true
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

func escapeInText(text, workdir string) bool {
	root := filepath.Clean(workdir)
	tree, ok := dataRoot(root)
	if !ok || strings.TrimSpace(text) == "" {
		return false
	}
	bases := []string{root}
	for _, m := range reCdTarget.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 || len(bases) >= 8 {
			continue
		}
		if target := resolveCandidate(m[1], root); target != "" {
			bases = append(bases, target)
		}
	}
	check := func(shape, base string) bool {
		resolved := resolveShape(shape, base)
		return resolved != "" && escapesProfile(resolved, root, tree)
	}
	// Absolute shapes match anywhere, including inside quotes and parens.
	for _, m := range reAbsPath.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 {
			continue
		}
		if resolved := resolveShape(m[1], root); resolved != "" && escapesProfile(resolved, root, tree) {
			return true
		}
	}
	// Relative shapes resolve against the workdir and every cd target.
	for _, m := range reRelPath.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 {
			continue
		}
		for _, base := range bases {
			if check(m[1], base) {
				return true
			}
		}
	}
	// Plain relative tokens (profiles/x for the default profile) resolve
	// against the workdir.
	for _, token := range strings.Fields(text) {
		if resolved := resolveCandidate(token, root); resolved != "" && escapesProfile(resolved, root, tree) {
			return true
		}
	}
	return false
}
