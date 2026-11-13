package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
	providerbrowser "github.com/jonahgcarpenter/oswald-ai/internal/providers/browser"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/image_generate/comfy_ui"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web/brave"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web/searxng"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/skills"
	toolbrowser "github.com/jonahgcarpenter/oswald-ai/internal/tools/browser"
	toolclarify "github.com/jonahgcarpenter/oswald-ai/internal/tools/clarify"
	toolcron "github.com/jonahgcarpenter/oswald-ai/internal/tools/cronjob"
	tooldelegate "github.com/jonahgcarpenter/oswald-ai/internal/tools/delegate"
	toolexec "github.com/jonahgcarpenter/oswald-ai/internal/tools/execute_code"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	toolmemory "github.com/jonahgcarpenter/oswald-ai/internal/tools/memory"
	toolpatch "github.com/jonahgcarpenter/oswald-ai/internal/tools/patch"
	toolprocess "github.com/jonahgcarpenter/oswald-ai/internal/tools/process_manage"
	toolread "github.com/jonahgcarpenter/oswald-ai/internal/tools/read_file"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	toolsearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/search_files"
	sessionsearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/session_search"
	toolskill "github.com/jonahgcarpenter/oswald-ai/internal/tools/skill_manage"
	toolterm "github.com/jonahgcarpenter/oswald-ai/internal/tools/terminal"
	visionanalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/vision_analyze"
	websearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/web_search"
	toolwrite "github.com/jonahgcarpenter/oswald-ai/internal/tools/write_file"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/docache"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/vault"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

