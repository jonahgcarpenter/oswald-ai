package plugins

import (
	"fmt"
	"io"
	"sort"
)

// listCmd prints one line per installed profile plugin: key, source, version.
// Presence means enabled. It never executes plugin code and never prints
// settings values, env values, or paths beyond the plugin key.
func listCmd(root, flagProfile string, stdout, stderr io.Writer) int {
	_, _, profileRoot, err := resolveScope(root, flagProfile, false)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	winners := discoverForList(profileRoot)
	if len(winners) == 0 {
		fmt.Fprintln(stdout, "no plugins")
		return exitOK
	}
	keys := make([]string, 0, len(winners))
	for key := range winners {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		manifest := winners[key]
		source := manifest.Source
		version := manifest.Version
		if version == "" {
			version = "-"
		}
		fmt.Fprintf(stdout, "%s  %s  %s  enabled\n", key, source, version)
	}
	return exitOK
}
