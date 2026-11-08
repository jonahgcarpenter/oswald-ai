package plugins

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
)

// installCmd clones or copies a plugin into the shared plugins-available tree.
// Code writes require default scope; --profile selects the enable target and
// follows the same fencing as enable (contradictions fail closed).
func installCmd(ctx context.Context, root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	var source, ref, subdir string
	enable, noEnable, force := false, false, false
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		switch {
		case token == "--enable":
			enable = true
		case token == "--no-enable":
			noEnable = true
		case token == "--force":
			force = true
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
		case strings.HasPrefix(token, "-"):
			fmt.Fprintln(stderr, "usage: oswald plugins install <source> [--ref SHA] [--subdir PATH] [--enable|--no-enable] [--force] [--profile NAME]")
			return exitUsage
		default:
			if source != "" {
				fmt.Fprintln(stderr, "usage: oswald plugins install <source> [--ref SHA] [--subdir PATH] [--enable|--no-enable] [--force] [--profile NAME]")
				return exitUsage
			}
			source = token
		}
	}
	if source == "" || (enable && noEnable) {
		fmt.Fprintln(stderr, "usage: oswald plugins install <source> [--ref SHA] [--subdir PATH] [--enable|--no-enable] [--force] [--profile NAME]")
		return exitUsage
	}
	if err := config.RequireDefaultScope(); err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	target, err := config.ResolveProfile(flagProfile)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	globalRoot := mustAbs(root)
	result, err := plugins.InstallFromURL(ctx, globalRoot, source, ref, subdir, force, nil)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	state := "not enabled"
	if enable {
		oldCfg, _ := config.Validate(globalRoot)
		if err := setEnabledForProfile(globalRoot, target, result.Key, true); err != nil {
			reportPluginsError(stderr, err)
			return exitError
		}
		state = fmt.Sprintf("enabled for %s (%s)", target, effectForChange(globalRoot, oldCfg))
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
	fmt.Fprintf(stdout, "%s %s (%s, %s)\n", verb, result.Key, pin, state)
	return exitOK
}

// updateCmd refreshes one managed install in place. Pinned installs need an
// explicit --ref; unpinned installs follow the remote HEAD.
func updateCmd(ctx context.Context, root string, rest []string, stdout, stderr io.Writer) int {
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
			fmt.Fprintln(stderr, "usage: oswald plugins update <name> [--ref SHA]")
			return exitUsage
		default:
			if name != "" {
				fmt.Fprintln(stderr, "usage: oswald plugins update <name> [--ref SHA]")
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
	if err := config.RequireDefaultScope(); err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	result, err := plugins.UpdateFromURL(ctx, mustAbs(root), key, ref, nil)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	pin := "unpinned"
	if result.Pinned {
		pin = "pinned@" + shortSHA(result.Revision)
	}
	fmt.Fprintf(stdout, "updated %s (%s, restarting service to apply)\n", key, pin)
	return exitOK
}

// removeCmd drops one plugin for a target profile and deletes shared code.
// It refuses when other profiles still reference the key. --full also removes
// per-profile storage dirs.
func removeCmd(root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	var names []string
	full := false
	for _, token := range rest {
		switch token {
		case "--full":
			full = true
		default:
			if strings.HasPrefix(token, "-") {
				fmt.Fprintln(stderr, "usage: oswald plugins remove <name> [--full] [--profile NAME]")
				return exitUsage
			}
			names = append(names, token)
		}
	}
	if len(names) != 1 {
		fmt.Fprintln(stderr, "usage: oswald plugins remove <name> [--full] [--profile NAME]")
		return exitUsage
	}
	key := strings.ToLower(strings.TrimSpace(names[0]))
	if !plugins.ValidPluginKey(key) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", names[0])
		return exitUsage
	}
	if err := config.RequireDefaultScope(); err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	target, err := config.ResolveProfile(flagProfile)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	globalRoot := mustAbs(root)
	cfg, err := config.Validate(globalRoot)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	if others := referencingProfiles(cfg, target, key); len(others) > 0 {
		fmt.Fprintf(stderr, "oswald: plugin %q is still referenced by profiles: %s\n", key, strings.Join(others, ", "))
		return exitError
	}
	oldCfg := cfg
	if err := clearPluginForProfile(globalRoot, target, key); err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	if err := plugins.RemoveInstall(globalRoot, key); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	note := "directory kept"
	if full {
		for _, profileRoot := range allProfileRoots(cfg, globalRoot) {
			_ = os.RemoveAll(filepath.Join(profileRoot, "plugins", filepath.FromSlash(key)))
		}
		note = "and its directory"
	}
	fmt.Fprintf(stdout, "removed %s %s (%s)\n", key, note, effectForChange(globalRoot, oldCfg))
	return exitOK
}

