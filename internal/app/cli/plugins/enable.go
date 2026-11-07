package plugins

import (
	"fmt"
	"io"
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
	_, profileName, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	winners := discoverForList(globalRootFor(profileRoot, profileName))
	if _, ok := winners[name]; !ok {
		fmt.Fprintf(stderr, "oswald: unknown plugin %q\n", raw)
		return exitError
	}
	oldCfg, _ := config.Validate(mustAbs(root))
	if err := setEnabledForProfile(mustAbs(root), profileName, name, true); err != nil {
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
	_, profileName, _, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	oldCfg, _ := config.Validate(mustAbs(root))
	if err := setEnabledForProfile(mustAbs(root), profileName, name, false); err != nil {
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
