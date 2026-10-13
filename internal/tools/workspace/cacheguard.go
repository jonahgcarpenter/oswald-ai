package workspace

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Managed-cache containment for shell and Python execution.
//
// File tools (read_file/write_file/patch/search_files) jail paths through
// Workspace.Resolve, which allows cache/scratch/ but denies every other
// cache/ sibling and .cache/. Shell and Python do not resolve paths: the
// command runs with the profile root as its working directory but the text
// can name cache/scratch-gopath-style siblings directly. The scans below
// close that gap by resolving path-shaped tokens lexically against the
// workspace root and hard-denying anything inside managed cache but outside
// cache/scratch/.
//
// Matching is fail-closed and hard-deny (never reviewer-escalated): there is
// always a correct alternative (use cache/scratch/).

var (
	// reCacheLiteral finds quoted string literals so os.path.join("cache",
	// "x") style splits still resolve even though no single token carries
	// a slash.
	reCacheLiteral = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)
)

// cacheFileSink reports whether text looks like a filesystem operation. Bare
// "cache"/".cache" literals only fire alongside a sink so print("cache")
// stays prose while open("cache") and ls cache do not.
func cacheFileSink(text string) bool {
	lowered := strings.ToLower(text)
	for _, sink := range []string{
		"open", "path", "join", "mkdir", "makedir", "listdir", "scandir",
		"walk", "glob", "remove", "unlink", "rmdir", "rename", "replace",
		"chdir", "chmod", "stat", "sqlite", "save", "load", "write", "read",
		"ls", "cat", "cp", "mv", "rm", "touch", "find", "grep", "tee",
		"shutil", "os.", "pathlib",
	} {
		if strings.Contains(lowered, sink) {
			return true
		}
	}
	return false
}

// managedCacheResolved reports whether a workspace-absolute path reaches
// managed cache the model must not touch: anything under <root>/cache
// outside <root>/cache/scratch, or anything under <root>/.cache.
func managedCacheResolved(resolved, root string) bool {
	under := func(p, dir string) bool {
		return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
	}
	root = filepath.Clean(root)
	cacheRoot := filepath.Join(root, "cache")
	scratchRoot := filepath.Join(cacheRoot, "scratch")
	dotCache := filepath.Join(root, ".cache")
	if resolved == scratchRoot || under(resolved, scratchRoot) {
		return false
	}
	if resolved == cacheRoot || under(resolved, cacheRoot) {
		return true
	}
	if resolved == dotCache || under(resolved, dotCache) {
		return true
	}
	return false
}

// ScanManagedCacheShell reports shell text referencing managed cache outside
// cache/scratch/. Only echo/printf prose is blanked, matching the other
// path-sensitive scans.
func ScanManagedCacheShell(command, workdir string) bool {
	return managedCacheInText(proseBlanked(normalizeCommand(command)), workdir, true)
}

// ScanManagedCacheCode reports Python code referencing managed cache outside
// cache/scratch/. String literals are live in Python, so the raw code is
// scanned.
func ScanManagedCacheCode(code, workdir string) bool {
	return managedCacheInText(code, workdir, false)
}

func managedCacheInText(text, workdir string, shell bool) bool {
	root := filepath.Clean(workdir)
	if strings.TrimSpace(text) == "" || root == "" || root == "." || root == "/" {
		return false
	}
	bases := []string{root}
	if shell {
		for _, m := range reCdTarget.FindAllStringSubmatch(text, -1) {
			if len(m) < 2 || len(bases) >= 8 {
				continue
			}
			if target := resolveCandidate(m[1], root); target != "" {
				bases = append(bases, target)
			}
		}
	}
	check := func(shape, base string) bool {
		resolved := resolveShape(shape, base)
		return resolved != "" && managedCacheResolved(resolved, root)
	}
	// Absolute shapes match anywhere, including inside quotes and parens.
	for _, m := range reAbsPath.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 {
			continue
		}
		if resolved := resolveShape(m[1], root); resolved != "" && managedCacheResolved(resolved, root) {
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
	// Plain relative tokens (cache/foo) resolve against the workdir.
	for _, token := range strings.Fields(text) {
		if resolved := resolveCandidate(token, root); resolved != "" && managedCacheResolved(resolved, root) {
			return true
		}
		// VAR=value assignments with a relative RHS (GOPATH=cache/...) are
		// rejected by resolveCandidate but still name a workspace path.
		if i := strings.Index(token, "="); i >= 0 {
			rhs := strings.Trim(strings.TrimSpace(token[i+1:]), "\"'();,")
			if rhs == "" || (!strings.Contains(rhs, "/") && rhs != "cache" && rhs != ".cache") {
				continue
			}
			if !strings.Contains(strings.ToLower(rhs), "cache") {
				continue
			}
			for _, base := range bases {
				if check(rhs, base) {
					return true
				}
			}
		}
	}
	// Split literals (os.path.join("cache", "x")) and bare "cache" refs:
	// a slash-bearing cache literal is a path on its own, while a bare
	// literal needs a filesystem sink so print("cache") stays allowed.
	sink := cacheFileSink(text)
	for _, m := range reCacheLiteral.FindAllStringSubmatch(text, -1) {
		for _, lit := range m[1:] {
			lit = strings.TrimSpace(lit)
			if lit == "" {
				continue
			}
			// Cut query strings like the absolute-path scanner.
			if i := strings.Index(lit, "?"); i >= 0 {
				lit = lit[:i]
			}
			trimmed := strings.Trim(lit, "\"'();,")
			if trimmed == "" {
				continue
			}
			if strings.Contains(trimmed, "/") || strings.Contains(trimmed, "\\") {
				// Only interesting when cache-shaped.
				lowered := strings.ToLower(strings.ReplaceAll(trimmed, "\\", "/"))
				if !strings.Contains(lowered, "cache") {
					continue
				}
				for _, base := range bases {
					if check(trimmed, base) {
						return true
					}
				}
				continue
			}
			if trimmed == "cache" || trimmed == ".cache" {
				if sink {
					if resolved := filepath.Join(root, trimmed); managedCacheResolved(resolved, root) {
						return true
					}
				}
			}
		}
	}
	// Unquoted bare refs (ls cache): deny when a filesystem sink is present
	// so echo cache without file intent stays allowed.
	if sink {
		for _, field := range strings.Fields(text) {
			t := strings.Trim(field, "\"'();,")
			if t == "cache" || t == ".cache" {
				if resolved := filepath.Join(root, t); managedCacheResolved(resolved, root) {
					return true
				}
			}
		}
	}
	return false
}
