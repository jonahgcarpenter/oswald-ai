package builtin

import (
	"fmt"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/currenttime"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Register wires all builtin tools into the shared registry.
func Register(reg *registry.Registry, cfg *config.Config, soulStore *soul.Store, userMemStore *usermemory.Store, soulAuthorizer soul.Authorizer, chatClient llm.Chatter, model string, log *config.Logger) error {
	bootstrapLog := log.Server("tool.bootstrap")
	searchClient := websearch.NewClient(cfg.SearxngURL, log.Server("tool.web.search"))
	if err := reg.RegisterHandler("web.search", registry.Handler(websearch.NewHandler(searchClient, log))); err != nil {
		return fmt.Errorf("failed to initialize web.search tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured web search tool", config.F("tool_name", "web.search"), config.F("path", cfg.SearxngURL))

	if err := reg.RegisterHandler("time.current", registry.Handler(currenttime.NewHandler(time.Now))); err != nil {
		return fmt.Errorf("failed to initialize time.current tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured current time tool", config.F("tool_name", "time.current"))

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

	if err := reg.RegisterHandler("memory.forget", registry.Handler(usermemory.NewForgetHandler(userMemStore, cfg.RetentionPolicy, log))); err != nil {
		return fmt.Errorf("failed to initialize memory.forget tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured memory tool", config.F("tool_name", "memory.forget"), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler("transcript.search", registry.Handler(usermemory.NewTranscriptSearchHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize transcript.search tool: %w", err)
	}

	if err := reg.RegisterHandler("deployment_memory.propose", registry.Handler(usermemory.NewDeploymentMemoryProposeHandler(userMemStore, soulAuthorizer, log))); err != nil {
		return fmt.Errorf("failed to initialize deployment_memory.propose tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured transcript tool", config.F("tool_name", "transcript.search"))

	if err := reg.RegisterHandler("soul.read", registry.Handler(soul.NewReadHandler(soulStore, log))); err != nil {
		return fmt.Errorf("failed to initialize soul.read tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured soul tool", config.F("tool_name", "soul.read"), config.F("path", config.DefaultSoulPath))

	if err := reg.RegisterHandler("soul.patch", registry.Handler(soul.NewPatchHandler(soulStore, soulAuthorizer, log))); err != nil {
		return fmt.Errorf("failed to initialize soul.patch tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured soul tool", config.F("tool_name", "soul.patch"), config.F("path", config.DefaultSoulPath))

	return nil
}
