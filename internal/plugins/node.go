package plugins

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// Node dependency bounds. Sidecars install once globally under the shared
// plugin home and never touch the terminal PATH.
const (
	nodeInstallTimeout   = 5 * time.Minute
	maxNodeInstallOutput = 32 << 10
)

// SharedNodeDir returns the shared plugin home owning its node_modules tree:
// <globalRoot>/plugins-available/<name>. The plugin argument is a canonical
// key. Per-profile data stays under the profile plugins/ tree (see
// Manager.StorageRoot); only code and its sidecar live here.
func SharedNodeDir(globalRoot, plugin string) (string, error) {
	if !ValidPluginKey(plugin) {
		return "", fmt.Errorf("invalid plugin key")
	}
	if strings.TrimSpace(globalRoot) == "" {
		return "", fmt.Errorf("plugin home is unavailable")
	}
	return filepath.Join(UserPluginsDir(globalRoot), filepath.FromSlash(plugin)), nil
}

// NodeBinPath resolves the absolute plugin-local binary without executing it.
// The binary must already be installed under the shared node_modules/.bin;
// the terminal PATH is never mutated. Callers invoke the returned absolute
// path explicitly.
func NodeBinPath(globalRoot, plugin, bin string) (string, error) {
	if !ValidPluginKey(plugin) {
		return "", fmt.Errorf("invalid plugin key")
	}
	if strings.TrimSpace(bin) == "" || len(bin) > 64 || !npmBinRE.MatchString(bin) {
		return "", fmt.Errorf("invalid plugin binary")
	}
	dir, err := SharedNodeDir(globalRoot, plugin)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(dir, "node_modules", ".bin", bin)
	info, err := os.Lstat(candidate)
	if err != nil {
		return "", fmt.Errorf("plugin binary is not installed")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", fmt.Errorf("plugin binary is unavailable")
		}
		if rel, err := filepath.Rel(dir, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", fmt.Errorf("plugin binary escapes its home")
		}
		candidate = resolved
	} else if !info.Mode().IsRegular() {
		return "", fmt.Errorf("plugin binary is unavailable")
	}
	return candidate, nil
}

// InstallNodeDeps installs the manifest's pinned npm package into the shared
// plugin home. The explicit CLI invocation is the consent gate and requires
// default scope; versions are exact pins so two installs resolve identically.
// It never logs command output, only the plugin name and exit status.
func InstallNodeDeps(ctx context.Context, globalRoot string, manifest Manifest, log *config.Logger) error {
	if err := ValidateNodeDeps(&manifest); err != nil {
		return err
	}
	if strings.TrimSpace(manifest.NodePackage) == "" {
		return fmt.Errorf("plugin %q declares no node dependencies", manifest.Name)
	}
	dir, err := SharedNodeDir(globalRoot, manifest.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create plugin home: %w", err)
	}
	_ = os.Chmod(dir, 0700)
	cacheDir := filepath.Join(dir, "cache", "npm")
	tmpDir := filepath.Join(dir, "cache", "tmp")
	for _, path := range []string{cacheDir, tmpDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return fmt.Errorf("create plugin cache: %w", err)
		}
	}
	spec := manifest.NodePackage + "@" + manifest.NodeVersion
	runCtx, cancel := context.WithTimeout(ctx, nodeInstallTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "npm", "install", "--no-save", "--no-audit", "--no-fund", "--no-update-notifier", "--no-bin-links=false", spec)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"LANG=C.UTF-8",
		"TMPDIR=" + tmpDir,
		"NPM_CONFIG_CACHE=" + cacheDir,
		"NPM_CONFIG_AUDIT=false",
		"NPM_CONFIG_FUND=false",
		"NPM_CONFIG_UPDATE_NOTIFIER=false",
	}
	output, err := cmd.CombinedOutput()
	_ = output
	if err != nil {
		if log != nil {
			log.Server("plugin.node").Warn("plugin.node.install_failed", "plugin node install failed", config.F("plugin", manifest.Name))
		}
		if runCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("plugin node install timed out")
		}
		return fmt.Errorf("plugin node install failed")
	}
	if log != nil {
		log.Server("plugin.node").Debug("plugin.node.installed", "installed plugin node dependencies", config.F("plugin", manifest.Name))
	}
	// Verify the expected binary resolves inside the shared plugin home.
	if _, err := NodeBinPath(globalRoot, manifest.Name, DefaultNodeBin(manifest)); err != nil {
		return fmt.Errorf("plugin binary is not installed")
	}
	return nil
}
