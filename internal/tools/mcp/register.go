package mcp

import (
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	mcpmanager "github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Register wires discovered MCP tools into the shared registry.
func Register(reg *registry.Registry, manager *mcpmanager.Manager, log *config.Logger) error {
	if manager == nil || manager.ServerCount() == 0 {
		return nil
	}

	bootstrapLog := log.Server("tool.bootstrap")
	for _, tool := range manager.ToolSpecs() {
		params := make([]registry.ParamSpec, 0, len(tool.Parameters))
		for _, p := range tool.Parameters {
			params = append(params, registry.ParamSpec{
				Name:        p.Name,
				Type:        p.Type,
				Required:    p.Required,
				Description: p.Description,
				Enum:        p.Enum,
			})
		}

		if err := reg.RegisterTool(registry.Spec{
			Name:        tool.Name,
			Description: tool.Description,
			Source:      registry.ToolSourceMCP,
			Server:      tool.Server,
			Parameters:  params,
		}, registry.Handler(tool.Handler)); err != nil {
			return fmt.Errorf("failed to register MCP tool %q: %w", tool.Name, err)
		}
		bootstrapLog.Debug("tool.bootstrap.configured", "configured MCP tool", config.F("tool_name", tool.Name), config.F("source", "mcp"))
	}

	bootstrapLog.Info("tool.bootstrap.mcp_enabled", "enabled MCP tools", config.F("server_count", manager.ServerCount()), config.F("servers", manager.ServerNames()), config.F("tool_count", len(manager.ToolSpecs())))
	return nil
}
