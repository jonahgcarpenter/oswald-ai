package plugins

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
)

// installCmd clones or copies a plugin into the calling profile's
// <profile>/plugins/<name>/code tree. Presence means enabled. The node
// sidecar installs as part of the invocation unless --no-deps opts out;
// --node-package with --node-version supplies it for repos whose manifest
// predates the node_package convention.
func installCmd(ctx context.Context, root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	const usage = "usage: oswald plugins install <source> [--ref SHA] [--subdir PATH] [--node-package PKG --node-version VER] [--no-deps] [--force] [--profile NAME]"
	var source, ref, subdir, nodePackage, nodeVersion string
	force, noDeps := false, false
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		switch {
		case token == "--force":
			force = true
		case token == "--no-deps":
			noDeps = true
		case token == "--ref" && i+1 < len(rest):
			i++
			ref = rest[i]
		case strings.HasPrefix(token, "--ref="):
			ref = strings.TrimPrefix(token, "--ref=")
		case token == "--subdir" && i+1 < len(rest):
			i++
			subdir = rest[i]
		case strings.HasPrefix(token, "--subdir="):
			subdir = strings.TrimPrefix(token, "--subdir=")
		case token == "--node-package" && i+1 < len(rest):
			i++
			nodePackage = rest[i]
		case strings.HasPrefix(token, "--node-package="):
			nodePackage = strings.TrimPrefix(token, "--node-package=")
		case token == "--node-version" && i+1 < len(rest):
			i++
			nodeVersion = rest[i]
		case strings.HasPrefix(token, "--node-version="):
			nodeVersion = strings.TrimPrefix(token, "--node-version=")
		case strings.HasPrefix(token, "-"):
			fmt.Fprintln(stderr, usage)
			return exitUsage
		default:
			if source != "" {
				fmt.Fprintln(stderr, usage)
				return exitUsage
			}
			source = token
		}
	}
	if source == "" || (nodePackage == "") != (nodeVersion == "") {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	_, target, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	oldCfg, _ := config.Validate(mustAbs(root))
	opts := plugins.InstallOptions{
		Ref: ref, Subdir: subdir, Force: force,
		NodePackage: strings.TrimSpace(nodePackage), NodeVersion: strings.TrimSpace(nodeVersion),
		NoDeps: noDeps,
	}
	result, err := plugins.InstallFromURL(ctx, profileRoot, source, opts, nil)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	pin := "unpinned"
	if result.Pinned {
		pin = "pinned@" + shortSHA(result.Revision)
	} else if result.Revision != "" {
		pin = "at " + shortSHA(result.Revision)
	}
	verb := "installed"
	if result.AlreadyInstalled {
		verb = "already installed"
	}
	deps := ""
	if result.DepsInstalled {
		deps = ", node deps installed"
		if bin := runnableBinName(profileRoot, result.Key); bin != "" {
			deps += ", '" + bin + "' runnable now"
		}
	}
	fmt.Fprintf(stdout, "%s %s (%s, enabled for %s%s, %s)\n", verb, result.Key, pin, target, deps, effectForChange(mustAbs(root), oldCfg))
	return exitOK
}

// runnableBinName returns the bare terminal name for an installed plugin's
// CLI (node bin or entry key), or "" when the plugin exposes no binary.
// Shims regenerate during install, so a non-empty name is runnable now.
func runnableBinName(profileRoot, key string) string {
	winners := discoverForList(profileRoot)
	manifest, ok := winners[key]
	if !ok {
		return ""
	}
	meta, _ := plugins.ReadInstallMetadata(profileRoot, key)
	if bin := plugins.EffectiveNodeBin(manifest, meta, ""); bin != "" {
		return bin
	}
	if strings.TrimSpace(manifest.Entry) != "" {
		return key
	}
	return ""
}

// updateCmd refreshes one profile install in place. Pinned installs need an
// explicit --ref; unpinned installs follow the remote HEAD.
func updateCmd(ctx context.Context, root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	const usage = "usage: oswald plugins update <name> [--ref SHA] [--profile NAME]"
	var name, ref string
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		switch {
		case token == "--ref" && i+1 < len(rest):
			i++
			ref = rest[i]
		case strings.HasPrefix(token, "--ref="):
			ref = strings.TrimPrefix(token, "--ref=")
		case strings.HasPrefix(token, "-"):
			fmt.Fprintln(stderr, usage)
			return exitUsage
		default:
			if name != "" {
				fmt.Fprintln(stderr, usage)
				return exitUsage
			}
			name = token
		}
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if !plugins.ValidPluginKey(key) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", name)
		return exitUsage
	}
	_, _, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	result, err := plugins.UpdateFromURL(ctx, profileRoot, key, ref, nil)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	pin := "unpinned"
	if result.Pinned {
		pin = "pinned@" + shortSHA(result.Revision)
	}
	fmt.Fprintf(stdout, "updated %s (%s, restart required - run `oswald restart` to apply)\n", key, pin)
	return exitOK
}

// removeCmd drops one profile plugin. By default only code/ is removed so
// data/home logins survive; --full also removes data/.
func removeCmd(root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	const usage = "usage: oswald plugins remove <name> [--full] [--profile NAME]"
	var names []string
	full := false
	for _, token := range rest {
		switch token {
		case "--full":
			full = true
		default:
			if strings.HasPrefix(token, "-") {
				fmt.Fprintln(stderr, usage)
				return exitUsage
			}
			names = append(names, token)
		}
	}
	if len(names) != 1 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	key := strings.ToLower(strings.TrimSpace(names[0]))
	if !plugins.ValidPluginKey(key) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", names[0])
		return exitUsage
	}
	_, _, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	oldCfg, _ := config.Validate(mustAbs(root))
	if err := clearPluginEntryForProfile(mustAbs(root), profileRoot, key); err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	if err := plugins.RemoveInstall(profileRoot, key, full); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	note := "code removed, data kept"
	if full {
		note = "code and data removed"
	}
	fmt.Fprintf(stdout, "removed %s (%s, %s)\n", key, note, effectForChange(mustAbs(root), oldCfg))
	return exitOK
}

// clearPluginEntryForProfile removes a plugin's entries row for one profile.
// Missing leaves are skipped so remove is idempotent. The profile is
// identified by root to avoid cross-profile mutation.
func clearPluginEntryForProfile(globalRoot, profileRoot, key string) error {
	cfg, err := config.Validate(globalRoot)
	if err != nil {
		return err
	}
	profile := "default"
	if cfg.ProfileRoot != profileRoot {
		found := ""
		for name, pc := range cfg.Profiles {
			if pc != nil && pc.ProfileRoot == profileRoot {
				found = name
				break
			}
		}
		if found == "" {
			return fmt.Errorf("unknown profile")
		}
		profile = found
	}
	var profileCfg *config.Config
	if profile == "default" {
		profileCfg = cfg
	} else {
		profileCfg = cfg.Profiles[profile]
		if profileCfg == nil {
			return fmt.Errorf("unknown profile %q", profile)
		}
	}
	if _, ok := profileCfg.PluginEntries[key]; ok {
		if err := config.UnsetConfigValue(globalRoot, profile, []string{"plugins", "entries", key}); err != nil {
			return err
		}
	}
	return nil
}

func shortSHA(revision string) string {
	if len(revision) >= 8 {
		return revision[:8]
	}
	return revision
}
