package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// IsSkippedPluginDir reports cache or foreign-harness dirs discovery never treats
// as plugins. One unreadable dir must not abort the whole sweep.
func IsSkippedPluginDir(name string) bool {
	if strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__") {
		return true
	}
	switch name {
	case "__pycache__", "__MACOSX__", "plugins-backup":
		return true
	}
	return false
}

// ScanProfilePlugins reads manifests under <profileRoot>/plugins: flat
// <plugins>/<name>/code/plugin.yaml, keyed by manifest name. Failures are
// per-child and fail closed; a missing plugins dir reads as empty.
func ScanProfilePlugins(profileRoot string) []Manifest {
	if strings.TrimSpace(profileRoot) == "" {
		return nil
	}
	root := ProfilePluginsDir(profileRoot)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []Manifest
	for _, entry := range entries {
		if !entry.IsDir() || IsSkippedPluginDir(entry.Name()) {
			continue
		}
		codeDir := PluginCodeDir(profileRoot, entry.Name())
		info, err := os.Stat(filepath.Join(codeDir, "plugin.yaml"))
		if err != nil || !info.Mode().IsRegular() {
			if info, err := os.Stat(filepath.Join(codeDir, "plugin.yml")); err != nil || !info.Mode().IsRegular() {
				continue
			}
		}
		m, err := ParseManifestFile(codeDir, "profile")
		if err != nil {
			continue
		}
		m.Key = m.Name
		if !ValidPluginKey(m.Key) || entry.Name() != m.Key {
			// Directory must match the manifest name; a mismatch is a
			// misplaced tree, never a rename.
			continue
		}
		out = append(out, *m)
	}
	return out
}

// ScanProfileWinners indexes the profile scan by canonical key.
func ScanProfileWinners(profileRoot string) map[string]Manifest {
	winners := make(map[string]Manifest)
	for _, m := range ScanProfilePlugins(profileRoot) {
		key := m.Key
		if key == "" {
			key = m.Name
		}
		if key == "" {
			continue
		}
		winners[key] = m
	}
	return winners
}

// ResolveLoadOrder returns canonical keys in dependency order (dependencies
// first) for the enabled subset. Unknown dependencies and cycles fail closed
// with the offending key named.
func ResolveLoadOrder(winners map[string]Manifest, enabled []string) ([]string, error) {
	ordered := make([]string, 0, len(enabled))
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(key string) error
	visit = func(key string) error {
		if visited[key] {
			return nil
		}
		if visiting[key] {
			return fmt.Errorf("plugin dependency cycle at %q", key)
		}
		m, ok := winners[key]
		if !ok {
			return fmt.Errorf("unknown plugin %q", key)
		}
		visiting[key] = true
		deps := append([]string(nil), m.RequiresPlugins...)
		sort.Strings(deps)
		for _, dep := range deps {
			if _, ok := winners[dep]; !ok {
				return fmt.Errorf("plugin %q requires unknown plugin %q", key, dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		delete(visiting, key)
		visited[key] = true
		ordered = append(ordered, key)
		return nil
	}
	keys := append([]string(nil), enabled...)
	sort.Strings(keys)
	for _, key := range keys {
		if err := visit(key); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}
