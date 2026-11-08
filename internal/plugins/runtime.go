package plugins

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// UserPluginsDir returns the shared install root for one global root:
// <globalRoot>/plugins-available/<name>/plugin.yaml. Code is shared; enablement
// stays per-profile via config.
func UserPluginsDir(globalRoot string) string {
	return filepath.Join(globalRoot, "plugins-available")
}

// RepairProfile ensures the per-profile plugins parent exists owner-only.
// Per-plugin dirs are created on demand by StorageRoot. It never migrates or
// deletes operator data.
func RepairProfile(profileRoot string) {
	dir := filepath.Join(profileRoot, "plugins")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	_ = os.Chmod(dir, 0700)
}

// LoadEnabledFromConfig discovers shared + opt-in project plugins and enables
// those listed in cfg.PluginEnabled (unless disabled). Missing requires_env
// entries fail closed for that plugin only with a recorded error.
func (m *Manager) LoadEnabledFromConfig(cfg *config.Config, globalRoot string) {
	if cfg == nil {
		return
	}
	userDir := ""
	if globalRoot != "" {
		userDir = UserPluginsDir(globalRoot)
	}
	projectDir := ""
	projectEnabled := false
	if os.Getenv("OSWALD_ENABLE_PROJECT_PLUGINS") == "1" {
		if cwd, err := os.Getwd(); err == nil {
			projectDir = filepath.Join(cwd, ".oswald", "plugins")
			projectEnabled = true
		}
	}
	manifests := CollectManifests("", userDir, projectDir, projectEnabled)
	winners := ResolveWinners(manifests)
	m.Discover(winners)
	ordered, err := ResolveLoadOrder(winners, cfg.PluginEnabled)
	if err != nil {
		m.log.Warn("plugin.manager.order_failed", "plugin dependency order failed", config.F("profile", m.profile))
		return
	}
	for _, key := range ordered {
		// Disabled always wins; config validation already rejects both-lists,
		// but runtime re-checks for defense in depth.
		blocked := false
		for _, denied := range cfg.PluginDisabled {
			if denied == key {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		loaded, ok := m.plugins[key]
		if !ok {
			continue
		}
		missing := false
		for _, variable := range loaded.Manifest.RequiresEnv {
			if _, ok := os.LookupEnv(variable); !ok {
				// Also allow dotenv-resolved values via process env only;
				// config interpolation already fails closed for ${VAR} refs.
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
		_ = m.SetEnabled(key, true)
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

// ProjectPluginsEnabled reports whether opt-in project-local plugins load.
func ProjectPluginsEnabled() bool {
	return strings.TrimSpace(os.Getenv("OSWALD_ENABLE_PROJECT_PLUGINS")) == "1"
}
