// Package workspace owns profile-scoped terminal and filesystem state.
//
// Each profile gets one CurrentWorkingDir bound at registry construction from
// the profile's validated ProfileRoot: the default profile uses the global
// root itself, named profiles use <root>/profiles/<name>/. Model-supplied
// paths resolve inside that directory only; anything escaping via ..,
// absolute prefixes, or symlinks is rejected. The default profile
// additionally denies profiles/* and operator secrets that live inside its
// working directory.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Workspace binds file and terminal execution to one profile directory.
type Workspace struct {
	root      string
	profile   string
	isDefault bool
	mu        sync.Mutex
	// reads tracks request-local full-file reads for the write_file gate:
	// request ID -> absolute path -> content hash.
	reads map[string]map[string]string
}

// NewWorkspace binds a workspace to a profile root. An empty root fails
// closed on every operation.
func NewWorkspace(root, profile string) *Workspace {
	abs := strings.TrimSpace(root)
	if abs != "" {
		if resolved, err := filepath.Abs(abs); err == nil {
			abs = resolved
		}
	}
	return &Workspace{
		root:      abs,
		profile:   strings.TrimSpace(profile),
		isDefault: strings.TrimSpace(profile) == "" || strings.TrimSpace(profile) == "default",
		reads:     make(map[string]map[string]string),
	}
}

// Root returns the absolute working directory, or "" when unconfigured.
func (w *Workspace) Root() string {
	if w == nil {
		return ""
	}
	return w.root
}

// ScratchDir returns <cwd>/cache/scratch without creating it.
func (w *Workspace) ScratchDir() string {
	if w == nil || w.root == "" {
		return ""
	}
	return filepath.Join(w.root, "cache", "scratch")
}

// EnsureScratch creates <cwd>/cache/scratch with private permissions,
// rejecting symlink escapes along the way.
func (w *Workspace) EnsureScratch() (string, error) {
	if w == nil || w.root == "" {
		return "", errors.New("workspace is unavailable")
	}
	if err := validateRoot(w.root); err != nil {
		return "", err
	}
	dir := w.ScratchDir()
	if err := mkdirPrivateAll(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Resolve maps a model-supplied path into the workspace. It accepts
// absolute paths, CWD-relative paths, and ~/paths (resolved inside the
// workspace, never the OS home). It returns the absolute path and the
// CWD-relative form. Escapes and denied operator paths fail closed.
func (w *Workspace) Resolve(raw string) (string, string, error) {
	if w == nil || w.root == "" {
		return "", "", errors.New("workspace is unavailable")
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", errors.New("path is required")
	}
	if len(trimmed) > 4096 || strings.ContainsRune(trimmed, 0) {
		return "", "", errors.New("path is invalid")
	}
	cwd := w.root
	var abs string
	switch {
	case strings.HasPrefix(trimmed, "~/"):
		abs = filepath.Join(cwd, strings.TrimPrefix(trimmed, "~/"))
	case trimmed == "~":
		abs = cwd
	case filepath.IsAbs(trimmed):
		abs = filepath.Clean(trimmed)
	default:
		abs = filepath.Join(cwd, trimmed)
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", errors.New("path escapes the working directory")
	}
	if err := w.checkDenied(rel); err != nil {
		return "", "", err
	}
	if err := validateRoot(cwd); err != nil {
		return "", "", err
	}
	resolved, err := resolveNoEscape(cwd, abs)
	if err != nil {
		return "", "", err
	}
	rel, err = filepath.Rel(cwd, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", errors.New("path escapes the working directory")
	}
	if err := w.checkDenied(rel); err != nil {
		return "", "", err
	}
	return resolved, rel, nil
}

// ResolveDir maps a working-directory argument inside the workspace,
// defaulting empty input to the workspace root. The directory must exist.
func (w *Workspace) ResolveDir(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if w == nil || w.root == "" {
			return "", errors.New("workspace is unavailable")
		}
		return w.root, nil
	}
	abs, _, err := w.Resolve(trimmed)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", errors.New("working directory does not exist")
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("working directory is not a directory")
	}
	return abs, nil
}