// setEnabledForProfile adds key to enabled and drops it from disabled for one
// profile through the mediated write path.
func setEnabledForProfile(globalRoot, profile, key string, enable bool) error {
	cfg, err := config.Validate(globalRoot)
	if err != nil {
		return err
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
	enabled := append([]string(nil), profileCfg.PluginEnabled...)
	disabled := append([]string(nil), profileCfg.PluginDisabled...)
	if enable {
		seen := map[string]bool{}
		for _, existing := range enabled {
			seen[existing] = true
		}
		if !seen[key] {
			enabled = append(enabled, key)
		}
		disabled = filterOut(disabled, key)
	} else {
		enabled = filterOut(enabled, key)
		seen := map[string]bool{}
		for _, denied := range disabled {
			seen[denied] = true
		}
		if !seen[key] {
			disabled = append(disabled, key)
		}
	}
	sort.Strings(enabled)
	sort.Strings(disabled)
	if err := config.SetConfigValue(globalRoot, profile, []string{"plugins", "enabled"}, encodeStringList(enabled)); err != nil {
		return err
	}
	return config.SetConfigValue(globalRoot, profile, []string{"plugins", "disabled"}, encodeStringList(disabled))
}

func filterOut(values []string, drop string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != drop {
			out = append(out, value)
		}
	}
	return out
}

// clearPluginForProfile removes a key from enabled, disabled, and entries for
// one profile. Missing leaves are skipped so remove is idempotent.
func clearPluginForProfile(globalRoot, profile, key string) error {
	cfg, err := config.Validate(globalRoot)
	if err != nil {
		return err
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
	if err := config.SetConfigValue(globalRoot, profile, []string{"plugins", "enabled"}, encodeStringList(filterOut(profileCfg.PluginEnabled, key))); err != nil {
		return err
	}
	if err := config.SetConfigValue(globalRoot, profile, []string{"plugins", "disabled"}, encodeStringList(filterOut(profileCfg.PluginDisabled, key))); err != nil {
		return err
	}
	if _, ok := profileCfg.PluginEntries[key]; ok {
		if err := config.UnsetConfigValue(globalRoot, profile, []string{"plugins", "entries", key}); err != nil {
			return err
		}
	}
	return nil
}

// referencingProfiles lists profiles other than target whose enabled list or
// entries reference key.
func referencingProfiles(cfg *config.Config, target, key string) []string {
	var out []string
	check := func(name string, profileCfg *config.Config) {
		if name == target || profileCfg == nil {
			return
		}
		for _, enabled := range profileCfg.PluginEnabled {
			if enabled == key {
				out = append(out, name)
				return
			}
		}
		if _, ok := profileCfg.PluginEntries[key]; ok {
			out = append(out, name)
		}
	}
	check("default", cfg)
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		check(name, cfg.Profiles[name])
	}
	return out
}

// allProfileRoots returns default plus named profile roots for storage sweeps.
func allProfileRoots(cfg *config.Config, globalRoot string) []string {
	roots := []string{globalRoot}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if cfg.Profiles[name] != nil && cfg.Profiles[name].ProfileRoot != "" {
			roots = append(roots, cfg.Profiles[name].ProfileRoot)
		}
	}
	return roots
}

func shortSHA(revision string) string {
	if len(revision) >= 8 {
		return revision[:8]
	}
	return revision
}
