package tools

import (
	"context"
	"fmt"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
	providerbrowser "github.com/jonahgcarpenter/oswald-ai/internal/providers/browser"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/image_generate"
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
	videonalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/video_analyze"
	visionanalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/vision_analyze"
	websearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/web_search"
	toolwrite "github.com/jonahgcarpenter/oswald-ai/internal/tools/write_file"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/docache"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/vault"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// registerHandlers wires configured builtin handlers and policies into the registry.

func registerHandlers(reg *registry.Registry, cfg *config.Config, fileStore *files.Store, profileStore *memory.ProfileStore, cache *imagecache.Cache, log *config.Logger, peerMaps ...map[string]*memory.ProfileStore) error {
	bootstrapLog := log.Server("tool.bootstrap")
	ws := workspace.NewWorkspace(cfg.ProfileRoot, cfg.ProfileName)
	visionPolicy := governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}
	if err := reg.RegisterHandler(visionanalyze.Name, visionPolicy, registry.Handler(visionanalyze.NewHandlerWithWorkspace(cache, ws))); err != nil {
		return fmt.Errorf("register vision_analyze tool: %w", err)
	}
	if ffmpeg, ffprobe := lookupVideoBinaries(); ffmpeg == "" || ffprobe == "" {
		if err := reg.DisableBuiltin(videonalyze.Name); err != nil {
			return fmt.Errorf("failed to disable %s tool: %w", videonalyze.Name, err)
		}
		bootstrapLog.Debug("tool.bootstrap.disabled", "disabled video analysis because ffmpeg is unavailable", config.F("tool_name", videonalyze.Name))
	} else if err := reg.RegisterHandler(videonalyze.Name, visionPolicy, registry.Handler(videonalyze.NewHandlerWithWorkspace(docache.NewProfileMediaCache(cfg.ProfileRoot, cfg.ProfileName), cache, ws, ffmpeg, ffprobe))); err != nil {
		return fmt.Errorf("register video_analyze tool: %w", err)
	}
	comfyURL := strings.TrimSpace(cfg.ComfyUIURL)
	if comfyURL == "" {
		if err := reg.DisableBuiltin(imagegenerate.Name); err != nil {
			return fmt.Errorf("failed to disable %s tool: %w", imagegenerate.Name, err)
		}
		bootstrapLog.Debug("tool.bootstrap.disabled", "disabled image generation because no server is configured", config.F("tool_name", imagegenerate.Name))
	} else {
		textWorkflow, err := image_generate.NewWorkflow(image_generate.TextToImage)
		if err != nil {
			return err
		}
		imageWorkflow, err := image_generate.NewWorkflow(image_generate.ImageToImage)
		if err != nil {
			return err
		}
		client, err := image_generate.NewClient(comfyURL, cfg.ComfyUIGenerationTimeout)
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
	cronValidators := cronTargetValidators(cfg, log)
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
		{toolcron.Name, governance.ToolPolicy{NormalizeArgs: normalizeCronjobManageArgs, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}, registry.Handler(toolcron.NewCronjobManageHandler(ws, cronStore, cronValidators, cronModelValidatorFor(cfg, log)))},
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

// cronTargetValidators builds per-platform deliver-target liveness checks
// from the deployment config. A platform without a configured backend gets
// no entry, and create/update then rejects its targets as unservable rather
// than scheduling jobs that can never deliver.

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
