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
	case "__pycache__", "__MACOSX__":
		return true
	}
	return false
}

// ScanDirectory reads manifests under path: flat <root>/<name>/plugin.yaml (key
// name) or category <root>/<cat>/<name>/plugin.yaml (key cat/name). A depth cap
// of two keeps walks bounded; failures are per-child and fail closed.
func ScanDirectory(path, source string) []Manifest {
	var out []Manifest
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if !entry.IsDir() || IsSkippedPluginDir(entry.Name()) {
			continue
		}
		dir := filepath.Join(path, entry.Name())
		if hasManifest(dir) {
			m, err := ParseManifestFile(dir, source)
			if err != nil {
				continue
			}
			m.Key = entry.Name()
			if m.Name != "" && m.Key != m.Name {
				// Flat dir keyed by directory: a same-named directory is the
				// documented way to override; a differently named directory
				// gets its own key and never shadows the manifest name.
				m.Key = entry.Name()
			} else {
				m.Key = m.Name
			}
			out = append(out, *m)
			continue
		}
		// One-level category recurse: <cat>/<name>/plugin.yaml.
		subs, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() || IsSkippedPluginDir(sub.Name()) {
				continue
			}
			subDir := filepath.Join(dir, sub.Name())
			if !hasManifest(subDir) {
				continue
			}
			m, err := ParseManifestFile(subDir, source)
			if err != nil {
				continue
			}
			m.Key = entry.Name() + "/" + sub.Name()
			out = append(out, *m)
		}
	}
	return out
}

func hasManifest(dir string) bool {
	for _, name := range []string{"plugin.yaml", "plugin.yml"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// CollectManifests scans bundled, user, and opt-in project roots in order.
func CollectManifests(bundledDir, userDir, projectDir string, projectEnabled bool) []Manifest {
	var out []Manifest
	if bundledDir != "" {
		out = append(out, ScanDirectory(bundledDir, "bundled")...)
	}
	if userDir != "" {
		out = append(out, ScanDirectory(userDir, "user")...)
	}
	if projectEnabled && projectDir != "" {
		out = append(out, ScanDirectory(projectDir, "project")...)
	}
	return out
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

// ResolveWinners applies last-writer-wins on key collision (project > user >
// bundled). A flat user/project manifest claiming a bundled key from a
// differently named directory is an impostor and is skipped so enabling one
// name never runs unrelated code.
func ResolveWinners(manifests []Manifest) map[string]Manifest {
	winners := make(map[string]Manifest)
	order := map[string]int{"bundled": 0, "user": 1, "project": 2, "entrypoint": 3}
	for _, m := range manifests {
		key := m.Key
		if key == "" {
			key = m.Name
		}
		prev, ok := winners[key]
		if !ok {
			winners[key] = m
			continue
		}
		if prev.Source == "bundled" && (m.Source == "user" || m.Source == "project") {
			prevDir := filepath.Base(prev.Path)
			ownDir := filepath.Base(m.Path)
			if prevDir != "" && ownDir != "" && prevDir != ownDir && !strings.Contains(key, "/") {
				// Impostor: same manifest name, different directory.
				continue
			}
		}
		if order[m.Source] >= order[prev.Source] {
			winners[key] = m
		}
	}
	return winners
}
