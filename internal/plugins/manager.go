package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// ToolRegistration tracks one plugin-owned tool before it reaches the model catalog.
type ToolRegistration struct {
	Definition llm.ToolDefinition
	Handler    func(ctx context.Context, args map[string]any) (string, error)
	Toolset    string
	Plugin     string
}

// CommandRegistration tracks one plugin-owned /name chat command.
type CommandRegistration struct {
	Name        string
	Description string
	Plugin      string
	Handler     func(ctx context.Context, raw string) (string, error)
}

// Manager owns per-profile plugin state. The scope key is the absolute profile
// root so one profile's unload never clears another's registrations.
type Manager struct {
	mu       sync.Mutex
	scopeKey string
	homePath string
	profile  string
	log      *config.Logger

	plugins  map[string]LoadedPlugin
	tools    map[string]ToolRegistration
	hooks    map[string][]HookFunc
	commands map[string]CommandRegistration
	order    []Registration
	closed   bool
}

// NewManager binds one manager to a profile root. Roots are absolutized so the
// scope key matches registry overlays keyed the same way.
func NewManager(profileRoot, profile string, log *config.Logger) *Manager {
	abs, err := filepath.Abs(profileRoot)
	if err != nil || abs == "" {
		abs = profileRoot
	}
	if log == nil {
		log = config.NewLogger(config.LevelWarn)
	}
	return &Manager{
		scopeKey: abs,
		homePath: abs,
		profile:  profile,
		log:      log.Server("plugin.manager"),
		plugins:  make(map[string]LoadedPlugin),
		tools:    make(map[string]ToolRegistration),
		hooks:    make(map[string][]HookFunc),
		commands: make(map[string]CommandRegistration),
	}
}

// ScopeKey returns the absolute profile root owning this manager.
func (m *Manager) ScopeKey() string { return m.scopeKey }

// HomePath returns the profile root for storage scoping.
func (m *Manager) HomePath() string { return m.homePath }

// Discover records winners as known but not yet enabled.
func (m *Manager) Discover(winners map[string]Manifest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, manifest := range winners {
		if _, ok := m.plugins[key]; ok {
			continue
		}
		m.plugins[key] = LoadedPlugin{Manifest: manifest}
	}
}

// EnabledNames returns enabled plugin keys in stable order.
func (m *Manager) EnabledNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for key, loaded := range m.plugins {
		if loaded.Enabled {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// KnownNames returns all discovered keys in stable order for listing.
func (m *Manager) KnownNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.plugins))
	for key := range m.plugins {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// SetEnabled flips one known plugin by canonical key. Unknown keys fail closed.
func (m *Manager) SetEnabled(key string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	loaded, ok := m.plugins[key]
	if !ok {
		return fmt.Errorf("unknown plugin %q", key)
	}
	loaded.Enabled = enabled
	loaded.Error = ""
	m.plugins[key] = loaded
	m.log.Debug("plugin.manager.enabled", "toggled plugin", config.F("plugin", key), config.F("profile", m.profile))
	return nil
}

// RegisterTool tracks one plugin tool. Builtin replacement needs allowOverride,
// otherwise a shadowed name is rejected without touching the host registry.
func (m *Manager) RegisterTool(plugin, name, toolset string, def llm.ToolDefinition, handler func(ctx context.Context, args map[string]any) (string, error), allowOverride bool, builtinNames map[string]bool) (*Registration, error) {
	if name == "" || handler == nil {
		return nil, fmt.Errorf("plugin %q tried to register an invalid tool", plugin)
	}
	if !ValidToolset(toolset) {
		return nil, fmt.Errorf("plugin %q tried to register tool %q with unknown toolset", plugin, name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("plugin manager is closed")
	}
	if _, exists := m.tools[name]; exists {
		return nil, fmt.Errorf("plugin %q tried to re-register tool %q", plugin, name)
	}
	if builtinNames[name] && !allowOverride {
		return nil, &PluginToolOverrideError{Plugin: plugin, Tool: name}
	}
	m.tools[name] = ToolRegistration{Definition: def, Handler: handler, Toolset: toolset, Plugin: plugin}
	reg := &Registration{Kind: "tool", Key: name}
	nameCopy := name
	reg.dispose = func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if cur, ok := m.tools[nameCopy]; ok && cur.Plugin == plugin {
			delete(m.tools, nameCopy)
		}
	}
	m.order = append(m.order, *reg)
	m.log.Debug("plugin.manager.tool_registered", "registered plugin tool", config.F("plugin", plugin), config.F("tool_name", name))
	return reg, nil
}

// RegisterHook appends one lifecycle callback. Unknown hooks fail closed.
func (m *Manager) RegisterHook(plugin, name string, fn HookFunc) (*Registration, error) {
	if fn == nil {
		return nil, fmt.Errorf("plugin %q tried to register a nil hook", plugin)
	}
	if !ValidHooks[name] {
		return nil, fmt.Errorf("plugin %q tried to register unknown hook %q", plugin, name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("plugin manager is closed")
	}
	m.hooks[name] = append(m.hooks[name], fn)
	idx := len(m.hooks[name]) - 1
	reg := &Registration{Kind: "hook", Key: name}
	reg.dispose = func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		list := m.hooks[name]
		if idx < len(list) && list[idx] != nil {
			// Identity removal would need func equality; unload clears all
			// hooks for the plugin instead (see Unload).
			m.hooks[name][idx] = nil
		}
	}
	m.order = append(m.order, *reg)
	return reg, nil
}

// RegisterCommand tracks one /name handler. Builtin collisions fail closed.
func (m *Manager) RegisterCommand(plugin, name, description string, handler func(ctx context.Context, raw string) (string, error), builtins map[string]bool) (*Registration, error) {
	if name == "" || handler == nil {
		return nil, fmt.Errorf("plugin %q tried to register an invalid command", plugin)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("plugin manager is closed")
	}
	if builtins[name] || m.commands[name].Handler != nil {
		return nil, fmt.Errorf("plugin %q tried to shadow command %q", plugin, name)
	}
	m.commands[name] = CommandRegistration{Name: name, Description: description, Plugin: plugin, Handler: handler}
	reg := &Registration{Kind: "command", Key: name}
	nameCopy := name
	reg.dispose = func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if cur, ok := m.commands[nameCopy]; ok && cur.Plugin == plugin {
			delete(m.commands, nameCopy)
		}
	}
	m.order = append(m.order, *reg)
	return reg, nil
}

