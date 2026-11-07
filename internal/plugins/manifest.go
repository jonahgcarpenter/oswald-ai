package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var pluginNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,39}$`)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var npmPackageRE = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._~-]*/)?[a-z0-9][a-z0-9._~-]*$`)

var npmVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

var npmBinRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// manifestYAML is the on-disk plugin.yaml shape. Unknown keys are rejected by
// KnownFields decoding so typos fail closed.
type manifestYAML struct {
	Name              string   `yaml:"name"`
	Version           string   `yaml:"version"`
	Description       string   `yaml:"description"`
	Entry             string   `yaml:"entry"`
	RequiresEnv       []string `yaml:"requires_env"`
	Capabilities      []string `yaml:"capabilities"`
	AllowToolOverride bool     `yaml:"allow_tool_override"`
	NodePackage       string   `yaml:"node_package"`
	NodeVersion       string   `yaml:"node_version"`
	NodeBin           string   `yaml:"node_bin"`
	ManifestVersion   int      `yaml:"manifest_version"`
	ApiVersion        string   `yaml:"api_version"`
	RequiresPlugins   []string `yaml:"requires_plugins"`
}

// ParseManifestFile reads and validates dir/plugin.yaml. dir names the plugin
// directory for key derivation; source names bundled, user, or project.
func ParseManifestFile(dir, source string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read plugin manifest: %w", err)
	}
	if len(data) > 64*1024 {
		return nil, fmt.Errorf("plugin manifest exceeds size limit")
	}
	var raw manifestYAML
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse plugin manifest: %w", err)
	}
	m := &Manifest{
		Name:              strings.TrimSpace(raw.Name),
		Version:           strings.TrimSpace(raw.Version),
		Description:       strings.TrimSpace(raw.Description),
		Entry:             strings.TrimSpace(raw.Entry),
		RequiresEnv:       append([]string(nil), raw.RequiresEnv...),
		Capabilities:      append([]string(nil), raw.Capabilities...),
		AllowToolOverride: raw.AllowToolOverride,
		NodePackage:       strings.TrimSpace(raw.NodePackage),
		NodeVersion:       strings.TrimSpace(raw.NodeVersion),
		NodeBin:           strings.TrimSpace(raw.NodeBin),
		ManifestVersion:   raw.ManifestVersion,
		ApiVersion:        strings.TrimSpace(raw.ApiVersion),
		Source:            source,
		Path:              dir,
	}
	for _, dep := range raw.RequiresPlugins {
		if trimmed := strings.TrimSpace(dep); trimmed != "" {
			m.RequiresPlugins = append(m.RequiresPlugins, trimmed)
		}
	}
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}
	m.Key = ManifestKey(m, dir)
	return m, nil
}

// ManifestKey derives the canonical registry key. Subcategory dirs
// (platforms/foo) qualify as platforms/foo; flat dirs key by manifest name.
// Keys are exact: no bare-name aliases exist, so enablement, config, and
// storage all use this key verbatim.
func ManifestKey(m *Manifest, dir string) string {
	base := filepath.Base(filepath.Dir(dir))
	if base != "" && base != "plugins" && base != "." && base != "/" {
		leaf := filepath.Base(dir)
		// Only qualify when nested one level under a category dir; flat names win.
		if isCategoryDir(base) {
			return base + "/" + leaf
		}
	}
	return m.Name
}

// isCategoryDir reports known nesting buckets that qualify keys.
func isCategoryDir(name string) bool {
	switch name {
	case "platforms", "image_gen", "delegation", "memory", "context_engine", "model-providers":
		return true
	default:
		return false
	}
}

// ManifestKeyForLookup is intentionally absent: keys are canonical and exact.
// Enablement, config entries, storage, and dependency references all use the
// ManifestKey verbatim with no bare-name aliases.

// validPluginKey reports whether a canonical plugin key (flat name or
// single-level category/name) is well-formed.
func ValidPluginKey(key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) < 1 || len(parts) > 2 {
		return false
	}
	for _, part := range parts {
		if !pluginNameRE.MatchString(part) || part == "soul" {
			return false
		}
	}
	return true
}

