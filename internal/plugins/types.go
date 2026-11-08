// Package plugins owns the profile-scoped plugin host: manifest contracts,
// directory discovery, and per-profile registration tracking. Code is shared
// across profiles; enablement, config, and storage stay fenced to one profile
// root, mirroring the mcp_servers island rule.
package plugins

import (
	"context"
	"fmt"
	"time"
)

// Hook names accepted by RegisterHook. Directive hooks may block or rewrite;
// observer hooks must never block a turn.
const (
	HookPreToolCall     = "pre_tool_call"
	HookPostToolCall    = "post_tool_call"
	HookPromptAdmission = "prompt_admission"
	HookContextBuild    = "context_build"
	HookSessionStart    = "session_start"
	HookSessionEnd      = "session_end"
	HookPreCommand      = "pre_command"
)

// ValidHooks is the closed hook vocabulary. Shell-hook style free-form names
// are rejected; new names land only with a real fire-site.
var ValidHooks = map[string]bool{
	HookPreToolCall:     true,
	HookPostToolCall:    true,
	HookPromptAdmission: true,
	HookContextBuild:    true,
	HookSessionStart:    true,
	HookSessionEnd:      true,
	HookPreCommand:      true,
}

// ValidCapabilities is the consent vocabulary for privileged surfaces.
// Unknown capabilities fail closed at install time.
var ValidCapabilities = map[string]bool{
	"tools.override":           true,
	"llm.model_override":       true,
	"llm.provider_override":    true,
	"gateway.platform_actions": true,
}

// Manifest describes one discovered plugin directory or entry-point.
type Manifest struct {
	// Name is the operator-facing id (2–40 lowercase ASCII, soul reserved).
	Name string
	// Version is an opaque operator string, bounded for logging.
	Version string
	// Description is human-facing help text, never logged verbatim in full.
	Description string
	// Entry is the optional subprocess binary relative to the plugin dir.
	// Empty means in-process Go plugin (bundled only).
	Entry string
	// RequiresEnv names dotenv variables the plugin needs. Values are never stored.
	RequiresEnv []string
	// Capabilities declares privileged surfaces needing operator consent.
	Capabilities []string
	// AllowToolOverride permits replacing a same-named builtin tool.
	AllowToolOverride bool
	// ManifestVersion is the plugin contract version. Only 2 is accepted.
	ManifestVersion int
	// ApiVersion is the host API version the plugin targets. Only "1" is accepted.
	ApiVersion string
	// RequiresPlugins names canonical plugin keys that must load first.
	RequiresPlugins []string
	// NodePackage is an optional npm package name backing a plugin CLI
	// (for example "prettier" or "@scope/tool"). Empty means no Node deps.
	NodePackage string
	// NodeVersion is the exact pinned npm version (x.y.z, no ranges).
	// Required when NodePackage is set.
	NodeVersion string
	// NodeBin names the executable in node_modules/.bin. Empty defaults to
	// the package base name (scope stripped).
	NodeBin string
	// Source is bundled, user, project, or entrypoint.
	Source string
	// Path is the plugin directory or entry-point value. Never logged with secrets.
	Path string
	// Key is the registry key (category-qualified for subdirs).
	Key string
}

// Registration is a host-owned cleanup lease. Dispose removes the registration
// only while it is still current; late or duplicate disposes are no-ops.
type Registration struct {
	Kind string
	Key  string
	// dispose runs once under the manager lock.
	dispose func()
	// disposed guards double-dispose.
	disposed bool
}

// Dispose releases the registration. It never fails and never logs values.
func (r *Registration) Dispose() {
	if r == nil || r.disposed {
		return
	}
	r.disposed = true
	if r.dispose != nil {
		r.dispose()
	}
}

// HookFunc observes or directs one lifecycle event. The event map is owned and
// mutable by the host; hooks must not retain it after return. Returning a
// non-nil error from a directive hook blocks the operation fail-closed.
type HookFunc func(ctx context.Context, event map[string]any) error

// HookInvoker is the minimal host surface the agent needs to fire lifecycle
// hooks without importing the concrete Manager. The timeout bounds one hook
// chain; directive pre_tool_call failures fail closed.
type HookInvoker interface {
	InvokeHook(ctx context.Context, name string, event map[string]any, timeout time.Duration) error
}

// PluginToolOverrideError reports a blocked builtin replacement without the
// operator opt-in entry.
type PluginToolOverrideError struct {
	Plugin string
	Tool   string
}

// Error implements the error interface without naming values.
func (e *PluginToolOverrideError) Error() string {
	return fmt.Sprintf("plugin %q cannot override built-in tool %q without allow_tool_override", e.Plugin, e.Tool)
}

// LoadedPlugin is the runtime state for one loaded plugin.
type LoadedPlugin struct {
	Manifest Manifest
	Enabled  bool
	Error    string
}
