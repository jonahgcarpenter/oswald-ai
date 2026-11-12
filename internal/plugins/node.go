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

// Node dependency bounds. Sidecars install per profile under
// <profile>/plugins/<name>/code/node_modules and never touch the terminal PATH.
const (
	nodeInstallTimeout   = 5 * time.Minute
	maxNodeInstallOutput = 32 << 10
)

// NodeBinPath resolves the absolute plugin-local binary without executing it.
// The binary must already be installed under the profile code
// node_modules/.bin; the terminal PATH is never mutated. Callers invoke the
// returned absolute path explicitly.
func NodeBinPath(profileRoot, plugin, bin string) (string, error) {
	if !ValidPluginKey(plugin) {
		return "", fmt.Errorf("invalid plugin key")
	}
	if strings.TrimSpace(profileRoot) == "" {
		return "", fmt.Errorf("plugin home is unavailable")
	}
	return nodeBinPathIn(PluginCodeDir(profileRoot, plugin), bin)
}

// nodeBinPathIn resolves a binary inside an explicit plugin code directory:
// the profile code home for live installs, a staged temp dir for updates.
func nodeBinPathIn(dir, bin string) (string, error) {
	if strings.TrimSpace(bin) == "" || len(bin) > 64 || !npmBinRE.MatchString(bin) {
		return "", fmt.Errorf("invalid plugin binary")
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

// InstallNodeDeps installs the manifest's pinned npm package into the profile
// code home. The explicit CLI invocation is the consent gate; versions are
// exact pins so two installs resolve identically. It never logs command
// output, only the plugin name and exit status.
func InstallNodeDeps(ctx context.Context, profileRoot string, manifest Manifest, log *config.Logger) error {
	if err := ValidateNodeDeps(&manifest); err != nil {
		return err
	}
	if strings.TrimSpace(manifest.NodePackage) == "" {
		return fmt.Errorf("plugin %q declares no node dependencies", manifest.Name)
	}
	return InstallNodePackage(ctx, profileRoot, manifest.Name, DefaultNodeBin(manifest),
		manifest.NodePackage+"@"+manifest.NodeVersion, "", log)
}

// InstallNodePackage installs one exact npm pin into the plugin code directory:
// the profile code home when dir is "", a staged temp dir for updates. bin names
// the expected binary for post-install verification; empty skips it.
func InstallNodePackage(ctx context.Context, profileRoot, key, bin, spec, dir string, log *config.Logger) error {
	if !ValidPluginKey(key) {
		return fmt.Errorf("invalid plugin key")
	}
	if strings.TrimSpace(spec) == "" {
		return fmt.Errorf("plugin node spec is required")
	}
	isLive := dir == ""
	if isLive {
		if strings.TrimSpace(profileRoot) == "" {
			return fmt.Errorf("plugin home is unavailable")
		}
		dir = PluginCodeDir(profileRoot, key)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create plugin home: %w", err)
	}
	_ = os.Chmod(dir, 0700)
	var cacheDir, tmpDir string
	var err error
	if isLive {
		cacheDir, tmpDir, err = prepareProfileNodeCache(profileRoot, key)
	} else {
		cacheDir, tmpDir, err = prepareNodeCache(dir)
	}
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, nodeInstallTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "npm", "install", "--no-save", "--no-audit", "--no-fund", "--no-update-notifier", "--no-bin-links=false", spec)
	cmd.Dir = dir
	cmd.Env = npmEnv(cacheDir, tmpDir)
	output, err := cmd.CombinedOutput()
	_ = output
	if err != nil {
		if log != nil {
			log.Server("plugin.node").Warn("plugin.node.install_failed", "plugin node install failed", config.F("plugin", key))
		}
		if runCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("plugin node install timed out")
		}
		return fmt.Errorf("plugin node install failed")
	}
	if log != nil {
		log.Server("plugin.node").Debug("plugin.node.installed", "installed plugin node dependencies", config.F("plugin", key))
	}
	// Verify the expected binary resolves inside the install directory.
	if bin != "" {
		if _, err := nodeBinPathIn(dir, bin); err != nil {
			return fmt.Errorf("plugin binary is not installed")
		}
	}
	if isLive {
		if err := RegenerateShims(profileRoot); err != nil && log != nil {
			log.Server("plugin.node").Warn("plugin.shim.regen_failed", "plugin shim regeneration failed", config.F("plugin", key))
		}
	}
	return nil
}