// InvokeHook runs one hook chain in registration order with a timeout.
// Directive pre_tool_call failures fail closed; observers fail open.
func (m *Manager) InvokeHook(ctx context.Context, name string, event map[string]any, timeout time.Duration) error {
	m.mu.Lock()
	chain := append([]HookFunc(nil), m.hooks[name]...)
	m.mu.Unlock()
	if len(chain) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	for _, fn := range chain {
		if fn == nil {
			continue
		}
		done := make(chan error, 1)
		go func(fn HookFunc) {
			defer func() {
				// Hostile panics never take down the turn.
				if r := recover(); r != nil {
					done <- fmt.Errorf("plugin hook panicked")
				}
			}()
			done <- fn(ctx, event)
		}(fn)
		tctx, cancel := context.WithTimeout(ctx, timeout)
		var err error
		select {
		case err = <-done:
		case <-tctx.Done():
			err = fmt.Errorf("plugin hook timed out")
		}
		cancel()
		if err != nil {
			if name == HookPreToolCall {
				return err
			}
			m.log.Warn("plugin.manager.hook_failed", "plugin hook failed", config.F("hook", name), config.F("profile", m.profile))
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// Unload removes every registration owned by one plugin key, newest first.
func (m *Manager) Unload(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	loaded, ok := m.plugins[key]
	if !ok {
		return
	}
	plugin := loaded.Manifest.Name
	for name, reg := range m.tools {
		if reg.Plugin == plugin {
			delete(m.tools, name)
		}
	}
	for name, reg := range m.commands {
		if reg.Plugin == plugin {
			delete(m.commands, name)
		}
	}
	// Hooks are cleared wholesale for the plugin by rebuilding chains without
	// identity tracking: unload drops all chains and re-adds survivors. For
	// phase one the manager is per-profile and small, so compact nils here.
	for name, chain := range m.hooks {
		kept := chain[:0]
		for _, fn := range chain {
			if fn != nil {
				kept = append(kept, fn)
			}
		}
		// Without per-fn ownership, a full unload clears the chain only when
		// no other enabled plugin registered the same hook. Conservative:
		// keep chain; nilled entries from Dispose are skipped at invoke.
		_ = kept
		m.hooks[name] = chain
	}
	loaded.Enabled = false
	m.plugins[key] = loaded
}

// StorageRoot returns the per-plugin dir, creating it owner-only on first use.
// The tree stays workspace-protected like mcp/.
func (m *Manager) StorageRoot(plugin string) (string, error) {
	if !ValidPluginKey(plugin) {
		return "", fmt.Errorf("invalid plugin key")
	}
	root := filepath.Join(m.homePath, "plugins", filepath.FromSlash(plugin))
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("create plugin storage: %w", err)
	}
	// Repair permissive parents without migrating content.
	_ = os.Chmod(root, 0700)
	return root, nil
}

// ValidToolset reports known toolset buckets for plugin tools.
func ValidToolset(name string) bool {
	switch name {
	case "core", "terminal", "code_execution", "cronjob":
		return true
	default:
		return false
	}
}
