package plugins

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// ProfilePluginsDir returns the per-profile install root:
// <profileRoot>/plugins/<name>/{code,data}. Presence means enabled.
func ProfilePluginsDir(profileRoot string) string {
	return filepath.Join(profileRoot, "plugins")
}

// PluginCodeDir returns the immutable install tree for one profile plugin:
// <profileRoot>/plugins/<name>/code. Code updates replace this tree only.
func PluginCodeDir(profileRoot, plugin string) string {
	return filepath.Join(ProfilePluginsDir(profileRoot), filepath.FromSlash(plugin), "code")
}

// PluginDataDir returns the mutable runtime tree for one profile plugin:
// <profileRoot>/plugins/<name>/data. HOME and scratch live here and are
// never touched by install or update.
func PluginDataDir(profileRoot, plugin string) string {
	return filepath.Join(ProfilePluginsDir(profileRoot), filepath.FromSlash(plugin), "data")
}

// RepairProfile tightens permissions on the existing per-profile plugins
// parent without creating anything. Missing paths stay missing so creation
// remains lazy on first install; per-plugin dirs are created on demand. It
// never migrates or deletes operator data. It also regenerates bare-name
// shims so existing installs stay runnable after upgrades.
func RepairProfile(profileRoot string) {
	dir := ProfilePluginsDir(profileRoot)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	_ = os.Chmod(dir, 0700)
	_ = RegenerateShims(profileRoot)
}

// LoadFromProfile discovers profile-local plugins and enables every present
// one whose requires_env is satisfied. Missing variables fail closed for
// that plugin only with a recorded error.
func (m *Manager) LoadFromProfile(profileRoot string) {
	winners := ScanProfileWinners(profileRoot)
	m.Discover(winners)
	keys := make([]string, 0, len(winners))
	for key := range winners {
		keys = append(keys, key)
	}
	ordered, err := ResolveLoadOrder(winners, keys)
	if err != nil {
		m.log.Warn("plugin.manager.order_failed", "plugin dependency order failed", config.F("profile", m.profile))
		return
	}
	for _, key := range ordered {
		loaded, ok := m.plugins[key]
		if !ok {
			continue
		}
		missing := false
		for _, variable := range loaded.Manifest.RequiresEnv {
			if _, ok := os.LookupEnv(variable); !ok {
				missing = true
				break
			}
		}
		if missing {
			m.mu.Lock()
			loaded.Enabled = false
			loaded.Error = "missing required environment"
			m.plugins[key] = loaded
			m.mu.Unlock()
			m.log.Warn("plugin.manager.missing_env", "plugin missing required environment", config.F("plugin", loaded.Manifest.Name), config.F("profile", m.profile))
			continue
		}
		m.mu.Lock()
		loaded.Enabled = true
		loaded.Error = ""
		m.plugins[key] = loaded
		m.mu.Unlock()
		m.log.Debug("plugin.manager.enabled", "enabled profile plugin", config.F("plugin", key), config.F("profile", m.profile))
	}
}

// PluginToolNames returns tracked plugin tool names in stable order.
func (m *Manager) PluginToolNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.tools))
	for name := range m.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// RegisterIntoRegistry publishes tracked plugin tools into the profile
// registry. Builtin replacement needs allowOverride; otherwise shadowing fails
// closed. Plugin tools use full searchable history like workspace file tools.
func (m *Manager) RegisterIntoRegistry(reg *registry.Registry, allowOverride func(plugin string) bool) error {
	m.mu.Lock()
	tools := make([]ToolRegistration, 0, len(m.tools))
	names := make([]string, 0, len(m.tools))
	for name, tool := range m.tools {
		tools = append(tools, tool)
		names = append(names, name)
	}
	m.mu.Unlock()
	sort.Strings(names)
	byName := map[string]ToolRegistration{}
	for _, tool := range tools {
		byName[tool.Definition.Name] = tool
	}
	for _, name := range names {
		tool := byName[name]
		if tool.Definition.Name == "" {
			continue
		}
		// Definition first so handler wiring catches typos early.
		if err := reg.RegisterDefinition(tool.Definition); err != nil {
			// Already registered by a builtin or another plugin: require
			// explicit override consent for this plugin.
			allowed := false
			if allowOverride != nil {
				allowed = allowOverride(tool.Plugin)
			}
			if !allowed {
				return &PluginToolOverrideError{Plugin: tool.Plugin, Tool: name}
			}
			// Override path: registry has no replace API by design, so fail
			// closed until an explicit override slot lands. Record and skip.
			m.log.Warn("plugin.manager.override_deferred", "plugin override needs explicit slot", config.F("plugin", tool.Plugin), config.F("tool_name", name))
			continue
		}
		handler := tool.Handler
		policy := governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}
		// Wrap the string handler into the registry result shape. Real
		// subprocess dispatch lands later; for now in-process handlers run
		// directly and report productive content.
		wrapped := registry.Handler(func(ctx context.Context, args map[string]any) (governance.Result, error) {
			if handler == nil {
				return governance.Result{Content: "plugin tool unavailable in this build", Outcome: governance.OutcomeUnproductive, ReasonCode: "plugin_unavailable"}, nil
			}
			content, err := handler(ctx, args)
			if err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
		})
		if err := reg.RegisterHandler(name, policy, wrapped); err != nil {
			return err
		}
	}
	return nil
}
