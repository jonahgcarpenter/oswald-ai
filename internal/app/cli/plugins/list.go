package plugins

import (
	"fmt"
	"io"
	"sort"
)

// listCmd prints one line per discovered plugin: key, source, version, state.
// It never executes plugin code and never prints settings values, env values,
// or paths beyond the shared key.
func listCmd(root, flagProfile string, stdout, stderr io.Writer) int {
	profileCfg, profileName, profileRoot, err := resolveScope(root, flagProfile, false)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	winners := discoverForList(globalRootFor(profileRoot, profileName))
	if len(winners) == 0 && len(profileCfg.PluginEnabled) == 0 && len(profileCfg.PluginDisabled) == 0 {
		fmt.Fprintln(stdout, "no plugins")
		return exitOK
	}
	keys := make([]string, 0, len(winners))
	for key := range winners {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Include enabled names with no discovered code so drift is visible.
	for _, name := range profileCfg.PluginEnabled {
		found := false
		for _, key := range keys {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		manifest, ok := winners[key]
		source := "missing"
		version := "-"
		if ok {
			source = manifest.Source
			if manifest.Version != "" {
				version = manifest.Version
			}
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
		fmt.Fprintf(stdout, "%s  %s  %s  %s\n", key, source, version, state)
	}
	return exitOK
}
