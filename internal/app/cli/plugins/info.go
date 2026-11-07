package plugins

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// infoCmd shows one plugin's manifest and entry state. Settings values are
// never printed; only key counts and capability names identify configuration.
func infoCmd(root, flagProfile, raw string, stdout, stderr io.Writer) int {
	name := strings.ToLower(strings.TrimSpace(raw))
	if !config.ValidPluginName(name) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", raw)
		return exitUsage
	}
	profileCfg, profileName, profileRoot, err := resolveScope(root, flagProfile, false)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	winners := discoverForList(globalRootFor(profileRoot, profileName))
	key := name
	manifest, ok := winners[key]
	if !ok {
		for k, cand := range winners {
			if cand.Name == name {
				key, manifest, ok = k, cand, true
				break
			}
		}
	}
	if !ok {
		fmt.Fprintf(stderr, "oswald: unknown plugin %q\n", raw)
		return exitError
	}
	state := "not enabled"
	if profileCfg.PluginEnabledForProfile(key) {
		state = "enabled"
	} else {
		for _, denied := range profileCfg.PluginDisabled {
			if denied == key {
				state = "disabled"
				break
			}
		}
	}
	version := manifest.Version
	if version == "" {
		version = "-"
	}
	fmt.Fprintf(stdout, "name: %s\nkey: %s\nsource: %s\nversion: %s\nstate: %s\n", manifest.Name, key, manifest.Source, version, state)
	if manifest.Description != "" {
		fmt.Fprintf(stdout, "description: %s\n", manifest.Description)
	}
	if len(manifest.RequiresEnv) > 0 {
		fmt.Fprintf(stdout, "requires_env: %s\n", strings.Join(manifest.RequiresEnv, ", "))
	}
	if len(manifest.Capabilities) > 0 {
		fmt.Fprintf(stdout, "capabilities: %s\n", strings.Join(manifest.Capabilities, ", "))
	}
	entry, hasEntry := profileCfg.PluginEntries[key]
	if !hasEntry {
		entry, hasEntry = profileCfg.PluginEntries[manifest.Name]
	}
	if hasEntry {
		fmt.Fprintf(stdout, "allow_tool_override: %v\n", entry.AllowToolOverride)
		fmt.Fprintf(stdout, "mcp_allowlist: %d servers\n", len(entry.MCPAllowlist))
		fmt.Fprintf(stdout, "settings: %d keys\n", len(entry.Settings))
	} else {
		fmt.Fprintln(stdout, "allow_tool_override: false")
		fmt.Fprintln(stdout, "mcp_allowlist: 0 servers")
		fmt.Fprintln(stdout, "settings: 0 keys")
	}
	_ = filepath.Join
	return exitOK
}
