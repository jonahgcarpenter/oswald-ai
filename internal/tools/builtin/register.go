package builtin

import (
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	mcpmanager "github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/mcpbrowse"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Register wires all builtin tools into the shared registry.
func Register(reg *registry.Registry, cfg *config.Config, soulStore *soul.Store, userMemStore *usermemory.Store, chatClient llm.Chatter, model string, mcpManager *mcpmanager.Manager, log *config.Logger) error {
	bootstrapLog := log.Server("tool.bootstrap")
	searchClient := websearch.NewClient(cfg.SearxngURL, log.Server("tool.web.search"))
	if err := reg.RegisterHandler("web.search", registry.Handler(websearch.NewHandler(searchClient, log))); err != nil {
		return fmt.Errorf("failed to initialize web.search tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured web search tool", config.F("tool_name", "web.search"), config.F("path", cfg.SearxngURL))

	if err := reg.RegisterHandler("memory.save", registry.Handler(usermemory.NewSaveHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.save tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.save"), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler("memory.search", registry.Handler(usermemory.NewSearchHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.search tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.search"), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler("memory.list", registry.Handler(usermemory.NewListHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.list tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.list"), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler("memory.forget", registry.Handler(usermemory.NewForgetHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.forget tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.forget"), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler("mcp.servers", registry.Handler(mcpbrowse.NewServersHandler(mcpManager, log))); err != nil {
		return fmt.Errorf("failed to initialize mcp.servers tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured MCP browse tool", config.F("tool_name", "mcp.servers"))

	if err := reg.RegisterHandler("mcp.tools", registry.Handler(mcpbrowse.NewToolsHandler(reg, mcpManager, log))); err != nil {
		return fmt.Errorf("failed to initialize mcp.tools tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured MCP browse tool", config.F("tool_name", "mcp.tools"))

	if err := reg.RegisterHandler("soul.read", registry.Handler(soul.NewReadHandler(soulStore, log))); err != nil {
		return fmt.Errorf("failed to initialize soul.read tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured soul tool", config.F("tool_name", "soul.read"), config.F("path", config.DefaultSoulPath))

	if err := reg.RegisterHandler("soul.patch", registry.Handler(soul.NewPatchHandler(soulStore, log))); err != nil {
		return fmt.Errorf("failed to initialize soul.patch tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured soul tool", config.F("tool_name", "soul.patch"), config.F("path", config.DefaultSoulPath))

	return nil
}