// ValidateManifest enforces naming, bounds, and capability vocabulary. It
// never includes values in errors beyond the offending field shape.
func ValidateManifest(m *Manifest) error {
	if !pluginNameRE.MatchString(m.Name) || m.Name == "soul" {
		return fmt.Errorf("invalid plugin name %q", m.Name)
	}
	if len(m.Version) > 64 || strings.ContainsAny(m.Version, "\r\n\x00") {
		return fmt.Errorf("invalid plugin version for %q", m.Name)
	}
	if len([]rune(m.Description)) > 512 || strings.ContainsRune(m.Description, 0) {
		return fmt.Errorf("invalid plugin description for %q", m.Name)
	}
	if len(m.Entry) > 1024 || strings.ContainsRune(m.Entry, 0) {
		return fmt.Errorf("invalid plugin entry for %q", m.Name)
	}
	if len(m.RequiresEnv) > 32 {
		return fmt.Errorf("plugin %q lists too many required variables", m.Name)
	}
	for _, name := range m.RequiresEnv {
		if !envNameRE.MatchString(name) {
			return fmt.Errorf("plugin %q names invalid variable", m.Name)
		}
	}
	if len(m.Capabilities) > 16 {
		return fmt.Errorf("plugin %q declares too many capabilities", m.Name)
	}
	for _, cap := range m.Capabilities {
		if !ValidCapabilities[cap] {
			return fmt.Errorf("plugin %q declares unknown capability", m.Name)
		}
	}
	if m.ManifestVersion != 2 {
		return fmt.Errorf("plugin %q uses unsupported manifest_version (want 2)", m.Name)
	}
	if m.ApiVersion != "1" {
		return fmt.Errorf("plugin %q uses unsupported api_version (want \"1\")", m.Name)
	}
	if len(m.RequiresPlugins) > 16 {
		return fmt.Errorf("plugin %q lists too many required plugins", m.Name)
	}
	seen := map[string]bool{}
	for _, dep := range m.RequiresPlugins {
		if dep == m.Name || dep == m.Key {
			return fmt.Errorf("plugin %q cannot require itself", m.Name)
		}
		if !ValidPluginKey(dep) {
			return fmt.Errorf("plugin %q requires invalid plugin key", m.Name)
		}
		if seen[dep] {
			return fmt.Errorf("plugin %q lists a duplicate required plugin", m.Name)
		}
		seen[dep] = true
	}
	if strings.TrimSpace(m.Source) == "" {
		return fmt.Errorf("plugin %q has no source", m.Name)
	}
	if err := ValidateNodeDeps(m); err != nil {
		return err
	}
	return nil
}

// ValidateNodeDeps enforces pinned npm declarations. Ranges, tags, URLs, and
// file specs are rejected so installs resolve identically every time.
func ValidateNodeDeps(m *Manifest) error {
	hasPackage := strings.TrimSpace(m.NodePackage) != ""
	hasVersion := strings.TrimSpace(m.NodeVersion) != ""
	hasBin := strings.TrimSpace(m.NodeBin) != ""
	if !hasPackage && !hasVersion && !hasBin {
		return nil
	}
	if hasPackage != hasVersion {
		return fmt.Errorf("plugin %q must pin node_package with an exact node_version", m.Name)
	}
	if len(m.NodePackage) > 214 || !npmPackageRE.MatchString(m.NodePackage) {
		return fmt.Errorf("plugin %q names an invalid node_package", m.Name)
	}
	if len(m.NodeVersion) > 64 || !npmVersionRE.MatchString(m.NodeVersion) {
		return fmt.Errorf("plugin %q pins an invalid node_version (want exact x.y.z)", m.Name)
	}
	if hasBin && (len(m.NodeBin) > 64 || !npmBinRE.MatchString(m.NodeBin)) {
		return fmt.Errorf("plugin %q names an invalid node_bin", m.Name)
	}
	return nil
}

// DefaultNodeBin returns the effective binary name: explicit node_bin or the
// package base name with any scope stripped.
func DefaultNodeBin(m Manifest) string {
	if strings.TrimSpace(m.NodeBin) != "" {
		return m.NodeBin
	}
	base := m.NodePackage
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return base
}
