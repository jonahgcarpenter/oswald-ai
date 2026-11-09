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
	// DepsInstalled reports whether this invocation installed (or repaired)
	// the node sidecar. False when there is no sidecar, --no-deps was set,
	// or an idempotent hit found the binary already present.
	DepsInstalled bool
}

// InstallOptions carries the optional inputs to InstallFromURL beyond the
// source argument. NodePackage and NodeVersion must be set together or not
// at all; they supply the sidecar for repos whose manifest predates the
// node_package convention. NoDeps records the sidecar without installing it.
type InstallOptions struct {
	Ref         string
	Subdir      string
	Force       bool
	NodePackage string
	NodeVersion string
	NoDeps      bool
}

// InstallMetadata is the provenance record written beside installed code.
// It carries source, revision, and the resolved node sidecar — never config
// values or secrets. install-deps, bin-path, and exec fall back to the
// recorded node fields when the published manifest declares none.
type InstallMetadata struct {
	Source       string `json:"source"`
	URL          string `json:"url"`
	Ref          string `json:"ref,omitempty"`
	Revision     string `json:"revision,omitempty"`
	Pinned       bool   `json:"pinned"`
	Subdir       string `json:"subdir,omitempty"`
	NodePackage  string `json:"node_package,omitempty"`
	NodeVersion  string `json:"node_version,omitempty"`
	NodeLockfile bool   `json:"node_lockfile,omitempty"`
	InstalledAt  string `json:"installed_at"`
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

// NodeSpec is a resolved node sidecar: an exact package pin or a lockfile
// install from the plugin's own root package.json.
type NodeSpec struct {
	Package  string
	Version  string
	Lockfile bool
}

// Present reports whether a sidecar install is needed at all.
func (s NodeSpec) Present() bool {
	return s.Package != "" || s.Lockfile
}

// resolveNodeSidecar decides the sidecar for a fresh install. Explicit flags
// supply it for repos whose manifest predates the node_package convention;
// otherwise the manifest declaration wins; otherwise a root package.json
// with a lockfile falls back to npm ci. Flags conflicting with a manifest
// declaration fail closed instead of silently winning or losing.
func resolveNodeSidecar(manifest *Manifest, flagPackage, flagVersion, pluginDir string) (NodeSpec, error) {
	flagPackage = strings.TrimSpace(flagPackage)
	flagVersion = strings.TrimSpace(flagVersion)
	if flagPackage != "" || flagVersion != "" {
		if flagPackage == "" || flagVersion == "" {
			return NodeSpec{}, fmt.Errorf("plugin --node-package and --node-version must be set together")
		}
		if err := ValidateNodeDeps(&Manifest{Name: manifest.Name, NodePackage: flagPackage, NodeVersion: flagVersion}); err != nil {
			return NodeSpec{}, err
		}
		if strings.TrimSpace(manifest.NodePackage) != "" &&
			(manifest.NodePackage != flagPackage || manifest.NodeVersion != flagVersion) {
			return NodeSpec{}, fmt.Errorf("plugin %q already declares node_package %q; flags conflict with the manifest", manifest.Name, manifest.NodePackage)
		}
		return NodeSpec{Package: flagPackage, Version: flagVersion}, nil
	}
	if strings.TrimSpace(manifest.NodePackage) != "" {
		return NodeSpec{Package: manifest.NodePackage, Version: manifest.NodeVersion}, nil
	}
	if hasLockfileSidecar(pluginDir) {
		return NodeSpec{Lockfile: true}, nil
	}
	return NodeSpec{}, nil
}

// resolveUpdateNodeSpec decides the sidecar for an update. A fresh manifest
// declaration wins; otherwise the recorded install-time spec carries forward.
func resolveUpdateNodeSpec(manifest *Manifest, meta InstallMetadata) NodeSpec {
	if strings.TrimSpace(manifest.NodePackage) != "" {
		return NodeSpec{Package: manifest.NodePackage, Version: manifest.NodeVersion}
	}
	return NodeSpec{Package: meta.NodePackage, Version: meta.NodeVersion, Lockfile: meta.NodeLockfile}
}

// hasLockfileSidecar reports whether dir carries a root package.json with a
// lockfile, making it installable via npm ci with fully pinned deps. A
// package.json without a lockfile is correctly ignored: unpinned installs
// never run.
func hasLockfileSidecar(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	if info, err := os.Stat(filepath.Join(dir, "package.json")); err != nil || !info.Mode().IsRegular() {
		return false
	}
	for _, lock := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
		if info, err := os.Stat(filepath.Join(dir, lock)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// ResolveNodeSpec decides the sidecar for an already-installed plugin:
// the published manifest declaration wins, otherwise the recorded
// install-time metadata carries. It backs install-deps after the fact.
func ResolveNodeSpec(manifest Manifest, meta InstallMetadata) (NodeSpec, bool) {
	if strings.TrimSpace(manifest.NodePackage) != "" {
		return NodeSpec{Package: manifest.NodePackage, Version: manifest.NodeVersion}, true
	}
	if strings.TrimSpace(meta.NodePackage) != "" {
		return NodeSpec{Package: meta.NodePackage, Version: meta.NodeVersion}, true
	}
	if meta.NodeLockfile {
		return NodeSpec{Lockfile: true}, true
	}
	return NodeSpec{}, false
}

// EffectiveNodeBin resolves the binary name for display and exec: explicit,
// else the manifest default, else the recorded package base. Lockfile-only
// installs resolve to "" and need an explicit --bin.
func EffectiveNodeBin(manifest Manifest, meta InstallMetadata, explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return strings.TrimSpace(explicit)
	}
	if bin := DefaultNodeBin(manifest); bin != "" {
		return bin
	}
	base := meta.NodePackage
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return base
}

// sidecarMissing reports whether the installed sidecar binary (or, for
// lockfile-only installs with no known bin, the node_modules tree) is absent.
func sidecarMissing(profileRoot, key, bin string) bool {
	if bin != "" {
		_, err := NodeBinPath(profileRoot, key, bin)
		return err != nil
	}
	info, err := os.Stat(filepath.Join(PluginCodeDir(profileRoot, key), "node_modules"))
	return err != nil || !info.IsDir()
}

// ProfileHasPlugin reports whether a profile has a code install for key.
func ProfileHasPlugin(profileRoot, key string) bool {
	if !ValidPluginKey(key) || strings.TrimSpace(profileRoot) == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(PluginCodeDir(profileRoot, key), "plugin.yaml"))
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return true
}

// InstallFromURL clones or copies a plugin into the profile
// <profileRoot>/plugins/<name>/code tree and records provenance metadata.
// Presence means enabled; there is no separate enable step. Force replaces
// the code tree, backing up the previous code first; data/ is never touched.
// The node sidecar installs as part of the invocation unless NoDeps opts out;
// a sidecar failure rolls the code tree back so installs stay atomic.
func InstallFromURL(ctx context.Context, profileRoot, raw string, opts InstallOptions, log *config.Logger) (InstallResult, error) {
	source, err := ParseInstallSource(raw)
	if err != nil {
		return InstallResult{}, err
	}
	if err := ValidateInstallRef(opts.Ref); err != nil {
		return InstallResult{}, err
	}
	cleanSubdir, err := ValidateInstallSubdir(opts.Subdir)
	if err != nil {
		return InstallResult{}, err
	}
	ref := strings.TrimSpace(opts.Ref)
	if source.Kind == "local" {
		if _, err := os.Stat(filepath.Join(source.URL, ".git")); err == nil {
			// Local git checkout: clone it so the install records a
			// revision and supports update. File clones need no network.
			source = InstallSource{Kind: "git", URL: source.URL}
		} else if ref != "" {
			return InstallResult{}, fmt.Errorf("plugin --ref applies to git sources only")
		}
	}
	if strings.TrimSpace(profileRoot) == "" {
		return InstallResult{}, fmt.Errorf("plugin home is unavailable")
	}
	available := ProfilePluginsDir(profileRoot)
	if err := os.MkdirAll(available, 0700); err != nil {
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
	manifest, err := ParseManifestFile(pluginDir, "profile")
	if err != nil {
		return InstallResult{}, err
	}
	spec, err := resolveNodeSidecar(manifest, opts.NodePackage, opts.NodeVersion, pluginDir)
	if err != nil {
		return InstallResult{}, err
	}
	// The published directory is always the manifest name so the key stays
	// canonical: the repo or local path is only a transport.
	dest := PluginCodeDir(profileRoot, manifest.Name)
	if _, err := os.Lstat(dest); err == nil {
		if !opts.Force {
			if result, done, err := existingInstallState(profileRoot, manifest.Name, source, cleanSubdir, ref, spec); err != nil {
				return InstallResult{}, err
			} else if done {
				if repaired, err := repairSidecar(ctx, profileRoot, manifest, spec, opts.NoDeps, log); err != nil {
					return InstallResult{}, err
				} else if repaired {
					result.DepsInstalled = true
				}
				return result, nil
			}
		}
		backup, err := backupInstall(profileRoot, manifest.Name)
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
	depsInstalled := false
	if spec.Present() && !opts.NoDeps {
		bin := EffectiveNodeBin(*manifest, InstallMetadata{NodePackage: spec.Package}, "")
		if err := installSidecarInto(ctx, profileRoot, manifest.Name, bin, spec, dest, log); err != nil {
			_ = os.RemoveAll(dest)
			return InstallResult{}, err
		}
		depsInstalled = true
	}
	meta := InstallMetadata{
		Source:       source.Kind,
		URL:          source.URL,
		Ref:          ref,
		Revision:     revision,
		Pinned:       ref != "",
		Subdir:       cleanSubdir,
		NodePackage:  spec.Package,
		NodeVersion:  spec.Version,
		NodeLockfile: spec.Lockfile,
		InstalledAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteInstallMetadata(dest, meta); err != nil {
		_ = os.RemoveAll(dest)
		return InstallResult{}, err
	}
	if log != nil {
		log.Server("plugin.install").Debug("plugin.install.completed", "installed plugin", config.F("plugin", manifest.Name), config.F("pinned", meta.Pinned))
	}
	return InstallResult{Key: manifest.Name, Revision: revision, Pinned: meta.Pinned, DepsInstalled: depsInstalled}, nil
}

// installSidecarInto installs the resolved sidecar into an already-published
// plugin code directory: an exact npm pin, or npm ci for lockfile fallbacks.
// dir is the install target (the published code tree, or staged temp on update).
func installSidecarInto(ctx context.Context, profileRoot, key, bin string, spec NodeSpec, dir string, log *config.Logger) error {
	if spec.Lockfile {
		return InstallNodeLockfile(ctx, profileRoot, key, dir, log)
	}
	return InstallNodePackage(ctx, profileRoot, key, bin, spec.Package+"@"+spec.Version, dir, log)
}

// repairSidecar verifies the sidecar on the idempotent path and installs it
// when the binary (or lockfile tree) is missing. It reports whether a repair
// ran. With NoDeps the missing sidecar stays missing: the operator opted out.
func repairSidecar(ctx context.Context, profileRoot string, manifest *Manifest, spec NodeSpec, noDeps bool, log *config.Logger) (bool, error) {
	if !spec.Present() || noDeps {
		return false, nil
	}
	meta, err := ReadInstallMetadata(profileRoot, manifest.Name)
	if err != nil {
		meta = InstallMetadata{}
	}
	if !sidecarMissing(profileRoot, manifest.Name, EffectiveNodeBin(*manifest, meta, "")) {
		return false, nil
	}
	dest := PluginCodeDir(profileRoot, manifest.Name)
	bin := EffectiveNodeBin(*manifest, InstallMetadata{NodePackage: spec.Package}, "")
	if err := installSidecarInto(ctx, profileRoot, manifest.Name, bin, spec, dest, log); err != nil {
		return false, err
	}
	return true, nil
}

// existingInstallState compares a staged install request against the
// published code tree. It returns done=true with an AlreadyInstalled result when
// the published tree already satisfies the request (no copy needed).
// Otherwise it returns a guiding error: conflicting sources or node sidecars
// must be replaced with --force, and revision moves go through update.
func existingInstallState(profileRoot, name string, source InstallSource, subdir, ref string, spec NodeSpec) (InstallResult, bool, error) {
	meta, err := ReadInstallMetadata(profileRoot, name)
	if err != nil {
		return InstallResult{}, false, fmt.Errorf("plugin %q is already installed but unmanaged (use --force to replace)", name)
	}
	if meta.Source != source.Kind || meta.URL != source.URL || meta.Subdir != subdir {
		return InstallResult{}, false, fmt.Errorf("plugin %q is already installed from a different source (use --force to replace)", name)
	}
	if meta.NodePackage != spec.Package || meta.NodeVersion != spec.Version || meta.NodeLockfile != spec.Lockfile {
		return InstallResult{}, false, fmt.Errorf("plugin %q is already installed with different node dependencies (use --force to replace)", name)
	}
	if ref != "" && ref != meta.Revision {
		if source.Kind != "git" {
			return InstallResult{}, false, fmt.Errorf("plugin %q is already installed (use --force to replace)", name)
		}
		return InstallResult{}, false, fmt.Errorf("plugin %q is installed at another revision (use plugins update to move it)", name)
	}
	return InstallResult{Key: name, Revision: meta.Revision, Pinned: meta.Pinned, AlreadyInstalled: true}, true, nil
}

// MetadataPath returns the provenance file for one installed plugin key,
// inside the profile code tree.
func MetadataPath(profileRoot, key string) (string, error) {
	if !ValidPluginKey(key) {
		return "", fmt.Errorf("invalid plugin key")
	}
	if strings.TrimSpace(profileRoot) == "" {
		return "", fmt.Errorf("plugin home is unavailable")
	}
	return filepath.Join(PluginCodeDir(profileRoot, key), ".install-metadata.json"), nil
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
func ReadInstallMetadata(profileRoot, key string) (InstallMetadata, error) {
	path, err := MetadataPath(profileRoot, key)
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

// backupInstall moves the previous code tree aside. Data/ is never touched;
// nothing is merged.
func backupInstall(profileRoot, name string) (string, error) {
	backupRoot := filepath.Join(profileRoot, "plugins-backup")
	if err := os.MkdirAll(backupRoot, 0700); err != nil {
		return "", fmt.Errorf("prepare plugin backup: %w", err)
	}
	dest := filepath.Join(backupRoot, name+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(PluginCodeDir(profileRoot, name), dest); err != nil {
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

// gitLsRemote resolves the remote HEAD SHA without cloning. It needs the
// network for remote URLs but works offline for local paths.
func gitLsRemote(ctx context.Context, url string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, installRevTimeout)
	defer cancel()
	tmp, err := os.MkdirTemp("", "oswald-git-*")
	if err != nil {
		return "", fmt.Errorf("stage plugin fetch: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	cmd := exec.CommandContext(runCtx, "git", "ls-remote", url, "HEAD")
	cmd.Env = gitEnv(tmp)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("plugin fetch failed")
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 || !fullSHARE.MatchString(fields[0]) {
		return "", fmt.Errorf("plugin fetch failed")
	}
	return fields[0], nil
}

// UpdateFromURL refreshes one managed install by re-cloning its recorded
// source and publishing the result over the installed code tree. Published
// trees carry no git metadata (only provenance), so in-place fetch is
// impossible. Pinned installs need an explicit --ref; unpinned installs follow
// the remote HEAD. Data/ is never touched.
func UpdateFromURL(ctx context.Context, profileRoot, key, ref string, log *config.Logger) (InstallResult, error) {
	if !ValidPluginKey(key) {
		return InstallResult{}, fmt.Errorf("invalid plugin key")
	}
	if err := ValidateInstallRef(ref); err != nil {
		return InstallResult{}, err
	}
	ref = strings.TrimSpace(ref)
	meta, err := ReadInstallMetadata(profileRoot, key)
	if err != nil {
		return InstallResult{}, fmt.Errorf("plugin %q has no install provenance; remove and reinstall it", key)
	}
	if meta.Pinned && ref == "" {
		return InstallResult{}, fmt.Errorf("plugin %q is pinned; choose a new commit explicitly", key)
	}
	if meta.Source != "git" {
		return InstallResult{}, fmt.Errorf("plugin %q is not a git install", key)
	}
	staging, err := os.MkdirTemp("", "oswald-plugin-*")
	if err != nil {
		return InstallResult{}, fmt.Errorf("stage plugin update: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	cloneDir := filepath.Join(staging, "repo")
	if err := gitClone(ctx, meta.URL, cloneDir); err != nil {
		return InstallResult{}, err
	}
	target := ref
	if target == "" {
		target, err = gitLsRemote(ctx, meta.URL)
		if err != nil {
			return InstallResult{}, err
		}
	}
	revision, err := gitCheckoutPin(ctx, cloneDir, target)
	if err != nil {
		return InstallResult{}, err
	}
	pluginDir := cloneDir
	if meta.Subdir != "" {
		pluginDir = filepath.Join(cloneDir, filepath.FromSlash(meta.Subdir))
	}
	manifest, err := ParseManifestFile(pluginDir, "profile")
	if err != nil {
		return InstallResult{}, err
	}
	if manifest.Name != key {
		return InstallResult{}, fmt.Errorf("plugin update changed its name to %q", manifest.Name)
	}
	dest := PluginCodeDir(profileRoot, key)
	fresh, err := os.MkdirTemp(filepath.Dir(dest), ".plugin-update-*.tmp")
	if err != nil {
		return InstallResult{}, fmt.Errorf("stage plugin update: %w", err)
	}
	defer func() { _ = os.RemoveAll(fresh) }()
	if err := copyPluginTree(pluginDir, fresh); err != nil {
		return InstallResult{}, err
	}
	spec := resolveUpdateNodeSpec(manifest, meta)
	depsInstalled := false
	if spec.Present() {
		bin := EffectiveNodeBin(*manifest, InstallMetadata{NodePackage: spec.Package}, "")
		if err := installSidecarInto(ctx, profileRoot, key, bin, spec, fresh, log); err != nil {
			return InstallResult{}, err
		}
		depsInstalled = true
	}
	meta.Revision = revision
	meta.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	if strings.TrimSpace(manifest.NodePackage) != "" {
		meta.NodePackage = manifest.NodePackage
		meta.NodeVersion = manifest.NodeVersion
		meta.NodeLockfile = false
	}
	if ref != "" {
		meta.Ref = ref
		meta.Pinned = true
	} else {
		meta.Ref = ""
		meta.Pinned = false
	}
	if err := WriteInstallMetadata(fresh, meta); err != nil {
		return InstallResult{}, err
	}
	if err := os.RemoveAll(dest); err != nil {
		return InstallResult{}, fmt.Errorf("plugin update failed: %w", err)
	}
	if err := os.Rename(fresh, dest); err != nil {
		return InstallResult{}, fmt.Errorf("plugin update failed: %w", err)
	}
	if log != nil {
		log.Server("plugin.install").Debug("plugin.install.updated", "updated plugin", config.F("plugin", key))
	}
	return InstallResult{Key: key, Revision: revision, Pinned: meta.Pinned, DepsInstalled: depsInstalled}, nil
}

// RemoveInstall deletes one profile plugin. By default only the code tree is
// removed so data/home logins survive; full also removes data/ and the whole
// plugin directory.
func RemoveInstall(profileRoot, key string, full bool) error {
	if !ValidPluginKey(key) {
		return fmt.Errorf("invalid plugin key")
	}
	if strings.TrimSpace(profileRoot) == "" {
		return fmt.Errorf("plugin home is unavailable")
	}
	if full {
		dir := filepath.Join(ProfilePluginsDir(profileRoot), filepath.FromSlash(key))
		if _, err := os.Lstat(dir); err != nil {
			return fmt.Errorf("unknown plugin %q", key)
		}
		return os.RemoveAll(dir)
	}
	dir := PluginCodeDir(profileRoot, key)
	if _, err := os.Lstat(dir); err != nil {
		return fmt.Errorf("unknown plugin %q", key)
	}
	return os.RemoveAll(dir)
}
