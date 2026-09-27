package builtin

import (
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/comfyui"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/filememory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Register wires all builtin tools into the shared registry.
func Register(reg *registry.Registry, cfg *config.Config, userMemStore *memory.Store, fileStore *files.Store, log *config.Logger) error {
	_ = userMemStore
	bootstrapLog := log.Server("tool.bootstrap")
	comfyURL := strings.TrimSpace(cfg.ComfyUIURL)
	if comfyURL == "" {
		for _, name := range []string{toolnames.ComfyUIImageToImage, toolnames.ComfyUITextToImage} {
			if err := reg.DisableBuiltin(name); err != nil {
				return fmt.Errorf("failed to disable %s tool: %w", name, err)
			}
		}
		bootstrapLog.Info("tool.bootstrap.disabled", "disabled ComfyUI tools because no server is configured", config.F("tool_name", toolnames.ComfyUITextToImage+","+toolnames.ComfyUIImageToImage), config.F("status", "ok"))
	} else {
		textWorkflow, err := comfyui.LoadWorkflow(cfg.ComfyUITextToImageWorkflowPath, comfyui.TextToImage)
		if err != nil {
			return err
		}
		imageWorkflow, err := comfyui.LoadWorkflow(cfg.ComfyUIImageToImageWorkflowPath, comfyui.ImageToImage)
		if err != nil {
			return err
		}
		client, err := comfyui.NewClient(comfyURL, cfg.ComfyUIGenerationTimeout)
		if err != nil {
			return fmt.Errorf("initialize ComfyUI client: %w", err)
		}
		policy := governance.ToolPolicy{
			BlockDuplicates: true, NormalizeArgs: normalizeComfyArgs,
			History: governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false},
		}
		if err := reg.RegisterHandler(toolnames.ComfyUITextToImage, policy, registry.Handler(comfyui.NewHandler(comfyui.TextToImage, textWorkflow, client, log))); err != nil {
			return fmt.Errorf("initialize %s tool: %w", toolnames.ComfyUITextToImage, err)
		}
		policy.NormalizeArgs = normalizeComfyImageArgs
		if err := reg.RegisterHandler(toolnames.ComfyUIImageToImage, policy, registry.Handler(comfyui.NewHandler(comfyui.ImageToImage, imageWorkflow, client, log))); err != nil {
			return fmt.Errorf("initialize %s tool: %w", toolnames.ComfyUIImageToImage, err)
		}
		bootstrapLog.Debug("tool.bootstrap.configured", "configured ComfyUI image tools", config.F("tool_name", toolnames.ComfyUITextToImage+","+toolnames.ComfyUIImageToImage))
	}
	braveKey := strings.TrimSpace(cfg.BraveAPIKey)
	searxngURL := strings.TrimSpace(cfg.SearxngURL)
	var braveClient websearch.Searcher
	var searxngClient websearch.Searcher
	if braveKey != "" {
		client, err := websearch.NewBraveClient(braveKey, log.Server("tool.web.search"))
		if err != nil {
			return fmt.Errorf("failed to initialize Brave web_search client: %w", err)
		}
		braveClient = client
	}
	if searxngURL != "" {
		client, err := websearch.NewSearxngClient(searxngURL, log.Server("tool.web.search"))
		if err != nil {
			return fmt.Errorf("failed to initialize SearXNG web_search client: %w", err)
		}
		searxngClient = client
	}

	var searcher websearch.Searcher
	primary := ""
	fallback := ""
	switch {
	case braveClient != nil && searxngClient != nil:
		searcher = websearch.NewFallbackSearcher(braveClient, searxngClient, log)
		primary, fallback = "brave", "searxng"
	case braveClient != nil:
		searcher = braveClient
		primary = "brave"
	case searxngClient != nil:
		searcher = searxngClient
		primary = "searxng"
	default:
		if err := reg.DisableBuiltin(toolnames.WebSearch); err != nil {
			return fmt.Errorf("failed to disable %s tool: %w", toolnames.WebSearch, err)
		}
		bootstrapLog.Info("tool.bootstrap.disabled", "disabled web search because no search provider is configured", config.F("tool_name", toolnames.WebSearch), config.F("status", "ok"))
	}
	if searcher != nil {
		searchPolicy := toolPolicy(2, normalizeWebSearchArgs)
		searchPolicy.MaxFailures = 2
		if err := reg.RegisterHandler(toolnames.WebSearch, searchPolicy, registry.Handler(websearch.NewHandler(searcher, log))); err != nil {
			return fmt.Errorf("failed to initialize web_search tool: %w", err)
		}
		bootstrapLog.Debug("tool.bootstrap.configured", "configured web search tool", config.F("tool_name", toolnames.WebSearch), config.F("primary_provider", primary), config.F("fallback_provider", fallback))
	}

	if err := reg.DisableBuiltin(toolnames.SessionTranscriptSearch); err != nil {
		return err
	}
	if err := reg.RegisterHandler(toolnames.Memory, governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false}}, registry.Handler(filememory.NewHandler(fileStore))); err != nil {
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

func normalizeComfyArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{
		"prompt":          normalizedString(args, "prompt", false),
		"negative_prompt": normalizedString(args, "negative_prompt", false),
	}
}

func normalizeComfyImageArgs(args map[string]interface{}) interface{} {
	normalized := normalizeComfyArgs(args).(map[string]interface{})
	normalized["create_variant"] = false
	if variant, exists := args["create_variant"]; exists {
		normalized["create_variant"] = variant
	}
	if strength, exists := args["strength"]; exists {
		normalized["strength"] = strength
	}
	if source, exists := args["source_image_id"]; exists {
		// Source IDs are exact-match selectors; preserve invalid values for validation.
		normalized["source_image_id"] = source
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
