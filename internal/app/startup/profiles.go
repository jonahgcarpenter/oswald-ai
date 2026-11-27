package startup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	commandbuiltin "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/builtin"
	modelcommands "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/model"
	sessioncommands "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/session"
	stopcommands "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/stop"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	managerdelegate "github.com/jonahgcarpenter/oswald-ai/internal/delegate"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/profiles"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/chatgpt"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/compaction"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	tooldelegate "github.com/jonahgcarpenter/oswald-ai/internal/tools/delegate"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/docache"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/vault"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

type profileProcessor struct{ agents map[string]*agent.Agent }

func (p *profileProcessor) Process(ctx context.Context, request agent.Request) (*agent.Response, error) {
	engine := p.agents[request.Principal.CanonicalUserID]
	if engine == nil || !request.Principal.Authenticated() {
		return nil, errors.New("profile processor is unavailable")
	}
	return engine.Process(ctx, request)
}

type profileDependencies struct {
	newClient   func(*config.Config, *config.Logger) llm.Chatter
	newGateways func(*config.Config, identity.Resolver, gatewayruntime.Dependencies, *config.Logger) ([]gateway.Service, error)
	// drainSignals injects shutdown signals for the restart-drain
	// controller in tests. Nil subscribes to the real process signals.
	drainSignals <-chan os.Signal
}

// profileRuntime retains the per-profile pieces the delegation manager needs
// after the registry exists: the broker only exists once every profile's
// agent is built, so manager construction waits for the second pass.
type profileRuntime struct {
	registry *registry.Registry
	mcp      *mcp.Provider
	soul     *soul.Store
	client   llm.Chatter
	cache    *imagecache.Cache
	media    *docache.Cache
	// router owns this profile's per-turn model resolution. Kept here so later
	// phases (command service, delegate hot view) share the live routing state
	// the agent resolves against.
	router *agent.ModelRouter
}

// newSessionCanceller ends the open session, then interrupts the profile's
// live delegated children. Their partial results are dropped rather than
// delivered: the session they belonged to is gone.
type newSessionCanceller struct {
	sessioncommands.Starter
	profile  string
	delegate *managerdelegate.Manager
}

// NewSessionContext implements sessioncommands.Starter.
func (s *newSessionCanceller) NewSessionContext(ctx context.Context, owner, key string) error {
	if err := s.Starter.NewSessionContext(ctx, owner, key); err != nil {
		return err
	}
	if s.delegate != nil {
		s.delegate.CancelSession(s.profile, key)
	}
	return nil
}

// stopWithDelegate stops the active parent turn, then interrupts the
// profile's live delegated children. Killed units record honestly but their
// results are dropped: a stopped conversation spawns no follow-up turns.
type stopWithDelegate struct {
	broker   *broker.Broker
	profile  string
	delegate sessionChildCanceller
}

// sessionChildCanceller interrupts live delegated children owned by one
// session. *managerdelegate.Manager satisfies it; tests substitute fakes.
type sessionChildCanceller interface {
	CancelSession(profile, sessionKey string) int
}

// CancelActiveAgentWork implements stopcommands.Canceler.
func (s *stopWithDelegate) CancelActiveAgentWork(canonicalUserID, sessionID string) broker.CancelReport {
	report := s.broker.CancelActiveAgentWork(canonicalUserID, sessionID)
	if s.delegate != nil {
		s.delegate.CancelSession(s.profile, sessionID)
	}
	return report
}

// brokerWorkerCount bounds concurrent agent turns. Lanes stay strictly FIFO
// per user/session; workers only let unrelated lanes overlap. The count is a
// RAM knob, not a throughput knob: worst-case memory is workers times the
// per-turn footprint plus the bounded queue, and past-capacity submissions
// shed with ErrQueueFull instead of growing memory. Model-side queuing stays
// with the vLLM backend.
const brokerWorkerCount = 5

func runProfiles(ctx context.Context, cfg *config.Config, rootLog *config.Logger) error {
	return runProfilesWith(ctx, cfg, rootLog, profileDependencies{newClient: func(cfg *config.Config, log *config.Logger) llm.Chatter {
		return defaultSubscriptionClient(cfg, log)
	}, newGateways: gateway.NewServicesFromConfig})
}

// cronGateway exposes scheduler delivery for one gateway.
type cronGateway interface {
	CronSender() cron.Sender
	CronGatewayName() string
}

