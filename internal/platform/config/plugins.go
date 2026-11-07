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
// Disabled always wins at runtime; listing a name in both fails closed so the
// operator resolves the contradiction explicitly.
func applyPlugins(cfg *Config, doc documentYAML) error {
	raw := doc.Plugins
	if raw == nil {
		cfg.PluginEnabled = nil
		cfg.PluginDisabled = nil
		cfg.PluginEntries = nil
		return nil
	}
	if len(raw.Enabled) > 32 || len(raw.Disabled) > 32 {
		return configErr("config_plugin_invalid", "plugins")
	}
	if raw.Entries != nil && len(raw.Entries) > 32 {
		return configErr("config_plugin_invalid", "plugins.entries")
	}
	enabled := make([]string, 0, len(raw.Enabled))
	seenEnabled := map[string]bool{}
	for _, name := range raw.Enabled {
		if !ValidPluginName(name) {
			return configErr("config_plugin_invalid", "plugins.enabled")
		}
		if !seenEnabled[name] {
			seenEnabled[name] = true
			enabled = append(enabled, name)
		}
	}
	disabled := make([]string, 0, len(raw.Disabled))
	seenDisabled := map[string]bool{}
	for _, name := range raw.Disabled {
		if !ValidPluginName(name) {
			return configErr("config_plugin_invalid", "plugins.disabled")
		}
		if !seenDisabled[name] {
			seenDisabled[name] = true
			disabled = append(disabled, name)
		}
	}
	for _, name := range enabled {
		if seenDisabled[name] {
			return configErr("config_plugin_invalid", "plugins")
		}
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
	sort.Strings(enabled)
	sort.Strings(disabled)
	cfg.PluginEnabled = enabled
	cfg.PluginDisabled = disabled
	if len(entries) == 0 {
		entries = nil
	}
	cfg.PluginEntries = entries
	return nil
}

// PluginEnabledForProfile reports whether a plugin may load for the profile:
// listed in enabled and not in disabled. Missing enabled list means nothing is
// enabled yet (opt-in); explicit empty means intentionally none.
func (c *Config) PluginEnabledForProfile(name string) bool {
	if c == nil {
		return false
	}
	for _, blocked := range c.PluginDisabled {
		if blocked == name {
			return false
		}
	}
	for _, allowed := range c.PluginEnabled {
		if allowed == name {
			return true
		}
	}
	return false
}
