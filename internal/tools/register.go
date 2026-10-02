package tools

import (
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/image_generate/comfy_ui"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web/brave"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web/searxng"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	toolmemory "github.com/jonahgcarpenter/oswald-ai/internal/tools/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	websearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/web_search"
)

// registerHandlers wires configured builtin handlers and policies into the registry.
func registerHandlers(reg *registry.Registry, cfg *config.Config, fileStore *files.Store, cache *imagecache.Cache, log *config.Logger) error {
	bootstrapLog := log.Server("tool.bootstrap")
	comfyURL := strings.TrimSpace(cfg.ComfyUIURL)
	if comfyURL == "" {
		if err := reg.DisableBuiltin(imagegenerate.Name); err != nil {
			return fmt.Errorf("failed to disable %s tool: %w", imagegenerate.Name, err)
		}
		bootstrapLog.Info("tool.bootstrap.disabled", "disabled image generation because no server is configured", config.F("tool_name", imagegenerate.Name), config.F("status", "ok"))
	} else {
		textWorkflow, err := comfy_ui.NewWorkflow(comfy_ui.TextToImage)
		if err != nil {
			return err
		}
		imageWorkflow, err := comfy_ui.NewWorkflow(comfy_ui.ImageToImage)
		if err != nil {
			return err
		}
		client, err := comfy_ui.NewClient(comfyURL, cfg.ComfyUIGenerationTimeout)
		if err != nil {
			return fmt.Errorf("initialize ComfyUI client: %w", err)
		}
		policy := governance.ToolPolicy{
			BlockDuplicates: true, NormalizeArgs: normalizeImageGenerateArgs,
			History: governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false},
		}
		if err := reg.RegisterHandler(imagegenerate.Name, policy, registry.Handler(imagegenerate.NewHandler(textWorkflow, imageWorkflow, client, cache, log))); err != nil {
			return fmt.Errorf("initialize %s tool: %w", imagegenerate.Name, err)
		}
		bootstrapLog.Debug("tool.bootstrap.configured", "configured image generation tool", config.F("tool_name", imagegenerate.Name))
	}
	braveKey := strings.TrimSpace(cfg.BraveAPIKey)
	searxngURL := strings.TrimSpace(cfg.SearxngURL)
	var braveClient web.Searcher
	var searxngClient web.Searcher
	if braveKey != "" {
		client, err := brave.NewClient(braveKey, log.Server("tool.web.search"))
		if err != nil {
			return fmt.Errorf("failed to initialize Brave web_search client: %w", err)
		}
		braveClient = client
	}
	if searxngURL != "" {
		client, err := searxng.NewClient(searxngURL, log.Server("tool.web.search"))
		if err != nil {
			return fmt.Errorf("failed to initialize SearXNG web_search client: %w", err)
		}
		searxngClient = client
	}

	var searcher web.Searcher
	primary := ""
	fallback := ""
	switch {
	case braveClient != nil && searxngClient != nil:
		searcher = web.NewFallbackSearcher(braveClient, searxngClient, log)
		primary, fallback = "brave", "searxng"
	case braveClient != nil:
		searcher = braveClient
		primary = "brave"
	case searxngClient != nil:
		searcher = searxngClient
		primary = "searxng"
	default:
		if err := reg.DisableBuiltin(websearch.Name); err != nil {
			return fmt.Errorf("failed to disable %s tool: %w", websearch.Name, err)
		}
		bootstrapLog.Info("tool.bootstrap.disabled", "disabled web search because no search provider is configured", config.F("tool_name", websearch.Name), config.F("status", "ok"))
	}
	if searcher != nil {
		searchPolicy := toolPolicy(2, normalizeWebSearchArgs)
		searchPolicy.MaxFailures = 2
		if err := reg.RegisterHandler(websearch.Name, searchPolicy, registry.Handler(websearch.NewHandler(searcher, log))); err != nil {
			return fmt.Errorf("failed to initialize web_search tool: %w", err)
		}
		bootstrapLog.Debug("tool.bootstrap.configured", "configured web search tool", config.F("tool_name", websearch.Name), config.F("primary_provider", primary), config.F("fallback_provider", fallback))
	}

	if err := reg.RegisterHandler(toolmemory.Name, governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false}}, registry.Handler(toolmemory.NewHandler(fileStore))); err != nil {
		return fmt.Errorf("register memory tool: %w", err)
	}

	return nil
}

func toolPolicy(maxUnproductive int, normalize governance.ArgumentNormalizer) governance.ToolPolicy {
	return governance.ToolPolicy{
		MaxUnproductive: maxUnproductive,
		BlockDuplicates: true,
		NormalizeArgs:   normalize,
		History:         governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
	}
}

func normalizeWebSearchArgs(args map[string]interface{}) interface{} {
	limit, err := websearch.WebResultLimit(args)
	var normalized interface{} = limit
	if err != nil {
		normalized = map[string]interface{}{"invalid": args["limit"]}
	}
	return map[string]interface{}{"query": normalizedString(args, "query", true), "limit": normalized}
}

func normalizeImageGenerateArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{"prompt": normalizedString(args, "prompt", false), "aspect_ratio": "landscape"}
	if aspect, exists := args["aspect_ratio"]; exists {
		normalized["aspect_ratio"] = aspect
	}
	if source, exists := args["image_url"]; exists {
		normalized["image_url"] = source
	}
	return normalized
}

func normalizedString(args map[string]interface{}, key string, lower bool) string {
	value, _ := args[key].(string)
	value = strings.Join(strings.Fields(value), " ")
	if lower {
		value = strings.ToLower(value)
	}
	return value
}
