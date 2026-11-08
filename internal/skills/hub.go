package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Hub install bounds. Skill bundles install per profile; shared code stays
// with plugins. Sizes mirror the scan caps so installed skills always load.
const (
	hubCloneTimeout  = 5 * time.Minute
	hubRevTimeout    = time.Minute
	maxHubFileBytes  = 1 << 20
	maxHubFiles      = 128
	maxHubLockBytes  = 64 * 1024
	hubLockFileName  = "skills.lock.json"
	hubBackupDirName = "skills-backup"
	hubFullSHARE     = `^[0-9a-f]{40}$`
)

var (
	hubSHARE     = regexp.MustCompile(hubFullSHARE)
	hubOwnerRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	hubTimestamp = func() string { return time.Now().UTC().Format("20060102T150405Z") }
)

// LockEntry is the provenance record for one installed skill. Secrets never
// appear here: only source identity and revisions.
type LockEntry struct {
	Source      string `json:"source"`
	URL         string `json:"url"`
	Ref         string `json:"ref,omitempty"`
	Revision    string `json:"revision,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	Subdir      string `json:"subdir,omitempty"`
	Pinned      bool   `json:"pinned"`
	InstalledAt string `json:"installed_at"`
}

// InstallSource is a parsed `skills install` argument.
type InstallSource struct {
	// Kind is git or local.
	Kind string
	// URL is the normalized git URL or absolute local path.
	URL string
}

// ParseInstallSource normalizes a skill install argument: owner/repo
// shorthand, https git URLs, file:// URLs, and absolute local paths.
func ParseInstallSource(raw string) (InstallSource, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > 1024 || strings.ContainsAny(trimmed, "\r\n\x00") || strings.ContainsAny(trimmed, " \t") {
		return InstallSource{}, fmt.Errorf("invalid skill source")
	}
	if hubOwnerRE.MatchString(trimmed) && !strings.Contains(trimmed, ":") {
		owner, repo, _ := strings.Cut(trimmed, "/")
		if owner == "." || owner == ".." || repo == "." || repo == ".." {
			return InstallSource{}, fmt.Errorf("invalid skill source")
		}
		return InstallSource{Kind: "git", URL: "https://github.com/" + trimmed + ".git"}, nil
	}
	lowered := strings.ToLower(trimmed)
	if strings.HasPrefix(lowered, "https://") {
		return InstallSource{Kind: "git", URL: trimmed}, nil
	}
	if strings.HasPrefix(lowered, "http://") {
		return InstallSource{}, fmt.Errorf("skill git URLs must use https")
	}
	path := trimmed
	if strings.HasPrefix(lowered, "file://") {
		path = strings.TrimPrefix(trimmed, "file://")
	}
	if !filepath.IsAbs(path) {
		return InstallSource{}, fmt.Errorf("skill paths must be absolute or file:// URLs")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return InstallSource{}, fmt.Errorf("skill source directory is unavailable")
	}
	return InstallSource{Kind: "local", URL: filepath.Clean(path)}, nil
}

// ValidateInstallRef accepts only a full 40-hex commit SHA (or empty for
// unpinned installs). Tags and branches are never accepted.
func ValidateInstallRef(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	if !hubSHARE.MatchString(strings.TrimSpace(ref)) {
		return fmt.Errorf("skill --ref must be a full 40-character commit SHA")
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
		return "", fmt.Errorf("invalid skill subdirectory")
	}
	return filepath.Clean(trimmed), nil
}

// LockPath returns the per-profile skills lockfile path.
func LockPath(profileRoot string) (string, error) {
	if strings.TrimSpace(profileRoot) == "" {
		return "", fmt.Errorf("profile root is unavailable")
	}
	return filepath.Join(profileRoot, hubLockFileName), nil
}

// ReadLock loads the per-profile skills lockfile. A missing file reads as
// empty; anything else that fails to parse fails closed.
func ReadLock(profileRoot string) (map[string]LockEntry, error) {
	path, err := LockPath(profileRoot)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]LockEntry{}, nil
		}
		return nil, fmt.Errorf("skill lockfile is unavailable")
	}
	if len(data) > maxHubLockBytes {
		return nil, fmt.Errorf("skill lockfile exceeds size limit")
	}
	var entries map[string]LockEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("skill lockfile is not valid")
	}
	if entries == nil {
		entries = map[string]LockEntry{}
	}
	return entries, nil
}

// WriteLock atomically replaces the per-profile skills lockfile, owner-only.
func WriteLock(profileRoot string, entries map[string]LockEntry) error {
	path, err := LockPath(profileRoot)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode skill lockfile: %w", err)
	}
	if len(data) > maxHubLockBytes {
		return fmt.Errorf("skill lockfile exceeds size limit")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".skills-lock-*.tmp")
	if err != nil {
		return fmt.Errorf("skill lockfile is unavailable")
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("skill lockfile is unavailable")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("skill lockfile is unavailable")
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("skill lockfile is unavailable")
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return fmt.Errorf("skill lockfile is unavailable")
	}
	return os.Rename(tmpName, path)
}

// ValidateBundle checks that dir holds one installable SKILL.md and returns
// its canonical skill name (declared frontmatter name, else directory base).
// Validation warns nothing and logs nothing: it fails closed with a reason.
func ValidateBundle(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil || len(data) == 0 || len(data) > maxSkillFileBytes {
		return "", fmt.Errorf("skill bundle has no readable SKILL.md")
	}
	front, _, ok := parseFrontmatter(string(data))
	if !ok {
		return "", fmt.Errorf("skill SKILL.md is not valid")
	}
	name := strings.ToLower(strings.TrimSpace(front.name))
	if name == "" {
		name = strings.ToLower(filepath.Base(dir))
	}
	if name, err = validateNewName(name); err != nil {
		return "", fmt.Errorf("skill bundle names an invalid skill: %w", err)
	}
	return name, nil
}

// InstallResult reports a completed skill install.
type InstallResult struct {
	// Name is the canonical installed skill name.
	Name string
	// Revision is the resolved git HEAD SHA, empty for local installs.
	Revision string
	// Pinned reports whether --ref pinned the install.
	Pinned bool
	// AlreadyInstalled reports an idempotent no-op.
	AlreadyInstalled bool
}

// InstallSkill validates a skill bundle from a git or local source and
// publishes it into the profile skills tree, recording provenance in the
// per-profile lockfile. Force replaces an existing install, backing it up
// first. Local installs record a content hash used by update to detect
// operator edits.
func InstallSkill(ctx context.Context, profileRoot, raw, ref, subdir string, force bool) (InstallResult, error) {
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
	if source.Kind == "local" {
		if _, err := os.Stat(filepath.Join(source.URL, ".git")); err == nil {
			// Local git checkout: clone it so the install records a
			// revision and supports update. File clones need no network.
			source = InstallSource{Kind: "git", URL: source.URL}
		} else if ref != "" {
			return InstallResult{}, fmt.Errorf("skill --ref applies to git sources only")
		}
	}
	staging, err := os.MkdirTemp("", "oswald-skill-*")
	if err != nil {
		return InstallResult{}, fmt.Errorf("stage skill install: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	checkout := staging
	var revision string
	switch source.Kind {
	case "git":
		cloneDir := filepath.Join(staging, "repo")
		if err := hubGitClone(ctx, source.URL, cloneDir); err != nil {
			return InstallResult{}, err
		}
		revision, err = hubGitCheckoutPin(ctx, cloneDir, ref)
		if err != nil {
			return InstallResult{}, err
		}
		checkout = cloneDir
	case "local":
		checkout = source.URL
	default:
		return InstallResult{}, fmt.Errorf("invalid skill source")
	}
	bundleDir := checkout
	if cleanSubdir != "" {
		bundleDir = filepath.Join(checkout, filepath.FromSlash(cleanSubdir))
	}
	name, err := ValidateBundle(bundleDir)
	if err != nil {
		return InstallResult{}, err
	}
	skillsRoot := filepath.Join(profileRoot, "skills")
	dest := filepath.Join(skillsRoot, name)
	if _, err := os.Lstat(dest); err == nil {
		if !force {
			if result, done, err := existingSkillState(profileRoot, name, source, cleanSubdir, ref, bundleDir); err != nil {
				return InstallResult{}, err
			} else if done {
				return result, nil
			}
		}
		backup, err := backupSkill(profileRoot, name)
		if err != nil {
			return InstallResult{}, err
		}
		_ = backup
		if err := os.RemoveAll(dest); err != nil {
			return InstallResult{}, fmt.Errorf("replace skill install: %w", err)
		}
	}
	if err := copySkillTree(bundleDir, dest); err != nil {
		return InstallResult{}, err
	}
	contentHash, err := skillContentHash(dest)
	if err != nil {
		_ = os.RemoveAll(dest)
		return InstallResult{}, err
	}
	entries, err := ReadLock(profileRoot)
	if err != nil {
		_ = os.RemoveAll(dest)
		return InstallResult{}, err
	}
	entries[name] = LockEntry{
		Source: source.Kind, URL: source.URL, Ref: ref, Revision: revision,
		ContentHash: contentHash, Subdir: cleanSubdir, Pinned: ref != "",
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteLock(profileRoot, entries); err != nil {
		_ = os.RemoveAll(dest)
		return InstallResult{}, err
	}
	return InstallResult{Name: name, Revision: revision, Pinned: ref != ""}, nil
}

// existingSkillState mirrors the plugin install gate: same source is a no-op
// (pins never move without update), anything else guides to --force/update.
func existingSkillState(profileRoot, name string, source InstallSource, subdir, ref, bundleDir string) (InstallResult, bool, error) {
	entries, err := ReadLock(profileRoot)
	if err != nil {
		return InstallResult{}, false, err
	}
	meta, ok := entries[name]
	if !ok {
		return InstallResult{}, false, fmt.Errorf("skill %q is already installed but unmanaged (use --force to replace)", name)
	}
	if meta.Source != source.Kind || meta.URL != source.URL || meta.Subdir != subdir {
		return InstallResult{}, false, fmt.Errorf("skill %q is already installed from a different source (use --force to replace)", name)
	}
	if ref != "" && ref != meta.Revision {
		return InstallResult{}, false, fmt.Errorf("skill %q is installed at another revision (use skills update to move it)", name)
	}
	_ = bundleDir
	return InstallResult{Name: name, Revision: meta.Revision, Pinned: meta.Pinned, AlreadyInstalled: true}, true, nil
}

// UpdateSkill refreshes one managed install by re-cloning its recorded
// source and publishing the result over the installed tree. Installed trees
// carry no git metadata (only the lockfile), so in-place fetch is impossible.
// Pinned installs need an explicit --ref; unpinned installs follow the remote
// HEAD. Operator-modified skills (SKILL.md hash drift) refuse without --force,
// which backs the modified tree up first.
func UpdateSkill(ctx context.Context, profileRoot, name, ref string, force bool) (InstallResult, error) {
	key, err := validateNewName(name)
	if err != nil {
		return InstallResult{}, fmt.Errorf("invalid skill name %q", name)
	}
	name = key
	if err := ValidateInstallRef(ref); err != nil {
		return InstallResult{}, err
	}
	ref = strings.TrimSpace(ref)
	entries, err := ReadLock(profileRoot)
	if err != nil {
		return InstallResult{}, err
	}
	meta, ok := entries[name]
	if !ok {
		return InstallResult{}, fmt.Errorf("skill %q is not a managed install", name)
	}
	if meta.Pinned && ref == "" {
		return InstallResult{}, fmt.Errorf("skill %q is pinned; choose a new commit explicitly", name)
	}
	if meta.Source != "git" {
		return InstallResult{}, fmt.Errorf("skill %q is not a git install; reinstall it to refresh", name)
	}
	dest := filepath.Join(profileRoot, "skills", name)
	current, err := skillContentHash(dest)
	if err != nil {
		return InstallResult{}, err
	}
	modified := current != meta.ContentHash
	if modified && !force {
		return InstallResult{}, fmt.Errorf("skill %q was modified; use --force to overwrite", name)
	}
	staging, err := os.MkdirTemp("", "oswald-skill-*")
	if err != nil {
		return InstallResult{}, fmt.Errorf("stage skill update: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	cloneDir := filepath.Join(staging, "repo")
	if err := hubGitClone(ctx, meta.URL, cloneDir); err != nil {
		return InstallResult{}, err
	}
	target := ref
	if target == "" {
		target, err = hubGitLsRemote(ctx, meta.URL)
		if err != nil {
			return InstallResult{}, err
		}
	}
	revision, err := hubGitCheckoutPin(ctx, cloneDir, target)
	if err != nil {
		return InstallResult{}, err
	}
	bundleDir := cloneDir
	if meta.Subdir != "" {
		bundleDir = filepath.Join(cloneDir, filepath.FromSlash(meta.Subdir))
	}
	if _, err := ValidateBundle(bundleDir); err != nil {
		return InstallResult{}, err
	}
	if modified {
		if _, err := backupSkill(profileRoot, name); err != nil {
			return InstallResult{}, err
		}
	}
	fresh, err := os.MkdirTemp(filepath.Join(profileRoot, "skills"), ".skill-update-*.tmp")
	if err != nil {
		return InstallResult{}, fmt.Errorf("stage skill update: %w", err)
	}
	defer func() { _ = os.RemoveAll(fresh) }()
	if err := copySkillTree(bundleDir, fresh); err != nil {
		return InstallResult{}, err
	}
	contentHash, err := skillContentHash(fresh)
	if err != nil {
		return InstallResult{}, err
	}
	if err := os.RemoveAll(dest); err != nil {
		return InstallResult{}, fmt.Errorf("skill update failed: %w", err)
	}
	if err := os.Rename(fresh, dest); err != nil {
		return InstallResult{}, fmt.Errorf("skill update failed: %w", err)
	}
	meta.Revision = revision
	meta.ContentHash = contentHash
	meta.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	if ref != "" {
		meta.Ref = ref
		meta.Pinned = true
	} else {
		meta.Ref = ""
		meta.Pinned = false
	}
	entries[name] = meta
	if err := WriteLock(profileRoot, entries); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Name: name, Revision: revision, Pinned: meta.Pinned}, nil
}

// RemoveSkill deletes one installed skill and its lockfile record. Skills are
// per-profile data, so no cross-profile guard is needed; --full also removes
// that skill's backups.
func RemoveSkill(profileRoot, name string, full bool) error {
	key, err := validateNewName(name)
	if err != nil {
		return fmt.Errorf("invalid skill name %q", name)
	}
	name = key
	dest := filepath.Join(profileRoot, "skills", name)
	if _, err := os.Lstat(dest); err != nil {
		return fmt.Errorf("unknown skill %q", name)
	}
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("remove skill: %w", err)
	}
	entries, err := ReadLock(profileRoot)
	if err == nil {
		if _, ok := entries[name]; ok {
			delete(entries, name)
			_ = WriteLock(profileRoot, entries)
		}
	}
	if full {
		matches, _ := filepath.Glob(filepath.Join(profileRoot, hubBackupDirName, name+"-*"))
		for _, match := range matches {
			_ = os.RemoveAll(match)
		}
	}
	return nil
}

// skillContentHash digests the installed SKILL.md for operator-edit detection.
func skillContentHash(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil || len(data) == 0 {
		return "", fmt.Errorf("installed skill is unreadable")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// backupSkill moves the previous tree aside before a forced replace.
func backupSkill(profileRoot, name string) (string, error) {
	backupRoot := filepath.Join(profileRoot, hubBackupDirName)
	if err := os.MkdirAll(backupRoot, 0700); err != nil {
		return "", fmt.Errorf("prepare skill backup: %w", err)
	}
	dest := filepath.Join(backupRoot, name+"-"+hubTimestamp())
	if err := os.Rename(filepath.Join(profileRoot, "skills", name), dest); err != nil {
		return "", fmt.Errorf("back up skill install: %w", err)
	}
	return dest, nil
}

// copySkillTree copies a validated skill bundle, skipping .git and the
// install metadata. Symlinks fail closed; files are bounded to the scan cap.
func copySkillTree(src, dest string) error {
	var count int
	err := filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			if err == nil {
				return os.MkdirAll(dest, 0700)
			}
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill bundle contains a symlink")
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("skill bundle file is not a regular file")
		}
		count++
		if count > maxHubFiles {
			return fmt.Errorf("skill bundle exceeds file limit")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(data) > maxHubFileBytes {
			return fmt.Errorf("skill bundle file exceeds size limit")
		}
		return os.WriteFile(target, data, 0600)
	})
	if err != nil {
		_ = os.RemoveAll(dest)
		return err
	}
	return nil
}

// hubGitEnv is the minimal non-interactive git environment for hub fetches.
// No credentials pass through: ambient helpers and agents own auth, and
// prompting is disabled so installs fail closed instead of hanging.
func hubGitEnv(tmp string) []string {
	return []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"LANG=C.UTF-8",
		"TMPDIR=" + tmp,
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	}
}

func hubGitRun(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tmp, err := os.MkdirTemp("", "oswald-skill-*")
	if err != nil {
		return "", fmt.Errorf("stage skill fetch: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	cmd := exec.CommandContext(runCtx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = hubGitEnv(tmp)
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = output
		if runCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("skill fetch timed out")
		}
		return "", fmt.Errorf("skill fetch failed")
	}
	return strings.TrimSpace(string(output)), nil
}

func hubGitClone(ctx context.Context, url, dest string) error {
	if _, err := hubGitRun(ctx, "", hubCloneTimeout, "clone", "--depth", "1", url, dest); err != nil {
		return err
	}
	return nil
}

func hubGitCheckoutPin(ctx context.Context, dir, ref string) (string, error) {
	if ref != "" {
		if _, err := hubGitRun(ctx, dir, hubRevTimeout, "fetch", "origin", ref); err != nil {
			return "", fmt.Errorf("skill ref is unavailable")
		}
		if _, err := hubGitRun(ctx, dir, hubRevTimeout, "checkout", "--detach", ref); err != nil {
			return "", fmt.Errorf("skill ref is unavailable")
		}
	}
	head, err := hubGitRun(ctx, dir, hubRevTimeout, "rev-parse", "HEAD")
	if err != nil || !hubSHARE.MatchString(head) {
		return "", fmt.Errorf("skill fetch failed")
	}
	if ref != "" && head != ref {
		return "", fmt.Errorf("skill ref mismatch")
	}
	return head, nil
}

// hubGitLsRemote resolves the remote HEAD SHA without cloning. It needs the
// network for remote URLs but works offline for local paths.
func hubGitLsRemote(ctx context.Context, url string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, hubRevTimeout)
	defer cancel()
	tmp, err := os.MkdirTemp("", "oswald-skill-*")
	if err != nil {
		return "", fmt.Errorf("stage skill fetch: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	cmd := exec.CommandContext(runCtx, "git", "ls-remote", url, "HEAD")
	cmd.Env = hubGitEnv(tmp)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("skill fetch failed")
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 || !hubSHARE.MatchString(fields[0]) {
		return "", fmt.Errorf("skill fetch failed")
	}
	return fields[0], nil
}

// SortedLockNames returns installed skill names in stable order for listing.
func SortedLockNames(entries map[string]LockEntry) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