// wireDelegateSenders installs live gateway senders on every delegation
// manager so background follow-ups can push to the origin conversation.
func wireDelegateSenders(delegates []*managerdelegate.Manager, gateways []gateway.Service) {
	senders := make(map[string]cron.Sender)
	for _, service := range gateways {
		if cg, ok := service.(cronGateway); ok {
			senders[cg.CronGatewayName()] = cg.CronSender()
		}
	}
	for _, dmgr := range delegates {
		if dmgr != nil {
			dmgr.SetSenders(senders)
		}
	}
}

// wireCronDispatchers attaches live agent dispatch and gateway senders to
// every profile ticker after the broker and gateways exist.
func wireCronDispatchers(names []string, cronStores map[string]*cron.Store, cronRunners map[string]*cron.Runner, stores map[string]*memory.ProfileStore, requestBroker *broker.Broker, gateways []gateway.Service, tickers []*cron.Ticker) {
	senders := make(map[string]cron.Sender)
	for _, service := range gateways {
		if Stockholder, ok := service.(cronGateway); ok {
			senders[Stockholder.CronGatewayName()] = Stockholder.CronSender()
		}
	}
	agentCall := func(ctx context.Context, call cron.AgentCall) (cron.AgentResult, error) {
		resp := make(chan broker.Result, 1)
		var override *agent.ModelOverride
		if call.Model != nil {
			override = &agent.ModelOverride{Provider: call.Model.Provider, Model: call.Model.Model}
		}
		err := requestBroker.Submit(&broker.Request{
			RequestID:   config.NewRequestID(),
			Principal:   identity.Principal{CanonicalUserID: call.Profile, Gateway: "cron", ExternalID: "cron:" + call.Profile, Assurance: identity.AssuranceCronScheduler},
			DisplayName: call.DisplayName, SessionKey: call.SessionKey, Prompt: call.Prompt,
			SkipMemory: call.SkipMemory, ModelOverride: override, EnabledToolsets: call.EnabledToolsets, MCPServers: call.MCPServers,
			ResponseChan: resp,
		})
		if err != nil {
			// Restart drain: the turn never ran. Map to the cron
			// sentinel so the dispatcher skips retries and streaks.
			if errors.Is(err, broker.ErrShuttingDown) {
				return cron.AgentResult{}, cron.ErrShuttingDown
			}
			return cron.AgentResult{}, err
		}
		select {
		case result := <-resp:
			if result.Err != nil {
				return cron.AgentResult{}, result.Err
			}
			if result.Response == nil {
				return cron.AgentResult{}, errors.New("empty agent response")
			}
			if result.Response.Error != "" {
				return cron.AgentResult{Text: result.Response.Response}, errors.New(result.Response.Error)
			}
			return cron.AgentResult{Text: result.Response.Response, Attachments: result.Response.Attachments}, nil
		case <-ctx.Done():
			return cron.AgentResult{}, ctx.Err()
		}
	}
	newGeneration := func(ctx context.Context, profile, session string) error {
		store, ok := stores[profile]
		if !ok || store == nil {
			return errors.New("profile store is unavailable")
		}
		return store.NewSessionContext(ctx, profile, session)
	}
	for i, name := range names {
		if i >= len(tickers) {
			break
		}
		store, ok := cronStores[name]
		if !ok || store == nil {
			continue
		}
		dispatcher := &cron.Dispatcher{
			Store: store, Runner: cronRunners[name], Profile: name,
			Agent: agentCall, Senders: senders, NewGeneration: newGeneration,
		}
		tickers[i].SetDispatcher(dispatcher)
		cron.RegisterDispatcher(store.Root(), dispatcher)
	}
}

