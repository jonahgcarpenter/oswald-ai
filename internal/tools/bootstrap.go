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
	sessionsearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/session_search"
	websearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/web_search"
)

// BuiltinDefinitions returns every Go-owned builtin tool definition in
// registration order.
func BuiltinDefinitions() []llm.ToolDefinition {
	return []llm.ToolDefinition{toolmemory.Definition(), sessionsearch.Definition(), websearch.Definition(), imagegenerate.Definition()}
}

// NewRegistryFromConfig creates a Registry with Go definitions and configured
// builtin handlers, using a nil profile store for stateless test registries.
func NewRegistryFromConfig(cfg *config.Config, fileStore *files.Store, profileStore *memory.ProfileStore, log *config.Logger) (*registry.Registry, error) {
	return NewRegistryWithImageCache(cfg, fileStore, profileStore, imagecache.NewProfileCache(cfg.ProfileRoot, cfg.ProfileName), log)
}

// NewRegistryWithImageCache wires tools to the cache and profile store used by
// the agent for this process.
func NewRegistryWithImageCache(cfg *config.Config, fileStore *files.Store, profileStore *memory.ProfileStore, cache *imagecache.Cache, log *config.Logger) (*registry.Registry, error) {
	bootstrapLog := log.Server("tool.bootstrap")
	reg := registry.New(log.Server("tool.registry"))
	for _, def := range BuiltinDefinitions() {
		if err := reg.RegisterDefinition(def); err != nil {
			return nil, err
		}
	}

	if err := registerHandlers(reg, cfg, fileStore, profileStore, cache, log); err != nil {
		return nil, err
	}
	if profileStore == nil {
		if err := reg.DisableBuiltin(sessionsearch.Name); err != nil {
			return nil, err
		}
	}

	enabled := reg.EnabledBuiltinNames()
	bootstrapLog.Info("tool.bootstrap.enabled", "enabled tools", config.F("tool_count", len(enabled)), config.F("tools", strings.Join(enabled, ",")))
	return reg, nil
}
