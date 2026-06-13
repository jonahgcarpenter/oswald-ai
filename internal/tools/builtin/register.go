package builtin

import (
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/sessionhistory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Register wires all builtin tools into the shared registry.
func Register(reg *registry.Registry, cfg *config.Config, soulStore *soul.Store, userMemStore *usermemory.Store, sessionStore *memory.Store, chatClient llm.Chatter, model string, log *config.Logger) error {
	bootstrapLog := log.Server("tool.bootstrap")
	searchClient := websearch.NewClient(cfg.SearxngURL, log.Server("tool.web.search"))
	if err := reg.RegisterHandler("web.search", registry.Handler(websearch.NewHandler(searchClient, log))); err != nil {
		return fmt.Errorf("failed to initialize web.search tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured web search tool", config.F("tool_name", "web.search"), config.F("path", cfg.SearxngURL))

	if err := reg.RegisterHandler("memory.remember", registry.Handler(usermemory.NewRememberHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.remember tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.remember"), config.F("path", config.DefaultUserMemoryPath))

	if err := reg.RegisterHandler("memory.recall", registry.Handler(usermemory.NewRecallHandler(userMemStore, chatClient, model, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.recall tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.recall"), config.F("path", config.DefaultUserMemoryPath))

	if err := reg.RegisterHandler("memory.forget", registry.Handler(usermemory.NewForgetHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.forget tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.forget"), config.F("path", config.DefaultUserMemoryPath))

	if err := reg.RegisterHandler("session.recent", registry.Handler(sessionhistory.NewRecentHandler(sessionStore, log))); err != nil {
		return fmt.Errorf("failed to initialize session.recent tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured session history tool", config.F("tool_name", "session.recent"))

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
