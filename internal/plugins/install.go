package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// Install bounds. Clones never prompt for credentials: GIT_TERMINAL_PROMPT=0
// fails closed so unattended installs cannot hang on auth.
const (
	installCloneTimeout = 5 * time.Minute
	installRevTimeout   = time.Minute
	maxInstallMetadata  = 16 * 1024
)

var (
	fullSHARE   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	ownerRepoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// InstallSource is a parsed `plugins install` argument.
type InstallSource struct {
	// Kind is git or local.
	Kind string
	// URL is the normalized git URL or absolute local path.
	URL string
}

// InstallResult reports a completed install.
type InstallResult struct {
	// Key is the canonical published plugin key.
	Key string
	// Revision is the resolved git HEAD SHA, empty for local installs.
	Revision string
	// Pinned reports whether --ref pinned the install.
	Pinned bool
	// AlreadyInstalled reports an idempotent no-op: the published tree
	// already satisfies the request, so nothing was copied or rewritten.
	AlreadyInstalled bool
}

// InstallMetadata is the provenance record written beside installed code.
// It carries source and revision only, never config values or secrets.
type InstallMetadata struct {
	Source      string `json:"source"`
	URL         string `json:"url"`
	Ref         string `json:"ref,omitempty"`
	Revision    string `json:"revision,omitempty"`
	Pinned      bool   `json:"pinned"`
	Subdir      string `json:"subdir,omitempty"`
	InstalledAt string `json:"installed_at"`
}

// ParseInstallSource normalizes a user-supplied install argument: owner/repo
// shorthand, git URLs (https or git@), file:// URLs, and local paths. Anything
// else fails closed.
func ParseInstallSource(raw string) (InstallSource, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > 1024 || strings.ContainsAny(trimmed, "\r\n\x00") || strings.ContainsAny(trimmed, " \t") {
		return InstallSource{}, fmt.Errorf("invalid plugin source")
	}
	if ownerRepoRE.MatchString(trimmed) && !strings.Contains(trimmed, ":") {
		owner, repo, _ := strings.Cut(trimmed, "/")
		if owner != "." && owner != ".." && repo != "." && repo != ".." {
			return InstallSource{Kind: "git", URL: "https://github.com/" + trimmed + ".git"}, nil
		}
		return InstallSource{}, fmt.Errorf("invalid plugin source")
	}
	lowered := strings.ToLower(trimmed)
	if strings.HasPrefix(lowered, "https://") || strings.HasPrefix(lowered, "http://") {
		if strings.HasPrefix(lowered, "http://") {
			return InstallSource{}, fmt.Errorf("plugin git URLs must use https or ssh")
		}
		return InstallSource{Kind: "git", URL: trimmed}, nil
	}
	if strings.HasPrefix(trimmed, "git@") || strings.HasPrefix(lowered, "ssh://") {
		return InstallSource{}, fmt.Errorf("ssh plugin sources are not supported in this build")
	}
	path := trimmed
	if strings.HasPrefix(lowered, "file://") {
		path = strings.TrimPrefix(trimmed, "file://")
	}
	if !filepath.IsAbs(path) {
		return InstallSource{}, fmt.Errorf("plugin paths must be absolute or file:// URLs")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return InstallSource{}, fmt.Errorf("plugin source directory is unavailable")
	}
	return InstallSource{Kind: "local", URL: filepath.Clean(path)}, nil
}

// ValidateInstallRef rejects anything but a full 40-hex commit SHA (or empty
// for unpinned installs). Tags and branches are never accepted.
func ValidateInstallRef(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	if !fullSHARE.MatchString(strings.TrimSpace(ref)) {
		return fmt.Errorf("plugin --ref must be a full 40-character commit SHA")
	}
	return nil
}

// ValidateInstallSubdir rejects absolute paths and escapes so monorepo
// installs stay inside the cloned tree.
func ValidateInstallSubdir(subdir string) (string, error) {
	trimmed := strings.TrimSpace(subdir)
	if trimmed == "" {
		return "", nil
	}
	if filepath.IsAbs(trimmed) || strings.Contains(trimmed, "..") || strings.ContainsRune(trimmed, 0) || len(trimmed) > 512 {
		return "", fmt.Errorf("invalid plugin subdirectory")
	}
	return filepath.Clean(trimmed), nil
}

// InstallFromURL clones or copies a plugin into the shared plugins-available
// tree and records provenance metadata. Code writes require default scope;
// callers check RequireDefaultScope before invoking. Force replaces an
// existing install, backing up the previous tree first.
func InstallFromURL(ctx context.Context, globalRoot, raw, ref, subdir string, force bool, log *config.Logger) (InstallResult, error) {
	source, err := ParseInstallSource(raw)
	if err != nil {
		return InstallResult{}, err
	}
	if err := ValidateInstallRef(ref); err != nil {
		return InstallResult{}, err
	}
	cleanSubdir, err := ValidateInstallSubdir(subdir)
	if err != nil {
		return InstallResult{}, err
	}
	ref = strings.TrimSpace(ref)
	if source.Kind == "local" && ref != "" {
		return InstallResult{}, fmt.Errorf("plugin --ref applies to git sources only")
	}
	available := UserPluginsDir(globalRoot)
	if err := os.MkdirAll(available, 0755); err != nil {
		return InstallResult{}, fmt.Errorf("prepare plugin directory: %w", err)
	}
	staging, err := os.MkdirTemp("", "oswald-plugin-*")
	if err != nil {
		return InstallResult{}, fmt.Errorf("stage plugin install: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	checkout := staging
	var revision string
	switch source.Kind {
	case "git":
		cloneDir := filepath.Join(staging, "repo")
		if err := gitClone(ctx, source.URL, cloneDir); err != nil {
			return InstallResult{}, err
		}
		revision, err = gitCheckoutPin(ctx, cloneDir, ref)
		if err != nil {
			return InstallResult{}, err
		}
		checkout = cloneDir
	case "local":
		checkout = source.URL
	default:
		return InstallResult{}, fmt.Errorf("invalid plugin source")
	}
	pluginDir := checkout
	if cleanSubdir != "" {
		pluginDir = filepath.Join(checkout, filepath.FromSlash(cleanSubdir))
	}
	manifest, err := ParseManifestFile(pluginDir, "user")
	if err != nil {
		return InstallResult{}, err
	}
	// The published directory is always the manifest name so the key stays
	// canonical: the repo or local path is only a transport.
	dest := filepath.Join(available, manifest.Name)
	if _, err := os.Lstat(dest); err == nil {
		if !force {
			if result, done, err := existingInstallState(globalRoot, manifest.Name, source, cleanSubdir, ref); err != nil {
				return InstallResult{}, err
			} else if done {
				return result, nil
			}
		}
		backup, err := backupInstall(globalRoot, manifest.Name)
		if err != nil {
			return InstallResult{}, err
		}
		_ = backup
		if err := os.RemoveAll(dest); err != nil {
			return InstallResult{}, fmt.Errorf("replace plugin install: %w", err)
		}
	}
	if err := copyPluginTree(pluginDir, dest); err != nil {
		return InstallResult{}, err
	}
	meta := InstallMetadata{
		Source:      source.Kind,
		URL:         source.URL,
		Ref:         ref,
		Revision:    revision,
		Pinned:      ref != "",
		Subdir:      cleanSubdir,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteInstallMetadata(dest, meta); err != nil {
		_ = os.RemoveAll(dest)
		return InstallResult{}, err
	}
	if log != nil {
		log.Server("plugin.install").Debug("plugin.install.completed", "installed plugin", config.F("plugin", manifest.Name), config.F("pinned", meta.Pinned))
	}
	return InstallResult{Key: manifest.Name, Revision: revision, Pinned: meta.Pinned}, nil
}

// existingInstallState compares a staged install request against the
// published tree. It returns done=true with an AlreadyInstalled result when
// the published tree already satisfies the request (no copy needed).
// Otherwise it returns a guiding error: conflicting sources must be replaced
// with --force, and revision moves go through update.
func existingInstallState(globalRoot, name string, source InstallSource, subdir, ref string) (InstallResult, bool, error) {
	meta, err := ReadInstallMetadata(globalRoot, name)
	if err != nil {
		return InstallResult{}, false, fmt.Errorf("plugin %q is already installed but unmanaged (use --force to replace)", name)
	}
	if meta.Source != source.Kind || meta.URL != source.URL || meta.Subdir != subdir {
		return InstallResult{}, false, fmt.Errorf("plugin %q is already installed from a different source (use --force to replace)", name)
	}
	if ref != "" && ref != meta.Revision {
		if source.Kind != "git" {
			return InstallResult{}, false, fmt.Errorf("plugin %q is already installed (use --force to replace)", name)
		}
		return InstallResult{}, false, fmt.Errorf("plugin %q is installed at another revision (use plugins update to move it)", name)
	}
	return InstallResult{Key: name, Revision: meta.Revision, Pinned: meta.Pinned, AlreadyInstalled: true}, true, nil
}

// MetadataPath returns the provenance file for one installed plugin key.
func MetadataPath(globalRoot, key string) (string, error) {
	if !ValidPluginKey(key) {
		return "", fmt.Errorf("invalid plugin key")
	}
	return filepath.Join(UserPluginsDir(globalRoot), filepath.FromSlash(key), ".install-metadata.json"), nil
}

// WriteInstallMetadata records provenance beside installed code, owner-only.
func WriteInstallMetadata(dest string, meta InstallMetadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plugin metadata: %w", err)
	}
	if len(data) > maxInstallMetadata {
		return fmt.Errorf("plugin metadata exceeds size limit")
	}
	if err := os.WriteFile(filepath.Join(dest, ".install-metadata.json"), append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write plugin metadata: %w", err)
	}
	return nil
}

// ReadInstallMetadata loads provenance for one installed key. Unmanaged
// directories (hand-placed code) report os.ErrNotExist.
func ReadInstallMetadata(globalRoot, key string) (InstallMetadata, error) {
	path, err := MetadataPath(globalRoot, key)
	if err != nil {
		return InstallMetadata{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return InstallMetadata{}, err
	}
	if len(data) > maxInstallMetadata+1 {
		return InstallMetadata{}, fmt.Errorf("plugin metadata exceeds size limit")
	}
	var meta InstallMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return InstallMetadata{}, fmt.Errorf("plugin metadata is not valid")
	}
	return meta, nil
}

// backupInstall moves the previous tree aside. Edits to tracked files are
// preserved in place by the move; nothing is merged.
func backupInstall(globalRoot, name string) (string, error) {
	backupRoot := filepath.Join(globalRoot, "plugins-backup")
	if err := os.MkdirAll(backupRoot, 0755); err != nil {
		return "", fmt.Errorf("prepare plugin backup: %w", err)
	}
	dest := filepath.Join(backupRoot, name+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(filepath.Join(UserPluginsDir(globalRoot), name), dest); err != nil {
		return "", fmt.Errorf("back up plugin install: %w", err)
	}
	return dest, nil
}

// copyPluginTree copies a validated plugin directory, skipping .git and
// metadata files so nested repos never nest.
func copyPluginTree(src, dest string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			if err == nil {
				return os.MkdirAll(dest, 0755)
			}
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == ".install-metadata.json" {
			return nil
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("plugin file is not a regular file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(data) > 1024*1024 {
			return fmt.Errorf("plugin file exceeds size limit")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode&0111 != 0 {
			mode = 0755
		} else {
			mode = 0644
		}
		return os.WriteFile(target, data, mode)
	})
}

// gitEnv returns the minimal, non-interactive git environment. No credentials
// pass through here: ambient helpers and agents own auth, and prompting is
// disabled so installs fail closed instead of hanging.
func gitEnv(tmp string) []string {
	return []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"LANG=C.UTF-8",
		"TMPDIR=" + tmp,
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	}
}

func gitClone(ctx context.Context, url, dest string) error {
	runCtx, cancel := context.WithTimeout(ctx, installCloneTimeout)
	defer cancel()
	tmp, err := os.MkdirTemp("", "oswald-git-*")
	if err != nil {
		return fmt.Errorf("stage plugin fetch: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	cmd := exec.CommandContext(runCtx, "git", "clone", "--depth", "1", url, dest)
	cmd.Env = gitEnv(tmp)
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = output
		if runCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("plugin fetch timed out")
		}
		return fmt.Errorf("plugin fetch failed")
	}
	return nil
}

// gitCheckoutPin checks out ref when pinned, or resolves the fetched HEAD.
// It returns the resolved HEAD SHA.
func gitCheckoutPin(ctx context.Context, dir, ref string) (string, error) {
	run := func(args ...string) (string, error) {
		runCtx, cancel := context.WithTimeout(ctx, installRevTimeout)
		defer cancel()
		tmp, err := os.MkdirTemp("", "oswald-git-*")
		if err != nil {
			return "", fmt.Errorf("stage plugin fetch: %w", err)
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		cmd := exec.CommandContext(runCtx, "git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv(tmp)
		output, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("plugin fetch failed")
		}
		return strings.TrimSpace(string(output)), nil
	}
	if ref != "" {
		if _, err := run("fetch", "origin", ref); err != nil {
			return "", fmt.Errorf("plugin ref is unavailable")
		}
		if _, err := run("checkout", "--detach", ref); err != nil {
			return "", fmt.Errorf("plugin ref is unavailable")
		}
		head, err := run("rev-parse", "HEAD")
		if err != nil {
			return "", fmt.Errorf("plugin fetch failed")
		}
		if head != ref {
			return "", fmt.Errorf("plugin ref mismatch")
		}
		return head, nil
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("plugin fetch failed")
	}
	if !fullSHARE.MatchString(head) {
		return "", fmt.Errorf("plugin fetch failed")
	}
	return head, nil
}

// UpdateFromURL refreshes one managed install in place. Pinned installs need
// an explicit --ref; unpinned installs follow the remote HEAD.
func UpdateFromURL(ctx context.Context, globalRoot, key, ref string, log *config.Logger) (InstallResult, error) {
	if !ValidPluginKey(key) || strings.Contains(key, "/") {
		return InstallResult{}, fmt.Errorf("invalid plugin key")
	}
	if err := ValidateInstallRef(ref); err != nil {
		return InstallResult{}, err
	}
	ref = strings.TrimSpace(ref)
	dir := filepath.Join(UserPluginsDir(globalRoot), key)
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil || !info.IsDir() {
		return InstallResult{}, fmt.Errorf("plugin %q is not a managed git install", key)
	}
	meta, err := ReadInstallMetadata(globalRoot, key)
	if err != nil {
		return InstallResult{}, fmt.Errorf("plugin %q has no install provenance; remove and reinstall it", key)
	}
	if meta.Pinned && ref == "" {
		return InstallResult{}, fmt.Errorf("plugin %q is pinned; choose a new commit explicitly", key)
	}
	if meta.Source != "git" {
		return InstallResult{}, fmt.Errorf("plugin %q is not a git install", key)
	}
	runCtx, cancel := context.WithTimeout(ctx, installCloneTimeout)
	defer cancel()
	tmp, err := os.MkdirTemp("", "oswald-git-*")
	if err != nil {
		return InstallResult{}, fmt.Errorf("stage plugin fetch: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env := gitEnv(tmp)
	fetch := exec.CommandContext(runCtx, "git", "fetch", "origin")
	fetch.Dir = dir
	fetch.Env = env
	if output, err := fetch.CombinedOutput(); err != nil {
		_ = output
		return InstallResult{}, fmt.Errorf("plugin fetch failed")
	}
	target := ref
	if target == "" {
		head := exec.CommandContext(runCtx, "git", "rev-parse", "origin/HEAD")
		head.Dir = dir
		head.Env = env
		output, err := head.Output()
		if err != nil {
			return InstallResult{}, fmt.Errorf("plugin fetch failed")
		}
		target = strings.TrimSpace(string(output))
	} else {
		fetchRef := exec.CommandContext(runCtx, "git", "fetch", "origin", target)
		fetchRef.Dir = dir
		fetchRef.Env = env
		if output, err := fetchRef.CombinedOutput(); err != nil {
			_ = output
			return InstallResult{}, fmt.Errorf("plugin ref is unavailable")
		}
	}
	checkout := exec.CommandContext(runCtx, "git", "checkout", "--detach", target)
	checkout.Dir = dir
	checkout.Env = env
	if output, err := checkout.CombinedOutput(); err != nil {
		_ = output
		return InstallResult{}, fmt.Errorf("plugin ref is unavailable")
	}
	revParse := exec.CommandContext(runCtx, "git", "rev-parse", "HEAD")
	revParse.Dir = dir
	revParse.Env = env
	output, err := revParse.Output()
	if err != nil {
		return InstallResult{}, fmt.Errorf("plugin fetch failed")
	}
	revision := strings.TrimSpace(string(output))
	if ref != "" && revision != ref {
		return InstallResult{}, fmt.Errorf("plugin ref mismatch")
	}
	if _, err := ParseManifestFile(dir, "user"); err != nil {
		return InstallResult{}, err
	}
	meta.Revision = revision
	meta.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	if ref != "" {
		meta.Ref = ref
		meta.Pinned = true
	} else {
		meta.Pinned = false
	}
	if err := WriteInstallMetadata(dir, meta); err != nil {
		return InstallResult{}, err
	}
	if log != nil {
		log.Server("plugin.install").Debug("plugin.install.updated", "updated plugin", config.F("plugin", key))
	}
	return InstallResult{Key: key, Revision: revision, Pinned: meta.Pinned}, nil
}

// RemoveInstall deletes shared code for one plugin key. Config cleanup is the
// caller's job per profile, and callers refuse first when other profiles still
// reference the key. Per-profile storage removal is also the caller's job
// (--full); this removes code only.
func RemoveInstall(globalRoot, key string) error {
	if !ValidPluginKey(key) || strings.Contains(key, "/") {
		return fmt.Errorf("invalid plugin key")
	}
	dir := filepath.Join(UserPluginsDir(globalRoot), key)
	if _, err := os.Lstat(dir); err != nil {
		return fmt.Errorf("unknown plugin %q", key)
	}
	return os.RemoveAll(dir)
}