// registerHandlers wires configured builtin handlers and policies into the registry.
func registerHandlers(reg *registry.Registry, cfg *config.Config, fileStore *files.Store, profileStore *memory.ProfileStore, cache *imagecache.Cache, log *config.Logger, peerMaps ...map[string]*memory.ProfileStore) error {
	bootstrapLog := log.Server("tool.bootstrap")
	ws := workspace.NewWorkspace(cfg.ProfileRoot, cfg.ProfileName)
	visionPolicy := governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}
	if err := reg.RegisterHandler(visionanalyze.Name, visionPolicy, registry.Handler(visionanalyze.NewHandlerWithWorkspace(cache, ws))); err != nil {
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
			NormalizeArgs: normalizeImageGenerateArgs,
			History:       governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
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
		searchPolicy := toolPolicy(normalizeWebSearchArgs)
		if err := reg.RegisterHandler(websearch.Name, searchPolicy, registry.Handler(websearch.NewHandler(searcher, log))); err != nil {
			return fmt.Errorf("failed to initialize web_search tool: %w", err)
		}
		bootstrapLog.Debug("tool.bootstrap.configured", "configured web search tool", config.F("tool_name", websearch.Name), config.F("primary_provider", primary), config.F("fallback_provider", fallback))
	}

	if err := reg.RegisterHandler(toolmemory.Name, governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolmemory.NewHandler(fileStore))); err != nil {
		return fmt.Errorf("register memory tool: %w", err)
	}

	if err := reg.RegisterHandler(toolskill.Name, governance.ToolPolicy{NormalizeArgs: normalizeSkillManageArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolskill.NewHandlerWithPlugins(cfg.ProfileRoot, PluginSkillRoots(cfg)))); err != nil {
		return fmt.Errorf("register skill_manage tool: %w", err)
	}

	if err := reg.RegisterHandler(toolclarify.Name, governance.ToolPolicy{NormalizeArgs: normalizeClarifyArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolclarify.NewHandler())); err != nil {
		return fmt.Errorf("register clarify tool: %w", err)
	}

	// The live delegate handler needs the per-profile delegation manager,
	// which startup installs after the registry exists; until then the tool
	// is visible but fails closed instead of dispatching half-wired.
	if err := reg.RegisterHandler(tooldelegate.Name, DelegatePolicy(), registry.Handler(tooldelegate.PlaceholderHandler())); err != nil {
		return fmt.Errorf("register delegate tool: %w", err)
	}

	if profileStore != nil {
		sessionPolicy := governance.ToolPolicy{
			NormalizeArgs: normalizeSessionSearchArgs,
			History:       governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
		}
		if err := reg.RegisterHandler(sessionsearch.Name, sessionPolicy, registry.Handler(sessionsearch.NewHandler(profileStore, peerMaps...))); err != nil {
			return fmt.Errorf("register session_search tool: %w", err)
		}
	}

	if err := registerBrowserTools(reg, cfg, cache, log); err != nil {
		return err
	}

	pm := process.NewManager(cfg.ProfileRoot)
	process.SetProfileEnvPassthrough(cfg.ProfileRoot, cfg.TerminalEnvPassthrough)
	cronStore := cron.NewStore(cfg.ProfileRoot)
	docCache := docache.NewProfileCache(cfg.ProfileRoot, cfg.ProfileName)
	workspaceTools := []struct {
		name    string
		policy  governance.ToolPolicy
		handler registry.Handler
	}{
		{toolread.Name, governance.ToolPolicy{NormalizeArgs: normalizeReadFileArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolread.NewReadFileHandlerWithDocCache(ws, docCache))},
		{toolwrite.Name, governance.ToolPolicy{NormalizeArgs: normalizeWriteFileArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolwrite.NewWriteFileHandler(ws))},
		{toolpatch.Name, governance.ToolPolicy{NormalizeArgs: normalizePatchArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolpatch.NewPatchHandler(ws))},
		{toolsearch.Name, governance.ToolPolicy{NormalizeArgs: normalizeSearchFilesArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolsearch.NewSearchFilesHandler(ws))},
		{toolterm.Name, governance.ToolPolicy{NormalizeArgs: normalizeTerminalArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolterm.NewTerminalHandler(ws, pm))},
		{toolexec.Name, governance.ToolPolicy{NormalizeArgs: normalizeExecuteCodeArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolexec.NewExecuteCodeHandler(ws))},
		{toolprocess.Name, governance.ToolPolicy{NormalizeArgs: normalizeProcessManageArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolprocess.NewProcessManageHandler(ws, pm))},
		{toolcron.Name, governance.ToolPolicy{NormalizeArgs: normalizeCronjobManageArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolcron.NewCronjobManageHandler(ws, cronStore))},
	}
	for _, tool := range workspaceTools {
		if err := reg.RegisterHandler(tool.name, tool.policy, tool.handler); err != nil {
			return fmt.Errorf("register %s tool: %w", tool.name, err)
		}
	}

	return nil
}

func toolPolicy(normalize governance.ArgumentNormalizer) governance.ToolPolicy {
	return governance.ToolPolicy{
		NormalizeArgs: normalize,
		History:       governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
	}
}

// DelegatePolicy returns the registry policy for the delegate tool: full
// history with searchable results. Startup reuses it when installing the
// live per-profile handler over the placeholder, so both registrations share
// one policy definition.
func DelegatePolicy() governance.ToolPolicy {
	return toolPolicy(normalizeDelegateArgs)
}

// PluginSkillRoots collects read-only bundled skills dirs for installed
// profile plugins. Presence means enabled. Missing skills dirs bind
// harmlessly: the scan reads them as empty. Shared with the agent prompt
// index so the tool catalog and the system prompt resolve the same visible set.
func PluginSkillRoots(cfg *config.Config) []skills.PluginRoot {
	if cfg == nil || strings.TrimSpace(cfg.ProfileRoot) == "" {
		return nil
	}
	manifests := plugins.ScanProfilePlugins(cfg.ProfileRoot)
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Key < manifests[j].Key })
	var out []skills.PluginRoot
	for _, m := range manifests {
		out = append(out, skills.PluginRoot{Key: m.Key, Dir: filepath.Join(plugins.PluginCodeDir(cfg.ProfileRoot, m.Key), "skills")})
	}
	return out
}

