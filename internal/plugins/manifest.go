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
		Source:            source,
		Path:              dir,
	}
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}
	m.Key = ManifestKey(m, dir)
	return m, nil
}

// ManifestKey derives the registry key. Subcategory dirs (platforms/foo)
// qualify as platforms/foo; flat dirs key by manifest name.
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

// ManifestKeyForLookup returns the effective key for enable matching: the full
// key plus the bare manifest name as a legacy alias.
func ManifestKeyForLookup(m Manifest) []string {
	if m.Key != "" && m.Key != m.Name {
		return []string{m.Key, m.Name}
	}
	return []string{m.Name}
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
	if strings.TrimSpace(m.Source) == "" {
		return fmt.Errorf("plugin %q has no source", m.Name)
	}
	return nil
}