func runProfilesWith(ctx context.Context, cfg *config.Config, rootLog *config.Logger, seams profileDependencies) (resultErr error) {
	if ctx.Err() != nil {
		return nil
	}
	// Subscribe before any work starts so a restart signal during startup is
	// honored at ready time instead of killing the process mid-repair.
	sigCh, stopSignals := drainSignalChannel(seams)
	defer stopSignals()
	log := rootLog.Server("app")
	processor := &profileProcessor{agents: map[string]*agent.Agent{}}
	stores := map[string]*memory.ProfileStore{}
	summarizers := map[string]*summarizerHolder{}
	var pruneWorkers []*prune.Worker
	var cronTickers []*cron.Ticker
	cronStores := make(map[string]*cron.Store)
	cronRunners := make(map[string]*cron.Runner)
	runtimes := make(map[string]*profileRuntime)
	var maintenanceStops []func()
	var shutdownClosers []func()
	var requestBroker *broker.Broker
	var gateways []gateway.Service
	var mcpManagers []*mcp.Manager
	var delegates []*managerdelegate.Manager
	delegateManagers := make(map[string]*managerdelegate.Manager)
	defer func() {
		started := time.Now()
		for _, service := range gateways {
			if outbound, ok := service.(interface{ StopOutbound() }); ok {
				outbound.StopOutbound()
			}
		}
		for _, worker := range pruneWorkers {
			worker.Stop()
		}
		for _, stop := range maintenanceStops {
			stop()
		}
		for _, dmgr := range delegates {
			dmgr.Stop()
		}
		if requestBroker != nil {
			requestBroker.Shutdown()
		}
		for _, ticker := range cronTickers {
			ticker.Stop()
		}
		process.StopAll()
		for _, closeFn := range shutdownClosers {
			closeFn()
		}
		for _, manager := range mcpManagers {
			if err := manager.Close(); err != nil {
				log.Warn("app.mcp.close_failed", "failed to close profile MCP sessions", config.ErrorField(err))
			}
		}
		for _, store := range stores {
			store.Close()
		}
		reason := "normal"
		if resultErr != nil {
			reason = "initialization_failure"
		}
		log.Info("app.stopped", "profile application cleanup completed", config.F("cleanup_reason", reason), config.F("duration_ms", time.Since(started).Milliseconds()))
	}()
	if cfg == nil || cfg.ProfileRoot == "" {
		return &Error{Event: "app.config.invalid", Message: "profile YAML configuration is required"}
	}
	directory, err := profiles.NewDirectory(cfg, rootLog)
	if err != nil {
		return &Error{Event: "app.profiles.init_failed", Message: "invalid profile routing", Cause: err}
	}
	reloader := NewReloader(cfg.ProfileRoot, cfg, directory, rootLog)
	names := []string{"default"}
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return nil
		}
		profile, _ := directory.Config(name)
		soul.RepairProfile(profile.ProfileRoot, filepath.Join(cfg.ProfileRoot, "SOUL.md"), name)
		files.RepairProfile(profile.ProfileRoot, name)
		workspace.RepairCache(profile.ProfileRoot, name, rootLog)
		serverNames := make([]string, 0, len(profile.MCPServers))
		for _, server := range profile.MCPServers {
			serverNames = append(serverNames, server.Name)
		}
		mcp.RepairTokens(profile.ProfileRoot, name, serverNames)
		chatgpt.RepairProfile(profile.ProfileRoot)
		vault.RepairProfile(profile.ProfileRoot, name)
		cron.RepairProfile(profile.ProfileRoot, name)
		imagecache.RepairProfile(profile.ProfileRoot, name)
		docache.RepairProfile(profile.ProfileRoot, name)
		docache.RepairMediaProfile(profile.ProfileRoot, name)
		plugins.RepairProfile(profile.ProfileRoot)
		store, err := memory.NewProfileStore(ctx, profile.ProfileRoot, name, rootLog)
		if err != nil {
			return &Error{Event: "app.profile_storage.init_failed", Message: "failed to open profile state", Cause: err}
		}
		stores[name] = store
		maintenanceStops = append(maintenanceStops, store.StartMaintenance())
		fileStore := files.NewProfileStore(profile.ProfileRoot, name)
		cache := imagecache.NewProfileCache(profile.ProfileRoot, name)
		mediaCache := docache.NewProfileMediaCache(profile.ProfileRoot, name)
		registry, err := tools.NewRegistryWithImageCache(profile, fileStore, store, cache, rootLog, stores)
		if err != nil {
			return &Error{Event: "app.tools.init_failed", Message: "failed to initialize profile tools", Cause: err}
		}
		shutdownClosers = append(shutdownClosers, registry.Close)
		pluginManager := plugins.NewManager(profile.ProfileRoot, name, rootLog)
		pluginManager.LoadFromProfile(profile.ProfileRoot)
		if err := pluginManager.RegisterIntoRegistry(registry, func(plugin string) bool {
			entry, ok := profile.PluginEntries[plugin]
			return ok && entry.AllowToolOverride
		}); err != nil {
			return &Error{Event: "app.plugins.init_failed", Message: "failed to initialize profile plugins", Cause: err}
		}
		publishPluginEnvPassthrough(pluginManager, name, log)
		client := seams.newClient(profile, rootLog)
		newClient, listModels := routerSeamsFor(profile, rootLog)
		modelRouter := agent.NewModelRouter(modelViewFor(profile), client, newClient, listModels, store)
		summarizer, err := compaction.NewSummarizer(client, profile.LLMGatewayModel)
		if err != nil {
			return &Error{Event: "app.session_summarizer.init_failed", Message: "failed to initialize profile summarizer", Cause: err}
		}
		holder := newSummarizerHolder(summarizer)
		summarizers[name] = holder
		manager, err := mcp.NewProfileManager(name, profile.ProfileRoot, profile.MCPServers, rootLog)
		if err != nil {
			return &Error{Event: "app.mcp.init_failed", Message: "failed to initialize profile MCP configuration", Cause: err}
		}
		mcpManagers = append(mcpManagers, manager)
		mcpProvider := mcp.NewProvider(manager, registry.EnabledBuiltinNames()...)
		soulStore := soul.NewProfileStore(profile.ProfileRoot, name, filepath.Join(cfg.ProfileRoot, "SOUL.md"))
		engine := agent.NewAgent(client, registry, profile.LLMGatewayModel, profile.LLMGatewayProvider, soulStore, store, budget.NewContextBudget(profile.ModelContextWindow), governance.DefaultGlobalPolicy(), rootLog, mcpProvider)
		engine.SetBillingBaseURL(profile.LLMGatewayURL)
		engine.SetFileMemory(fileStore)
		engine.SetImageCache(cache)
		engine.SetMediaCache(mediaCache)
		engine.SetPluginHooks(pluginManager)
		engine.SetMarkdownSummarizer(holder)
		engine.SetWorkspace(profile.ProfileRoot)
		engine.SetPluginSkills(tools.PluginSkillRoots(profile))
		engine.SetModelRouter(modelRouter)
		processor.agents[name] = engine
		runtimes[name] = &profileRuntime{
			registry: registry, mcp: mcpProvider, soul: soulStore,
			client: client, cache: cache, media: mediaCache, router: modelRouter,
		}
		cleanup := &prune.Worker{}
		cleanup.Add(imagecache.SweepHook(cache, rootLog))
		cleanup.Add(docache.SweepHook(docache.NewProfileCache(profile.ProfileRoot, name), rootLog))
		cleanup.Add(docache.SweepHook(docache.NewProfileMediaCache(profile.ProfileRoot, name), rootLog))
		cleanup.Add(workspace.SweepHook(profile.ProfileRoot, rootLog))
		cleanup.Add(process.Shared(profile.ProfileRoot).PruneHook(rootLog))
		cleanup.Add(tools.BrowserSweepHook(registry, rootLog))
		cleanup.Add(workspace.CacheChildrenHook(profile.ProfileRoot, rootLog, map[string]struct{}{
			"scratch": {}, "documents": {}, "media": {}, "processes": {}, "browser": {},
		}))
		cleanup.Start()
		pruneWorkers = append(pruneWorkers, cleanup)
		cronRunner := &cron.Runner{Root: profile.ProfileRoot, Profile: name, ScriptsDir: filepath.Join(profile.ProfileRoot, "scripts")}
		if ws := workspace.NewWorkspace(profile.ProfileRoot, name); ws.Root() != "" {
			cronRunner.Resolve = func(raw string) (string, error) {
				abs, _, err := ws.Resolve(raw)
				return abs, err
			}
			cronRunner.ResolveDir = func(raw string) (string, error) { return ws.ResolveDir(raw) }
			cronRunner.Scratch = ws.ScratchDir()
		}
		cronStores[name] = cron.NewStore(profile.ProfileRoot)
		cronRunners[name] = cronRunner
		ticker := cron.NewTicker(cronStores[name], cronRunner, 30*time.Second)
		ticker.Start()
		cronTickers = append(cronTickers, ticker)
		log.Debug("app.profile.initialized", "initialized profile runtime", config.F("profile", name))
	}
	requestBroker = broker.NewBroker(processor, brokerWorkerCount, rootLog.Server("broker"))
	requestBroker.Start()
	profileDeps := map[string]gatewayruntime.Dependencies{}
	for name, store := range stores {
		profile, _ := directory.Config(name)
		worker := compaction.NewProfileService(store)
		delegateStarter := sessioncommands.Starter(store)
		stopCanceler := stopcommands.Canceler(requestBroker)
		var modelRouter *agent.ModelRouter
		if rt := runtimes[name]; rt != nil {
			modelRouter = rt.router
			dmgr := managerdelegate.NewManager(managerdelegate.Deps{
				Profile:     name,
				ProfileRoot: profile.ProfileRoot,
				Log:         rootLog,
				Store:       store,
				Broker:      requestBroker,
				Processes:   process.Shared(profile.ProfileRoot),
				Parent:      rt.client,
				NewClient: func(provider string, endpoint config.ProviderEndpoint, virtualKey string, log *config.Logger) llm.Chatter {
					return subscriptionClientFor(profile.ProfileRoot, provider, endpoint, virtualKey, log)
				},
				Registry:   rt.registry,
				MCP:        rt.mcp,
				Soul:       rt.soul,
				Images:     rt.cache,
				Media:      rt.media,
				Window:     profile.ModelContextWindow,
				VirtualKey: profile.LLMGatewayVirtualKey,
				Config:     profile,
				Router:     rt.router,
			})
			if err := rt.registry.RegisterHandler(tooldelegate.Name, tools.DelegatePolicy(), registry.Handler(dmgr.Execute)); err != nil {
				return &Error{Event: "app.delegate.init_failed", Message: "failed to install live delegate handler", Cause: err}
			}
			dmgr.Start(ctx)
			delegates = append(delegates, dmgr)
			delegateManagers[name] = dmgr
			delegateStarter = &newSessionCanceller{Starter: store, profile: name, delegate: dmgr}
			stopCanceler = &stopWithDelegate{broker: requestBroker, profile: name, delegate: dmgr}
		}
		modelDefaults := modelcommands.DefaultsFunc(func(_ context.Context, provider, model string) error {
			return config.SetModelSelection(cfg.ProfileRoot, name, provider, model)
		})
		commands, err := commandbuiltin.NewProfileService(delegateStarter, stopCanceler, profile.ProfileRoot, summarizers[name], store, requestBroker, modelRouter, store, modelDefaults)
		if err != nil {
			return &Error{Event: "app.commands.init_failed", Message: "failed to initialize profile commands", Cause: err}
		}
		profileDeps[name] = gatewayruntime.Dependencies{Broker: requestBroker, Commands: commands, Log: rootLog, Compaction: worker}
	}
	handles := make(map[string]modelRoutingHandles, len(runtimes))
	for name, rt := range runtimes {
		if rt == nil {
			continue
		}
		summarizer := summarizers[name]
		if summarizer == nil {
			continue
		}
		handles[name] = modelRoutingHandles{router: rt.router, summarizer: summarizer, delegate: delegateManagers[name]}
	}
	reloader.SetModelApplier(buildModelApplier(handles, rootLog))
	reloader.Start(ctx)
	maintenanceStops = append(maintenanceStops, reloader.Stop)
	deps := gatewayruntime.Dependencies{Broker: requestBroker, Log: rootLog, ForProfile: func(name string) (gatewayruntime.Dependencies, bool) {
		value, ok := profileDeps[name]
		return value, ok
	}}
	gateways, err = seams.newGateways(cfg, directory, deps, rootLog)
	if err != nil {
		return &Error{Event: "app.gateways.init_failed", Message: "failed to initialize gateways", Cause: err}
	}
	wireCronDispatchers(names, cronStores, cronRunners, stores, requestBroker, gateways, cronTickers)
	wireDelegateSenders(delegates, gateways)
	for _, service := range gateways {
		go func(service gateway.Service) {
			if err := service.Start(requestBroker); err != nil {
				log.Error("app.gateway.stopped", "profile gateway stopped", config.ErrorField(err))
			}
		}(service)
	}
	log.Info("app.started", "profile application started", config.F("profile_count", len(stores)), config.F("gateway_count", len(gateways)))
	awaitShutdown(ctx, log, requestBroker, cronTickers, sigCh)
	return nil
}
