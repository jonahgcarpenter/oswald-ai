package tools

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/globalmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// NewRegistryFromConfig creates a Registry, loads tool definitions, and wires builtin tools.
// The user memory store is created externally and shared with the tool handlers.
func NewRegistryFromConfig(cfg *config.Config, userMemStore *usermemory.Store, globalMemStore *globalmemory.Store, globalMemoryAuthorizer globalmemory.GlobalMemoryAuthorizer, log *config.Logger) (*registry.Registry, error) {
	bootstrapLog := log.Server("tool.bootstrap")
	reg, err := registry.NewFromDirectory(config.DefaultToolsConfigDir, log.Server("tool.registry"))
	if err != nil {
		return nil, err
	}

	if err := builtin.Register(reg, cfg, userMemStore, globalMemStore, globalMemoryAuthorizer, log); err != nil {
		return nil, err
	}

	bootstrapLog.Info("tool.bootstrap.enabled", "enabled tools", config.F("tool_count", reg.Count()), config.F("tools", strings.Join(reg.Names(), ",")))
	return reg, nil
}
