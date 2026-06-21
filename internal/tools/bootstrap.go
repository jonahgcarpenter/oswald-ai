package tools

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	toolmcp "github.com/jonahgcarpenter/oswald-ai/internal/tools/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// NewRegistryFromConfig creates a Registry, loads tool definitions, and wires builtin tools.
// The soul store and user memory store are created externally and passed in so the agent
// can share the same instances with the tool handlers.
// chatClient and model are retained in the signature because the agent and tools
// share bootstrap wiring, but fresh memory storage has no legacy migration path.
func NewRegistryFromConfig(cfg *config.Config, soulStore *soul.Store, userMemStore *usermemory.Store, sessionStore *memory.Store, chatClient llm.Chatter, model string, mcpManager *mcp.Manager, log *config.Logger) (*registry.Registry, error) {
	bootstrapLog := log.Server("tool.bootstrap")
	reg, err := registry.NewFromDirectory(config.DefaultToolsConfigDir, log.Server("tool.registry"))
	if err != nil {
		return nil, err
	}

	if err := builtin.Register(reg, cfg, soulStore, userMemStore, sessionStore, chatClient, model, mcpManager, log); err != nil {
		return nil, err
	}
	if err := toolmcp.Register(reg, mcpManager, log); err != nil {
		return nil, err
	}

	bootstrapLog.Info("tool.bootstrap.enabled", "enabled tools", config.F("tool_count", reg.Count()), config.F("tools", strings.Join(reg.Names(), ",")))
	return reg, nil
}
