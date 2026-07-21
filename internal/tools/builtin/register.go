package builtin

import (
	"fmt"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/currenttime"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/globalmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Register wires all builtin tools into the shared registry.
func Register(reg *registry.Registry, cfg *config.Config, userMemStore *usermemory.Store, globalMemStore *globalmemory.Store, globalMemoryAuthorizer globalmemory.GlobalMemoryAuthorizer, log *config.Logger) error {
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

	if err := reg.RegisterHandler(toolnames.UserMemorySave, registry.Handler(usermemory.NewSaveHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.UserMemorySave, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured user memory tool", config.F("tool_name", toolnames.UserMemorySave), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler(toolnames.UserMemorySearch, registry.Handler(usermemory.NewSearchHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.UserMemorySearch, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured user memory tool", config.F("tool_name", toolnames.UserMemorySearch), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler(toolnames.UserMemoryList, registry.Handler(usermemory.NewListHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.UserMemoryList, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured user memory tool", config.F("tool_name", toolnames.UserMemoryList), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler(toolnames.UserMemoryForget, registry.Handler(usermemory.NewForgetHandler(userMemStore, cfg.RetentionPolicy, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.UserMemoryForget, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured user memory tool", config.F("tool_name", toolnames.UserMemoryForget), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler(toolnames.SessionTranscriptSearch, registry.Handler(usermemory.NewTranscriptSearchHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.SessionTranscriptSearch, err)
	}

	if err := reg.RegisterHandler(toolnames.GlobalMemorySave, registry.Handler(globalmemory.NewGlobalMemoryProposeHandler(globalMemStore, globalMemoryAuthorizer, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.GlobalMemorySave, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured session transcript tool", config.F("tool_name", toolnames.SessionTranscriptSearch))
	bootstrapLog.Debug("tool.bootstrap.configured", "configured global memory tool", config.F("tool_name", toolnames.GlobalMemorySave))

	return nil
}
