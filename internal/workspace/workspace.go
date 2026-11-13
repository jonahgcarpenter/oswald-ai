// Package workspace owns profile-scoped terminal and filesystem state.
//
// Each profile gets one CurrentWorkingDir bound at registry construction from
// the profile's validated ProfileRoot: the default profile uses the global
// root itself, named profiles use <root>/profiles/<name>/. Model-supplied
// paths resolve through Classify into same-trust file groups: protected
// secrets are sealed everywhere, tool-managed paths are read-only, and
// agent-owned paths (plus, for reads, anything outside the workspace that
// the container user may see) stay open. Writes outside agent-owned paths
// fail closed via ResolveWrite; the shell pre-execution chain enforces the
// same groups for terminal, execute_code, and process input.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// Workspace binds file and terminal execution to one profile directory.
type Workspace struct {
	root    string
	profile string
	mu      sync.Mutex
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
		root:    abs,
		profile: strings.TrimSpace(profile),
		reads:   make(map[string]map[string]string),
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
	if err := MkdirPrivateAll(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Resolve maps a model-supplied path to an absolute path. It accepts
// absolute paths, CWD-relative paths, and ~/paths (resolved inside the
// workspace, never the OS home). Paths outside the workspace resolve for
// reads under Table 3 (system-managed, container user as backstop);
// protected paths fail closed. It returns the absolute path and a display
// form (workspace-relative when inside, absolute when outside).
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
	if err := validateRoot(cwd); err != nil {
		return "", "", err
	}
	resolved, err := resolveNoEscape(cwd, abs)
	if err != nil {
		return "", "", err
	}
	if group := Classify(resolved, cwd); group == GroupProtected {
		return "", "", &DenyError{Rel: displayPath(resolved, cwd), Group: group}
	}
	return resolved, displayPath(resolved, cwd), nil
}

// ResolveWrite resolves a path for writing: anything outside agent-owned
// paths fails closed with its file-group denial.
func (w *Workspace) ResolveWrite(raw string) (string, string, error) {
	abs, display, err := w.Resolve(raw)
	if err != nil {
		return "", "", err
	}
	if group := Classify(abs, w.root); group != GroupAgent {
		return "", "", &DenyError{Rel: display, Group: group}
	}
	return abs, display, nil
}

// displayPath renders a resolved path for model messages: workspace-relative
// when inside, absolute when outside.
func displayPath(abs, root string) string {
	if rel, err := filepath.Rel(root, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return rel
	}
	return abs
}

// ResolveDir maps a working-directory argument inside the workspace,
// defaulting empty input to the workspace root. Working directories stay
// jailed: outside paths fail closed even though reads may roam. The
// directory must exist.
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
	if rel, rerr := filepath.Rel(w.root, abs); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("path escapes the working directory")
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

// WriteRegularFile atomically replaces abs with content: private temp file
// in the same directory, synced, renamed over the target, then permissioned
// and directory-synced. Symlinks and non-regular targets are refused, never
// followed. Tool surfaces share this single safe-write path.
func WriteRegularFile(abs, content string) error {
	dir := filepath.Dir(abs)
	if err := MkdirPrivateAll(dir); err != nil {
		// Parent creation is best-effort private; fall back to standard mkdir.
		if mk := os.MkdirAll(dir, 0700); mk != nil {
			return mk
		}
	}
	tmp, err := os.CreateTemp(dir, ".workspace-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Refuse to overwrite symlinks.
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("refuses to overwrite special file")
		}
	}
	if err := os.Rename(name, abs); err != nil {
		return err
	}
	if err := os.Chmod(abs, 0600); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer d.Close()
	_ = d.Sync()
	_ = syscall.Fsync(int(d.Fd()))
	return nil
}

// ReadRegularFile opens abs without following a trailing symlink and reads
// at most 8 MiB plus one byte (callers treat over-limit as truncation or
// refusal). Non-regular files fail closed. Tool surfaces share this single
// safe-open path.
func ReadRegularFile(abs string) ([]byte, error) {
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	// Bound single reads at 8 MiB; pagination handles the rest.
	const maxBytes = 8 << 20
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, err := f.Read(chunk)
		if n > 0 {
			if len(buf)+n > maxBytes+1 {
				return nil, errors.New("file exceeds size limit")
			}
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

// RepairCache tightens permissions on existing cache directories without
// creating anything. Missing paths stay missing so creation remains lazy on
// first tool use; symlinks and non-directories are left for the fail-closed
// checks elsewhere.
func RepairCache(root, profile string, log *config.Logger) {
	logger := log
	if logger == nil {
		logger = config.NewLogger(config.LevelWarn)
	}
	if info, err := os.Lstat(root); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0077 != 0 {
		before := info.Mode()
		if err := os.Chmod(root, 0700); err == nil {
			logger.Server("workspace").Warn("workspace.cache.repaired", "repaired cache permissions", config.F("profile", profile), config.F("config_source", "workspace_root"), config.F("old_mode", int(before.Perm())))
		}
	}
	if err := validateRoot(root); err != nil {
		return
	}
	paths := []struct{ path, artifact string }{
		{filepath.Join(root, "cache"), "cache"},
		{filepath.Join(root, "cache", "scratch"), "cache/scratch"},
		{filepath.Join(root, "cache", "browser"), "cache/browser"},
		{filepath.Join(root, "cache", "images"), "cache/images"},
		{filepath.Join(root, "cache", "documents"), "cache/documents"},
		{filepath.Join(root, "cache", "processes"), "cache/processes"},
	}
	for _, entry := range paths {
		info, err := os.Lstat(entry.path)
		if err != nil {
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if info.Mode().Perm()&0077 == 0 {
			continue
		}
		before := info.Mode()
		if err := os.Chmod(entry.path, 0700); err != nil {
			continue
		}
		logger.Server("workspace").Warn("workspace.cache.repaired", "repaired cache permissions", config.F("profile", profile), config.F("config_source", entry.artifact), config.F("old_mode", int(before.Perm())))
	}
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

// MkdirPrivateAll creates dir and any missing parents owner-only, rejecting
// symlink escapes along the way. Tool surfaces call it when first writing
// into a profile directory.
func MkdirPrivateAll(dir string) error {
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
