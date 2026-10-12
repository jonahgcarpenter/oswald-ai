package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/process"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/image_generate/comfy_ui"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web/brave"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web/searxng"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	toolmemory "github.com/jonahgcarpenter/oswald-ai/internal/tools/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	sessionsearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/session_search"
	visionanalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/vision_analyze"
	websearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/web_search"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/workspace"
)

// registerHandlers wires configured builtin handlers and policies into the registry.
func registerHandlers(reg *registry.Registry, cfg *config.Config, fileStore *files.Store, profileStore *memory.ProfileStore, cache *imagecache.Cache, log *config.Logger, peerMaps ...map[string]*memory.ProfileStore) error {
	bootstrapLog := log.Server("tool.bootstrap")
	visionPolicy := governance.ToolPolicy{BlockDuplicates: true, MaxFailures: 2, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: false}}
	if err := reg.RegisterHandler(visionanalyze.Name, visionPolicy, registry.Handler(visionanalyze.NewHandler(cache))); err != nil {
		return fmt.Errorf("register vision_analyze tool: %w", err)
	}
	comfyURL := strings.TrimSpace(cfg.ComfyUIURL)
	if comfyURL == "" {
		if err := reg.DisableBuiltin(imagegenerate.Name); err != nil {
			return fmt.Errorf("failed to disable %s tool: %w", imagegenerate.Name, err)
		}
		bootstrapLog.Debug("tool.bootstrap.disabled", "disabled image generation because no server is configured", config.F("tool_name", imagegenerate.Name))
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
			History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: false},
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
		bootstrapLog.Debug("tool.bootstrap.disabled", "disabled web search because no search provider is configured", config.F("tool_name", websearch.Name))
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

	if profileStore != nil {
		sessionPolicy := governance.ToolPolicy{
			MaxUnproductive: 2,
			BlockDuplicates: true,
			NormalizeArgs:   normalizeSessionSearchArgs,
			History:         governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
		}
		if err := reg.RegisterHandler(sessionsearch.Name, sessionPolicy, registry.Handler(sessionsearch.NewHandler(profileStore, peerMaps...))); err != nil {
			return fmt.Errorf("register session_search tool: %w", err)
		}
	}

	ws := workspace.NewWorkspace(cfg.ProfileRoot, cfg.ProfileName)
	pm := process.NewManager(cfg.ProfileRoot)
	workspaceTools := []struct {
		name    string
		policy  governance.ToolPolicy
		handler registry.Handler
	}{
		{workspace.ReadFileName, governance.ToolPolicy{MaxUnproductive: 2, MaxFailures: 2, BlockDuplicates: true, NormalizeArgs: normalizeReadFileArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(workspace.NewReadFileHandler(ws))},
		{workspace.WriteFileName, governance.ToolPolicy{MaxFailures: 2, BlockDuplicates: true, NormalizeArgs: normalizeWriteFileArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(workspace.NewWriteFileHandler(ws))},
		{workspace.PatchName, governance.ToolPolicy{MaxFailures: 2, BlockDuplicates: true, NormalizeArgs: normalizePatchArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(workspace.NewPatchHandler(ws))},
		{workspace.SearchFilesName, governance.ToolPolicy{MaxUnproductive: 2, MaxFailures: 2, BlockDuplicates: true, NormalizeArgs: normalizeSearchFilesArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(workspace.NewSearchFilesHandler(ws))},
		{workspace.TerminalName, governance.ToolPolicy{MaxFailures: 2, BlockDuplicates: true, NormalizeArgs: normalizeTerminalArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(workspace.NewTerminalHandler(ws, pm))},
		{workspace.ExecuteCodeName, governance.ToolPolicy{MaxFailures: 2, BlockDuplicates: true, NormalizeArgs: normalizeExecuteCodeArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(workspace.NewExecuteCodeHandler(ws))},
		{workspace.ProcessManageName, governance.ToolPolicy{MaxUnproductive: 2, MaxFailures: 2, BlockDuplicates: true, NormalizeArgs: normalizeProcessManageArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(workspace.NewProcessManageHandler(ws, pm))},
	}
	for _, tool := range workspaceTools {
		if err := reg.RegisterHandler(tool.name, tool.policy, tool.handler); err != nil {
			return fmt.Errorf("register %s tool: %w", tool.name, err)
		}
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

// normalizeSessionSearchArgs canonicalizes shape-defining selectors so repeated
// identical reads collapse, while distinct queries remain distinct.
func normalizeSessionSearchArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"query", "session_id", "profile", "sort", "detail", "after", "before", "role_filter"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	for _, key := range []string{"limit", "around_message_id", "window"} {
		if value, ok := args[key]; ok {
			normalized[key] = value
		}
	}
	if raw, ok := args["exclude_session_ids"].([]interface{}); ok {
		normalized["exclude_session_ids_count"] = len(raw)
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

func normalizeReadFileArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{
		"path":   normalizedString(args, "path", false),
		"offset": args["offset"],
		"limit":  args["limit"],
	}
}

func normalizeWriteFileArgs(args map[string]interface{}) interface{} {
	content, _ := args["content"].(string)
	sum := sha256.Sum256([]byte(content))
	return map[string]interface{}{
		"path":         normalizedString(args, "path", false),
		"content_hash": hex.EncodeToString(sum[:]),
	}
}

func normalizePatchArgs(args map[string]interface{}) interface{} {
	oldString, _ := args["old_string"].(string)
	newString, _ := args["new_string"].(string)
	oldSum := sha256.Sum256([]byte(oldString))
	newSum := sha256.Sum256([]byte(newString))
	return map[string]interface{}{
		"path":        normalizedString(args, "path", false),
		"old_hash":    hex.EncodeToString(oldSum[:]),
		"new_hash":    hex.EncodeToString(newSum[:]),
		"replace_all": args["replace_all"],
	}
}

func normalizeSearchFilesArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"pattern", "target", "path", "file_glob", "order", "output_mode"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	for _, key := range []string{"limit", "offset", "context"} {
		if value, ok := args[key]; ok {
			normalized[key] = value
		}
	}
	return normalized
}

func normalizeTerminalArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{
		"command": normalizedString(args, "command", false),
		"workdir": normalizedString(args, "workdir", false),
		"timeout": args["timeout"],
	}
}

func normalizeExecuteCodeArgs(args map[string]interface{}) interface{} {
	code, _ := args["code"].(string)
	sum := sha256.Sum256([]byte(code))
	return map[string]interface{}{
		"code_hash": hex.EncodeToString(sum[:]),
		"reset":     args["reset"],
	}
}

func normalizeProcessManageArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["action"].(string); ok {
		normalized["action"] = strings.Join(strings.Fields(value), " ")
	}
	if value, ok := args["session_id"].(string); ok {
		normalized["session_id"] = strings.TrimSpace(value)
	}
	for _, key := range []string{"timeout", "offset", "limit"} {
		if value, ok := args[key]; ok {
			normalized[key] = value
		}
	}
	if data, ok := args["data"].(string); ok {
		sum := sha256.Sum256([]byte(data))
		normalized["data_hash"] = hex.EncodeToString(sum[:])
	}
	return normalized
}
