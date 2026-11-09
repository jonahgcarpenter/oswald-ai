package config

import (
	"regexp"
	"sort"
	"strings"
)

// PluginEntry holds validated per-plugin operator options. Settings carry
// interpolated string values only; secrets live in dotenv and are never stored
// here beyond their interpolated form for the active profile.
type PluginEntry struct {
	AllowToolOverride bool
	MCPAllowlist      []string
	Settings          map[string]string
}

var pluginNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,39}$`)

var pluginServerNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{1,39}$`)

// ValidPluginName reports whether a plugin id may appear in config.
func ValidPluginName(name string) bool {
	return pluginNameRE.MatchString(name) && name != "soul"
}

type pluginEntryYAML struct {
	Settings          map[string]string `yaml:"settings"`
	AllowToolOverride *bool             `yaml:"allow_tool_override"`
	MCPAllowlist      []string          `yaml:"mcp_allowlist"`
}

type pluginsYAML struct {
	Enabled  []string                   `yaml:"enabled"`
	Disabled []string                   `yaml:"disabled"`
	Entries  map[string]pluginEntryYAML `yaml:"entries"`
}

// applyPlugins validates the plugins section into the effective config.
// Presence means enabled: plugins.enabled/disabled were removed with the move
// to per-profile installs. Any use fails closed so the operator removes the
// keys explicitly; nothing is migrated automatically.
func applyPlugins(cfg *Config, doc documentYAML) error {
	raw := doc.Plugins
	if raw == nil {
		cfg.PluginEntries = nil
		return nil
	}
	if len(raw.Enabled) > 0 || len(raw.Disabled) > 0 {
		return configErr("config_plugin_invalid", "plugins")
	}
	if raw.Entries != nil && len(raw.Entries) > 32 {
		return configErr("config_plugin_invalid", "plugins.entries")
	}
	entries := map[string]PluginEntry{}
	for name, entry := range raw.Entries {
		base := "plugins.entries." + name
		if !ValidPluginName(name) {
			return configErr("config_plugin_invalid", base)
		}
		if len(entry.Settings) > 32 {
			return configErr("config_plugin_invalid", base+".settings")
		}
		settings := map[string]string{}
		for key, value := range entry.Settings {
			if !pluginServerNameRE.MatchString(key) {
				return configErr("config_plugin_invalid", base+".settings")
			}
			if len(value) > 8192 || strings.ContainsAny(value, "\r\n\x00") {
				return configErr("config_plugin_invalid", base+".settings")
			}
			settings[key] = value
		}
		if len(entry.MCPAllowlist) > 32 {
			return configErr("config_plugin_invalid", base+".mcp_allowlist")
		}
		allowlist := make([]string, 0, len(entry.MCPAllowlist))
		seen := map[string]bool{}
		for _, server := range entry.MCPAllowlist {
			if !pluginServerNameRE.MatchString(server) || server == "soul" {
				return configErr("config_plugin_invalid", base+".mcp_allowlist")
			}
			if !seen[server] {
				seen[server] = true
				allowlist = append(allowlist, server)
			}
		}
		sort.Strings(allowlist)
		allow := false
		if entry.AllowToolOverride != nil {
			allow = *entry.AllowToolOverride
		}
		entries[name] = PluginEntry{AllowToolOverride: allow, MCPAllowlist: allowlist, Settings: settings}
	}
	if len(entries) == 0 {
		entries = nil
	}
	cfg.PluginEntries = entries
	return nil
}