// InstallNodeLockfile installs a plugin's own pinned dependencies via npm ci
// into the plugin code directory: the profile code home when dir is "", a
// staged temp dir for updates. The directory must carry package.json with a
// lockfile.
func InstallNodeLockfile(ctx context.Context, profileRoot, key, dir string, log *config.Logger) error {
	if !ValidPluginKey(key) {
		return fmt.Errorf("invalid plugin key")
	}
	isLive := dir == ""
	if isLive {
		if strings.TrimSpace(profileRoot) == "" {
			return fmt.Errorf("plugin home is unavailable")
		}
		dir = PluginCodeDir(profileRoot, key)
	}
	if !hasLockfileSidecar(dir) {
		return fmt.Errorf("plugin %q carries no lockfile", key)
	}
	var cacheDir, tmpDir string
	var err error
	if isLive {
		cacheDir, tmpDir, err = prepareProfileNodeCache(profileRoot, key)
	} else {
		cacheDir, tmpDir, err = prepareNodeCache(dir)
	}
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, nodeInstallTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "npm", "ci", "--no-audit", "--no-fund", "--no-update-notifier")
	cmd.Dir = dir
	cmd.Env = npmEnv(cacheDir, tmpDir)
	output, err := cmd.CombinedOutput()
	_ = output
	if err != nil {
		if log != nil {
			log.Server("plugin.node").Warn("plugin.node.install_failed", "plugin node install failed", config.F("plugin", key))
		}
		if runCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("plugin node install timed out")
		}
		return fmt.Errorf("plugin node install failed")
	}
	if log != nil {
		log.Server("plugin.node").Debug("plugin.node.installed", "installed plugin node dependencies", config.F("plugin", key))
	}
	if key != "" && strings.TrimSpace(profileRoot) != "" && isLive {
		if err := RegenerateShims(profileRoot); err != nil && log != nil {
			log.Server("plugin.node").Warn("plugin.shim.regen_failed", "plugin shim regeneration failed", config.F("plugin", key))
		}
	}
	return nil
}

// prepareProfileNodeCache creates the npm cache and temp dirs under the plugin
// data tree so build artifacts never pollute the immutable code copy.
func prepareProfileNodeCache(profileRoot, key string) (cacheDir, tmpDir string, err error) {
	base := filepath.Join(PluginDataDir(profileRoot, key), "cache", "node")
	cacheDir = filepath.Join(base, "npm")
	tmpDir = filepath.Join(base, "tmp")
	for _, path := range []string{cacheDir, tmpDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return "", "", fmt.Errorf("create plugin cache: %w", err)
		}
	}
	return cacheDir, tmpDir, nil
}

// prepareNodeCache creates the npm cache and temp dirs under a staged dir for
// updates, where no profile data tree exists yet.
func prepareNodeCache(dir string) (cacheDir, tmpDir string, err error) {
	cacheDir = filepath.Join(dir, "cache", "npm")
	tmpDir = filepath.Join(dir, "cache", "tmp")
	for _, path := range []string{cacheDir, tmpDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return "", "", fmt.Errorf("create plugin cache: %w", err)
		}
	}
	return cacheDir, tmpDir, nil
}

// npmEnv is the minimal non-interactive npm environment. No credentials pass
// through here; registry failures surface as plain install errors.
func npmEnv(cacheDir, tmpDir string) []string {
	return []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"LANG=C.UTF-8",
		"TMPDIR=" + tmpDir,
		"NPM_CONFIG_CACHE=" + cacheDir,
		"NPM_CONFIG_AUDIT=false",
		"NPM_CONFIG_FUND=false",
		"NPM_CONFIG_UPDATE_NOTIFIER=false",
	}
}
