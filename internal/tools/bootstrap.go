package tools

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory/files"
	toolbrowser "github.com/jonahgcarpenter/oswald-ai/internal/tools/browser"
	toolclarify "github.com/jonahgcarpenter/oswald-ai/internal/tools/clarify"
	tooldelegate "github.com/jonahgcarpenter/oswald-ai/internal/tools/delegate"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	toolmemory "github.com/jonahgcarpenter/oswald-ai/internal/tools/memory"
	toolprocess "github.com/jonahgcarpenter/oswald-ai/internal/tools/process_manage"
	toolread "github.com/jonahgcarpenter/oswald-ai/internal/tools/read_file"
toolexec "github.com/jonahgcarpenter/oswald-ai/internal/tools/execute_code"
toolterm "github.com/jonahgcarpenter/oswald-ai/internal/tools/terminal"
toolwrite "github.com/jonahgcarpenter/oswald-ai/internal/tools/write_file"
toolpatch "github.com/jonahgcarpenter/oswald-ai/internal/tools/patch"
toolsearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/search_files"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	sessionsearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/session_search"
	toolskill "github.com/jonahgcarpenter/oswald-ai/internal/tools/skill_manage"
	visionanalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/vision_analyze"
	websearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/web_search"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

// BuiltinDefinitions returns every Go-owned builtin tool definition in
// registration order. Workspace-owned surfaces are listed explicitly so
// each tool keeps its original position as surfaces move to tools/*.
func BuiltinDefinitions() []llm.ToolDefinition {
	defs := []llm.ToolDefinition{toolmemory.Definition(), toolskill.Definition(), sessionsearch.Definition(), websearch.Definition(), imagegenerate.Definition(), visionanalyze.Definition(), toolclarify.Definition(), tooldelegate.Definition()}
	defs = append(append(append(defs, toolbrowser.Definitions()...), toolbrowser.VaultDefinitions()...),
		toolread.Definition(),
		toolwrite.Definition(),
		toolpatch.Definition(),
		toolsearch.Definition(),
		toolterm.Definition(),
		toolexec.Definition(),
		toolprocess.Definition(),
		workspace.CronjobManageDefinition(),
	)
	return defs
}

// NewRegistryFromConfig creates a Registry with Go definitions and configured
// builtin handlers, using a nil profile store for stateless test registries.
func NewRegistryFromConfig(cfg *config.Config, fileStore *files.Store, profileStore *memory.ProfileStore, log *config.Logger, peerMaps ...map[string]*memory.ProfileStore) (*registry.Registry, error) {
	return NewRegistryWithImageCache(cfg, fileStore, profileStore, imagecache.NewProfileCache(cfg.ProfileRoot, cfg.ProfileName), log, peerMaps...)
}

// NewRegistryWithImageCache wires tools to the cache and profile store used by
// the agent for this process.
func NewRegistryWithImageCache(cfg *config.Config, fileStore *files.Store, profileStore *memory.ProfileStore, cache *imagecache.Cache, log *config.Logger, peerMaps ...map[string]*memory.ProfileStore) (*registry.Registry, error) {
	bootstrapLog := log.Server("tool.bootstrap")
	reg := registry.New(log.Server("tool.registry"))
	for _, def := range BuiltinDefinitions() {
		if err := reg.RegisterDefinition(def); err != nil {
			return nil, err
		}
	}

	if err := registerHandlers(reg, cfg, fileStore, profileStore, cache, log, peerMaps...); err != nil {
		return nil, err
	}
	if profileStore == nil {
		if err := reg.DisableBuiltin(sessionsearch.Name); err != nil {
			return nil, err
		}
	}

	enabled := reg.EnabledBuiltinNames()
	bootstrapLog.Debug("tool.bootstrap.enabled", "enabled tools", config.F("tool_count", len(enabled)), config.F("tools", strings.Join(enabled, ",")))
	return reg, nil
}
