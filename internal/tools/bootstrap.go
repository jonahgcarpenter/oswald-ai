package tools

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/files"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	toolmemory "github.com/jonahgcarpenter/oswald-ai/internal/tools/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	websearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/web_search"
)

// NewRegistryFromConfig creates a Registry with Go definitions and configured builtin handlers.
// The retained SQLite memory store remains a startup dependency but is not used by builtin tools.
func NewRegistryFromConfig(cfg *config.Config, userMemStore *memory.Store, fileStore *files.Store, log *config.Logger) (*registry.Registry, error) {
	return NewRegistryWithImageCache(cfg, userMemStore, fileStore, imagecache.New(config.DefaultDataRoot), log)
}

// NewRegistryWithImageCache wires tools to the cache used by the agent for this process.
func NewRegistryWithImageCache(cfg *config.Config, userMemStore *memory.Store, fileStore *files.Store, cache *imagecache.Cache, log *config.Logger) (*registry.Registry, error) {
	_ = userMemStore
	bootstrapLog := log.Server("tool.bootstrap")
	reg := registry.New(log.Server("tool.registry"))
	for _, def := range []llm.ToolDefinition{toolmemory.Definition(), websearch.Definition(), imagegenerate.Definition()} {
		if err := reg.RegisterDefinition(def); err != nil {
			return nil, err
		}
	}

	if err := registerHandlers(reg, cfg, fileStore, cache, log); err != nil {
		return nil, err
	}

	enabled := reg.EnabledBuiltinNames()
	bootstrapLog.Info("tool.bootstrap.enabled", "enabled tools", config.F("tool_count", len(enabled)), config.F("tools", strings.Join(enabled, ",")))
	return reg, nil
}
