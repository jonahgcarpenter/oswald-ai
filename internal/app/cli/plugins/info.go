package plugins

import (
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
)

// infoCmd shows one plugin's manifest and entry state. Settings values are
// never printed; only key counts and capability names identify configuration.
func infoCmd(root, flagProfile, raw string, stdout, stderr io.Writer) int {
	name := strings.ToLower(strings.TrimSpace(raw))
	if !plugins.ValidPluginKey(name) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", raw)
		return exitUsage
	}
	profileCfg, _, profileRoot, err := resolveScope(root, flagProfile, false)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	winners := discoverForList(profileRoot)
	key := name
	manifest, ok := winners[key]
	if !ok {
		fmt.Fprintf(stderr, "oswald: unknown plugin %q\n", raw)
		return exitError
	}
	version := manifest.Version
	if version == "" {
		version = "-"
	}
	fmt.Fprintf(stdout, "name: %s\nkey: %s\nsource: %s\nversion: %s\nstate: enabled\n", manifest.Name, key, manifest.Source, version)
	if manifest.Description != "" {
		fmt.Fprintf(stdout, "description: %s\n", manifest.Description)
	}
	if len(manifest.RequiresEnv) > 0 {
		fmt.Fprintf(stdout, "requires_env: %s\n", strings.Join(manifest.RequiresEnv, ", "))
	}
	if len(manifest.Capabilities) > 0 {
		fmt.Fprintf(stdout, "capabilities: %s\n", strings.Join(manifest.Capabilities, ", "))
	}
	if len(manifest.RequiresPlugins) > 0 {
		fmt.Fprintf(stdout, "requires: %s\n", strings.Join(manifest.RequiresPlugins, ", "))
	}
	if meta, err := plugins.ReadInstallMetadata(profileRoot, key); err == nil {
		provenance := meta.Source
		if meta.Pinned && meta.Revision != "" {
			provenance += " pinned@" + shortSHA(meta.Revision)
		} else if meta.Revision != "" {
			provenance += " at " + shortSHA(meta.Revision)
		}
		fmt.Fprintf(stdout, "installed_from: %s\n", provenance)
	}
	entry, hasEntry := profileCfg.PluginEntries[key]
	if hasEntry {
		fmt.Fprintf(stdout, "allow_tool_override: %v\n", entry.AllowToolOverride)
		fmt.Fprintf(stdout, "mcp_allowlist: %d servers\n", len(entry.MCPAllowlist))
		fmt.Fprintf(stdout, "settings: %d keys\n", len(entry.Settings))
	} else {
		fmt.Fprintln(stdout, "allow_tool_override: false")
		fmt.Fprintln(stdout, "mcp_allowlist: 0 servers")
		fmt.Fprintln(stdout, "settings: 0 keys")
	}
	return exitOK
}
