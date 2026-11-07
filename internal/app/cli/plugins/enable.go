package plugins

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
)

// enableCmd opts in to a discovered plugin: adds to plugins.enabled and drops
// any disabled entry so both-lists validation never trips.
func enableCmd(root, flagProfile, raw string, stdout, stderr io.Writer) int {
	name := strings.ToLower(strings.TrimSpace(raw))
	if !plugins.ValidPluginKey(name) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", raw)
		return exitUsage
	}
	profileCfg, profileName, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	winners := discoverForList(globalRootFor(profileRoot, profileName))
	if _, ok := winners[name]; !ok {
		fmt.Fprintf(stderr, "oswald: unknown plugin %q\n", raw)
		return exitError
	}
	enabled := append([]string(nil), profileCfg.PluginEnabled...)
	seen := map[string]bool{}
	for _, existing := range enabled {
		seen[existing] = true
	}
	if !seen[name] {
		enabled = append(enabled, name)
	}
	sort.Strings(enabled)
	disabled := make([]string, 0, len(profileCfg.PluginDisabled))
	for _, denied := range profileCfg.PluginDisabled {
		if denied != name {
			disabled = append(disabled, denied)
		}
	}
	oldCfg, _ := config.Validate(mustAbs(root))
	if err := config.SetConfigValue(mustAbs(root), profileName, []string{"plugins", "enabled"}, encodeStringList(enabled)); err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	if err := config.SetConfigValue(mustAbs(root), profileName, []string{"plugins", "disabled"}, encodeStringList(disabled)); err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "enabled plugin %s (%s)\n", name, effectForChange(mustAbs(root), oldCfg))
	return exitOK
}

// disableCmd opts out: drops from enabled and records in disabled, which
// always wins at runtime.
func disableCmd(root, flagProfile, raw string, stdout, stderr io.Writer) int {
	name := strings.ToLower(strings.TrimSpace(raw))
	if !plugins.ValidPluginKey(name) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", raw)
		return exitUsage
	}
	profileCfg, profileName, _, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	enabled := make([]string, 0, len(profileCfg.PluginEnabled))
	for _, existing := range profileCfg.PluginEnabled {
		if existing != name {
			enabled = append(enabled, existing)
		}
	}
	disabled := append([]string(nil), profileCfg.PluginDisabled...)
	seen := map[string]bool{}
	for _, denied := range disabled {
		seen[denied] = true
	}
	if !seen[name] {
		disabled = append(disabled, name)
	}
	sort.Strings(enabled)
	sort.Strings(disabled)
	oldCfg, _ := config.Validate(mustAbs(root))
	if err := config.SetConfigValue(mustAbs(root), profileName, []string{"plugins", "enabled"}, encodeStringList(enabled)); err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	if err := config.SetConfigValue(mustAbs(root), profileName, []string{"plugins", "disabled"}, encodeStringList(disabled)); err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "disabled plugin %s (%s)\n", name, effectForChange(mustAbs(root), oldCfg))
	return exitOK
}

func encodeStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
