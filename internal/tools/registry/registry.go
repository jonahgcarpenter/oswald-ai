package registry

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// Handler is the execution function for a single tool.
// It receives the model's tool call arguments and returns typed productivity
// plus the content injected as a tool response. ctx propagates cancellation.
type Handler func(ctx context.Context, arguments map[string]interface{}) (governance.Result, error)

// ParamSpec describes a parameter exposed in a catalog entry for MCP discovery.
type ParamSpec struct {
	Name        string
	Type        string
	Required    bool
	Description string
	Enum        []string
}

// ToolSourceMCP identifies tools discovered from connected MCP servers.
const ToolSourceMCP = "mcp"

// CatalogEntry is a registry tool definition annotated with its source.
type CatalogEntry struct {
	Name        string
	Description string
	Source      string
	Server      string
	Parameters  []ParamSpec
}

// ToolVisibility controls which builtin tools are hidden for the active request.
type ToolVisibility struct {
	HiddenBuiltins map[string]bool
}

// Registry maps tool definitions to registered handlers and policies.
type Registry struct {
	definitions map[string]llm.ToolDefinition
	handlers    map[string]Handler
	policies    map[string]governance.ToolPolicy
	disabled    map[string]bool
	log         *config.Logger
}

// New creates an empty Registry. Register definitions before their handlers.
func New(log *config.Logger) *Registry {
	return &Registry{
		definitions: make(map[string]llm.ToolDefinition),
		handlers:    make(map[string]Handler),
		policies:    make(map[string]governance.ToolPolicy),
		disabled:    make(map[string]bool),
		log:         log,
	}
}

// DisableBuiltin keeps a builtin name reserved while removing it from model-visible catalogs.
func (r *Registry) DisableBuiltin(name string) error {
	_, ok := r.definitions[name]
	if !ok {
		return fmt.Errorf("cannot disable builtin %q: no tool definition registered with that name", name)
	}
	if _, ok := r.handlers[name]; ok {
		return fmt.Errorf("cannot disable builtin %q after registering its handler", name)
	}
	r.disabled[name] = true
	r.log.Debug("tool.registry.builtin_disabled", "disabled builtin tool", config.F("tool_name", name))
	return nil
}

// RegisterDefinition adds a model-facing builtin definition before handler registration.
func (r *Registry) RegisterDefinition(def llm.ToolDefinition) error {
	if strings.TrimSpace(def.Name) == "" || strings.TrimSpace(def.Description) == "" || def.Parameters.Type != "object" || def.Parameters.Properties == nil {
		return fmt.Errorf("invalid tool definition: name, description, and object parameters with properties are required")
	}
	if _, exists := r.definitions[def.Name]; exists {
		return fmt.Errorf("tool definition %q already registered", def.Name)
	}
	r.definitions[def.Name] = def
	r.log.Debug("tool.registry.definition_registered", "registered tool definition", config.F("tool_name", def.Name))
	return nil
}

// RegisterHandler associates a Handler with a tool name.
// Returns an error if the name does not match a registered definition, to catch
// typos and orphaned handlers early at startup.
func (r *Registry) RegisterHandler(name string, policy governance.ToolPolicy, handler Handler) error {
	if _, ok := r.definitions[name]; !ok {
		return fmt.Errorf("cannot register handler for %q: no tool definition registered with that name", name)
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("invalid policy for tool %q: %w", name, err)
	}
	r.handlers[name] = handler
	r.policies[name] = policy
	r.log.Debug("tool.registry.handler_registered", "registered tool handler", config.F("tool_name", name))
	return nil
}

// LLMTools returns builtins as the []llm.Tool slice passed to ChatRequest.Tools.
func (r *Registry) LLMTools() []llm.Tool {
	return r.LLMToolsForVisibility(ToolVisibility{})
}

// LLMToolsForVisibility converts registered definitions into the []llm.Tool slice passed to
// ChatRequest.Tools, excluding disabled and request-hidden builtins.
func (r *Registry) LLMToolsForVisibility(visibility ToolVisibility) []llm.Tool {
	tools := make([]llm.Tool, 0, len(r.definitions))
	for _, name := range r.Names() {
		if r.disabled[name] || visibility.HiddenBuiltins[name] {
			continue
		}
		definition := r.definitions[name]
		definition.Description = strings.TrimSpace(definition.Description)
		tools = append(tools, llm.Tool{
			Type:     "function",
			Function: definition,
		})
	}
	return tools
}

// EnabledBuiltinNames returns executable, model-visible builtin names in stable order.
func (r *Registry) EnabledBuiltinNames() []string {
	names := make([]string, 0, len(r.handlers))
	for name := range r.handlers {
		if !r.disabled[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Execute calls the registered handler for the named tool with the given arguments.
// Returns an error if no handler is registered for the tool name.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]interface{}) (governance.Result, error) {
	handler, ok := r.handlers[name]
	if !ok {
		return governance.Result{}, r.unknownToolError(name)
	}
	return handler(ctx, args)
}

// Policy returns the validated runtime governance policy for a builtin tool.
func (r *Registry) Policy(name string) (governance.ToolPolicy, bool) {
	policy, ok := r.policies[name]
	return policy, ok
}

func (r *Registry) unknownToolError(name string) error {
	if prefix, ok := toolPrefix(name); ok {
		return fmt.Errorf("no handler registered for tool %q; available tools in prefix %q: %s", name, prefix, strings.Join(r.handlerNamesWithPrefix(prefix), ", "))
	}
	return fmt.Errorf("no handler registered for tool %q; available tools: %s", name, strings.Join(r.handlerNames(), ", "))
}

func toolPrefix(name string) (string, bool) {
	prefix, _, ok := strings.Cut(name, ".")
	if !ok || prefix == "" {
		return "", false
	}
	return prefix, true
}

func (r *Registry) handlerNamesWithPrefix(prefix string) []string {
	names := make([]string, 0)
	needle := prefix + "."
	for name := range r.handlers {
		if strings.HasPrefix(name, needle) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

func (r *Registry) handlerNames() []string {
	names := make([]string, 0, len(r.handlers))
	for name := range r.handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

// HasHandler returns true if a handler has been registered for the given tool name.
func (r *Registry) HasHandler(name string) bool {
	_, ok := r.handlers[name]
	return ok
}

// Count returns the number of registered tool definitions.
func (r *Registry) Count() int {
	return len(r.definitions)
}

// Names returns registered tool names in stable sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.definitions))
	for name := range r.definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