func (w *Workspace) checkDenied(rel string) error {
	// Operator secrets are never reachable through workspace tools, even
	// though they live inside a profile directory.
	parts := strings.Split(rel, string(filepath.Separator))
	first := ""
	if len(parts) > 0 {
		first = parts[0]
	}
	base := filepath.Base(rel)
	switch base {
	case "config.yaml", ".env", "state.db", "state.db-wal", "state.db-shm", "state.db-journal", "state.db-journal-mode":
		return errors.New("path is not accessible")
	}
	switch first {
	case "mcp-tokens":
		return errors.New("path is not accessible")
	case "profiles":
		if w.isDefault {
			return errors.New("path is not accessible")
		}
	}
	// Memory-tool state bypasses entry validation and locking.
	if first == "memories" {
		return errors.New("path is not accessible")
	}
	if base == "SOUL.md" {
		return errors.New("path is not accessible")
	}
	return nil
}

// NoteFullRead records a complete read of abs for requestID.
func (w *Workspace) NoteFullRead(requestID, abs, hash string) {
	if w == nil || requestID == "" || abs == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.reads) > 200 {
		w.reads = make(map[string]map[string]string)
	}
	paths := w.reads[requestID]
	if paths == nil {
		paths = make(map[string]string)
		w.reads[requestID] = paths
	}
	paths[abs] = hash
}

// HasFullRead reports whether requestID holds a current full read of abs.
func (w *Workspace) HasFullRead(requestID, abs, currentHash string) bool {
	if w == nil || requestID == "" || abs == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reads[requestID][abs] == currentHash && currentHash != ""
}

// NoteWrite records a completed write so a later write in the same request
// chains without another read.
func (w *Workspace) NoteWrite(requestID, abs, hash string) {
	w.NoteFullRead(requestID, abs, hash)
}

// FileHash returns the hex sha256 of data.
func FileHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validateRoot(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return errors.New("workspace root is unavailable")
	}
	path := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, path), path) {
		if part == "" {
			continue
		}
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return errors.New("workspace root is unavailable")
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("workspace root is unavailable")
		}
		if path == abs && info.Mode().Perm()&0077 != 0 {
			return errors.New("workspace root is unavailable")
		}
	}
	return nil
}

func mkdirPrivateAll(dir string) error {
	path := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(dir, path), path) {
		if part == "" {
			continue
		}
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(path)
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("workspace directory is unsafe")
		}
	}
	return nil
}

// resolveNoEscape resolves symlinks on the nearest existing ancestor and
// rejects escapes back outside cwd.
func resolveNoEscape(cwd, abs string) (string, error) {
	target := abs
	for {
		if _, err := os.Lstat(target); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("path is unavailable")
		}
		parent := filepath.Dir(target)
		if parent == target {
			break
		}
		target = parent
	}
	resolvedBase, err := filepath.EvalSymlinks(target)
	if err != nil {
		// Unresolvable symlinks fail closed; missing paths keep lexical form.
		if errors.Is(err, os.ErrNotExist) {
			resolvedBase = target
		} else {
			return "", errors.New("path is unavailable")
		}
	}
	if target != abs {
		rel, err := filepath.Rel(target, abs)
		if err != nil {
			return "", errors.New("path is unavailable")
		}
		if rel == "." {
			abs = resolvedBase
		} else {
			// A symlink on the ancestor already resolved; rejoin the tail
			// lexically and let the caller re-check containment.
			abs = filepath.Join(resolvedBase, rel)
		}
	} else {
		abs = resolvedBase
	}
	// Reject a symlink whose target points outside the workspace even when
	// the lexical path looked contained.
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("path is unavailable")
	}
	return abs, nil
}