// registerBrowserTools wires the local headless browser_* and
// browser_vault_* tools. The tools stay reserved but disabled unless the
// operator configured an endpoint or launch command; Chromium itself is
// dialed lazily on first navigation. The vault rides with the browser.
// Startup sweeps orphaned session dirs (a fresh process owns no live
// session); shutdown and the hourly prune tick own the rest.
func registerBrowserTools(reg *registry.Registry, cfg *config.Config, cache *imagecache.Cache, log *config.Logger) error {
	names := append(toolbrowser.Names(), toolbrowser.VaultNames()...)
	configured := strings.TrimSpace(cfg.BrowserEndpointURL) != "" || strings.TrimSpace(cfg.BrowserLaunchCommand) != ""
	if !configured {
		for _, name := range names {
			if err := reg.DisableBuiltin(name); err != nil {
				return fmt.Errorf("failed to disable %s tool: %w", name, err)
			}
		}
		log.Server("tool.bootstrap").Debug("tool.bootstrap.disabled", "disabled browser tools because no browser backend is configured", config.F("tool_count", len(names)))
		return nil
	}
	reaped := providerbrowser.SweepStaleProfiles(cfg.ProfileRoot)
	if reaped > 0 {
		log.Server("tool.bootstrap").Debug("tool.bootstrap.browser_stale_reaped", "removed orphaned browser session dirs", config.F("reaped_dirs", reaped))
	}
	opts := providerbrowser.Options{
		LaunchCommand:     cfg.BrowserLaunchCommand,
		EndpointURL:       cfg.BrowserEndpointURL,
		CommandTimeout:    cfg.BrowserCommandTimeout,
		SnapshotThreshold: cfg.BrowserSnapshotThreshold,
		InactivityTimeout: cfg.BrowserInactivityTimeout,
		RecordSessions:    cfg.BrowserRecordSessions,
		AllowPrivateURLs:  cfg.BrowserAllowPrivateURLs,
		AllowInsecureURLs: cfg.BrowserAllowInsecureURLs,
		RestrictEvaluate:  cfg.BrowserRestrictEvaluate,
	}.Effective()
	backend := providerbrowser.NewLocalBackend(opts, cfg.ProfileRoot)
	handlers := toolbrowser.NewHandlers(toolbrowser.Deps{Backend: backend, Cache: cache, Vault: vault.NewStore(cfg.ProfileRoot), Log: log})
	policies := map[string]governance.ToolPolicy{
		toolbrowser.NavigateName:  toolPolicy(normalizeBrowserNavigateArgs),
		toolbrowser.SnapshotName:  toolPolicy(normalizeBrowserSnapshotArgs),
		toolbrowser.ClickName:     toolPolicy(normalizeBrowserRefArgs),
		toolbrowser.TypeName:      toolPolicy(normalizeBrowserTypeArgs),
		toolbrowser.ScrollName:    toolPolicy(normalizeBrowserScrollArgs),
		toolbrowser.BackName:      toolPolicy(normalizeBrowserEmptyArgs),
		toolbrowser.PressName:     toolPolicy(normalizeBrowserRefArgs),
		toolbrowser.GetImagesName: toolPolicy(normalizeBrowserEmptyArgs),
		toolbrowser.ConsoleName:   toolPolicy(normalizeBrowserConsoleArgs),
		toolbrowser.DialogName:    toolPolicy(normalizeBrowserDialogArgs),
		toolbrowser.VaultListName: {
			NormalizeArgs: normalizeBrowserEmptyArgs,
			History:       governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
		},
		toolbrowser.VaultFillName: {
			NormalizeArgs: normalizeVaultHandleArgs,
			History:       governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
		},
		toolbrowser.VaultSaveLoginName: {
			NormalizeArgs: normalizeVaultSaveArgs,
			History:       governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
		},
		toolbrowser.VaultSaveAddressName: {
			NormalizeArgs: normalizeVaultSaveAddressArgs,
			History:       governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
		},
		toolbrowser.VaultRemoveName: {
			NormalizeArgs: normalizeVaultHandleArgs,
			History:       governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
		},
		toolbrowser.VaultEnterCodeName: {
			NormalizeArgs: normalizeVaultHandleArgs,
			History:       governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
		},
		toolbrowser.VisionName: {
			NormalizeArgs: normalizeBrowserVisionArgs,
			History:       governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
		},
	}
	for _, name := range names {
		policy, ok := policies[name]
		if !ok {
			return fmt.Errorf("missing browser policy for %s", name)
		}
		handler, ok := handlers[name]
		if !ok {
			return fmt.Errorf("missing browser handler for %s", name)
		}
		if err := reg.RegisterHandler(name, policy, registry.Handler(handler)); err != nil {
			return fmt.Errorf("register %s tool: %w", name, err)
		}
	}
	log.Server("tool.bootstrap").Debug("tool.bootstrap.configured", "configured browser tools", config.F("tool_count", len(names)))
	reg.OnClose(backend.CloseAll)
	reg.OnSweep(func(ctx context.Context, now time.Time) (prune.Counts, error) {
		return prune.Counts{RemovedDirs: backend.Sweep(now)}, nil
	})
	return nil
}

func normalizeBrowserNavigateArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{"url": normalizedString(args, "url", false)}
}

func normalizeBrowserSnapshotArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{"full": args["full"]}
}

func normalizeBrowserRefArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"ref", "key"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	return normalized
}

func normalizeBrowserTypeArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["ref"].(string); ok {
		normalized["ref"] = strings.Join(strings.Fields(value), " ")
	}
	if text, ok := args["text"].(string); ok {
		sum := sha256.Sum256([]byte(text))
		normalized["text_hash"] = hex.EncodeToString(sum[:])
	}
	return normalized
}

func normalizeBrowserScrollArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["direction"].(string); ok {
		normalized["direction"] = strings.Join(strings.Fields(value), " ")
	}
	return normalized
}

func normalizeBrowserEmptyArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{}
}

func normalizeBrowserConsoleArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{"clear": args["clear"]}
	if expression, ok := args["expression"].(string); ok {
		sum := sha256.Sum256([]byte(expression))
		normalized["expression_hash"] = hex.EncodeToString(sum[:])
	}
	return normalized
}

func normalizeVaultHandleArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["handle"].(string); ok {
		normalized["handle"] = strings.Join(strings.Fields(value), " ")
	}
	return normalized
}

func normalizeVaultSaveArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"label", "identifier", "identifier_type"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	for _, key := range []string{"password", "otp"} {
		if value, ok := args[key].(string); ok {
			sum := sha256.Sum256([]byte(value))
			normalized[key+"_hash"] = hex.EncodeToString(sum[:])
		}
	}
	return normalized
}

func normalizeVaultSaveAddressArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["label"].(string); ok {
		normalized["label"] = strings.Join(strings.Fields(value), " ")
	}
	for _, key := range []string{"address_line1", "address_line2", "city", "state", "postal_code", "country"} {
		if value, ok := args[key].(string); ok {
			sum := sha256.Sum256([]byte(value))
			normalized[key+"_hash"] = hex.EncodeToString(sum[:])
		}
	}
	return normalized
}

func normalizeBrowserDialogArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["action"].(string); ok {
		normalized["action"] = strings.Join(strings.Fields(value), " ")
	}
	if text, ok := args["prompt_text"].(string); ok {
		sum := sha256.Sum256([]byte(text))
		normalized["prompt_text_hash"] = hex.EncodeToString(sum[:])
	}
	if value, ok := args["dialog_id"].(string); ok {
		normalized["dialog_id"] = strings.Join(strings.Fields(value), " ")
	}
	return normalized
}

func normalizeBrowserVisionArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{"annotate": args["annotate"]}
	if question, ok := args["question"].(string); ok {
		sum := sha256.Sum256([]byte(question))
		normalized["question_hash"] = hex.EncodeToString(sum[:])
	}
	return normalized
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
	for _, key := range []string{"query", "session_id", "sort", "detail", "after", "before", "role_filter"} {
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

// normalizeSkillManageArgs canonicalizes skill selectors so repeated
// identical reads collapse while distinct skills stay distinct. Bodies and
// descriptions never survive normalization, only names and hashes.
func normalizeSkillManageArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"action", "name", "file_path"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	for _, key := range []string{"description", "content", "old_text"} {
		if value, ok := args[key].(string); ok {
			sum := sha256.Sum256([]byte(value))
			normalized[key+"_hash"] = hex.EncodeToString(sum[:])
		}
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

// normalizeDelegateArgs canonicalizes delegate calls to counts and labels so
// repeated identical dispatches collapse. Prompts, contexts, messages, and
// schemas never survive normalization — only counts and names.
func normalizeDelegateArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	if value, ok := args["action"].(string); ok {
		normalized["action"] = strings.Join(strings.Fields(value), " ")
	}
	if value, ok := args["role"].(string); ok {
		normalized["role"] = strings.Join(strings.Fields(value), " ")
	}
	if value, ok := args["subagent_id"].(string); ok {
		normalized["subagent_id"] = strings.TrimSpace(value)
	}
	if tasks, ok := args["tasks"].([]interface{}); ok {
		normalized["task_count"] = len(tasks)
		imageCount := 0
		hasSchema := false
		for _, raw := range tasks {
			entry, _ := raw.(map[string]interface{})
			if entry == nil {
				continue
			}
			if images, ok := entry["images"].([]interface{}); ok {
				imageCount += len(images)
			}
			if _, ok := entry["output_schema"]; ok {
				hasSchema = true
			}
		}
		normalized["image_count"] = imageCount
		normalized["has_output_schema"] = hasSchema
	} else {
		if _, ok := args["goal"]; ok {
			normalized["task_count"] = 1
		}
		if images, ok := args["images"].([]interface{}); ok {
			normalized["image_count"] = len(images)
		}
		if _, ok := args["output_schema"]; ok {
			normalized["has_output_schema"] = true
		}
	}
	return normalized
}

func normalizeCronjobManageArgs(args map[string]interface{}) interface{} {
	normalized := map[string]interface{}{}
	for _, key := range []string{"action", "job_id", "schedule", "script", "deliver"} {
		if value, ok := args[key].(string); ok {
			normalized[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	if prompt, ok := args["prompt"].(string); ok {
		sum := sha256.Sum256([]byte(prompt))
		normalized["prompt_hash"] = hex.EncodeToString(sum[:])
	}
	return normalized
}

// normalizeClarifyArgs canonicalizes the question form so repeated identical
// asks collapse while distinct forms stay distinct. Raw question text never
// survives normalization, only counts and a hash.
func normalizeClarifyArgs(args map[string]interface{}) interface{} {
	questions, _ := args["questions"].([]interface{})
	normalized := map[string]interface{}{"question_count": len(questions)}
	choiceCount := 0
	var sb strings.Builder
	for _, raw := range questions {
		entry, _ := raw.(map[string]interface{})
		if entry == nil {
			continue
		}
		if text, ok := entry["question"].(string); ok {
			sb.WriteString(strings.Join(strings.Fields(text), " "))
			sb.WriteString("\x00")
		}
		if multi, ok := entry["multi_select"].(bool); ok && multi {
			sb.WriteString("multi\x00")
		}
		if rawChoices, ok := entry["choices"].([]interface{}); ok {
			for _, rawChoice := range rawChoices {
				if choice, ok := rawChoice.(string); ok {
					choiceCount++
					sb.WriteString(strings.Join(strings.Fields(choice), " "))
					sb.WriteString("\x00")
				}
			}
		}
	}
	normalized["choice_count"] = choiceCount
	sum := sha256.Sum256([]byte(sb.String()))
	normalized["form_hash"] = hex.EncodeToString(sum[:])
	return normalized
}
