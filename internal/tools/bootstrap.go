package tools

import (
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcpclient"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/soulmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/websearch"
)

// NewRegistryFromConfig creates a Registry, loads tool definitions, and wires builtin tools.
// The soul store and user memory store are created externally and passed in so the agent
// can share the same instances with the tool handlers.
// chatClient and model are forwarded to the memory.recall handler so it can perform
// LLM-based migration of old flat-format user memory files on first recall.
func NewRegistryFromConfig(cfg *config.Config, soulStore *soulmemory.Store, userMemStore *usermemory.Store, chatClient ollama.Chatter, model string, mcpManager *mcpclient.Manager, log *config.Logger) (*Registry, error) {
	bootstrapLog := log.Server("tool.bootstrap")
	registry, err := NewRegistryFromDirectory(config.DefaultToolsConfigDir, log.Server("tool.registry"))
	if err != nil {
		return nil, err
	}

	if err := registerBuiltins(registry, cfg, soulStore, userMemStore, chatClient, model, log); err != nil {
		return nil, err
	}
	if err := registerMCPTools(registry, mcpManager, log); err != nil {
		return nil, err
	}

	bootstrapLog.Info("tool.bootstrap.enabled", "enabled tools", config.F("tool_count", registry.Count()), config.F("tools", strings.Join(registry.Names(), ",")))
	return registry, nil
}

// registerBuiltins wires all builtin tools into the shared registry.
func registerBuiltins(registry *Registry, cfg *config.Config, soulStore *soulmemory.Store, userMemStore *usermemory.Store, chatClient ollama.Chatter, model string, log *config.Logger) error {
	bootstrapLog := log.Server("tool.bootstrap")
	searchClient := websearch.NewClient(cfg.SearxngURL, log.Server("tool.web.search"))
	if err := registry.RegisterHandler("web.search", Handler(websearch.NewHandler(searchClient, log))); err != nil {
		return fmt.Errorf("failed to initialize web.search tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured web search tool", config.F("tool_name", "web.search"), config.F("path", cfg.SearxngURL))

	if err := registry.RegisterHandler("memory.remember", Handler(usermemory.NewRememberHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.remember tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.remember"), config.F("path", config.DefaultUserMemoryPath))

	if err := registry.RegisterHandler("memory.recall", Handler(usermemory.NewRecallHandler(userMemStore, chatClient, model, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.recall tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.recall"), config.F("path", config.DefaultUserMemoryPath))

	if err := registry.RegisterHandler("memory.forget", Handler(usermemory.NewForgetHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.forget tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.forget"), config.F("path", config.DefaultUserMemoryPath))

	if err := registry.RegisterHandler("soul.read", Handler(soulmemory.NewReadHandler(soulStore, log))); err != nil {
		return fmt.Errorf("failed to initialize soul.read tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured soul tool", config.F("tool_name", "soul.read"), config.F("path", config.DefaultSoulPath))

	if err := registry.RegisterHandler("soul.patch", Handler(soulmemory.NewPatchHandler(soulStore, log))); err != nil {
		return fmt.Errorf("failed to initialize soul.patch tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured soul tool", config.F("tool_name", "soul.patch"), config.F("path", config.DefaultSoulPath))

	return nil
}

func registerMCPTools(registry *Registry, manager *mcpclient.Manager, log *config.Logger) error {
	if manager == nil || manager.ServerCount() == 0 {
		return nil
	}

	bootstrapLog := log.Server("tool.bootstrap")
	for _, tool := range manager.ToolSpecs() {
		params := make([]ParamSpec, 0, len(tool.Parameters))
		for _, p := range tool.Parameters {
			params = append(params, ParamSpec{
				Name:        p.Name,
				Type:        p.Type,
				Required:    p.Required,
				Description: p.Description,
				Enum:        p.Enum,
			})
		}

		if err := registry.RegisterTool(Spec{
			Name:        tool.Name,
			Description: tool.Description,
			Source:      ToolSourceMCP,
			Server:      tool.Server,
			Parameters:  params,
		}, Handler(tool.Handler)); err != nil {
			return fmt.Errorf("failed to register MCP tool %q: %w", tool.Name, err)
		}
		bootstrapLog.Debug("tool.bootstrap.configured", "configured MCP tool", config.F("tool_name", tool.Name), config.F("source", "mcp"))
	}

	bootstrapLog.Info("tool.bootstrap.mcp_enabled", "enabled MCP tools", config.F("server_count", manager.ServerCount()), config.F("servers", manager.ServerNames()), config.F("tool_count", len(manager.ToolSpecs())))
	return nil
}
