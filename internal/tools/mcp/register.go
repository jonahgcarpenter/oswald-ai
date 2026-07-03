package mcp

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	mcpmanager "github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Register is retained for package compatibility. MCP tools are now resolved
// request-locally by internal/mcp.Provider instead of being registered globally.
func Register(reg *registry.Registry, manager *mcpmanager.Manager, log *config.Logger) error {
	_ = reg
	_ = manager
	_ = log
	return nil
}
