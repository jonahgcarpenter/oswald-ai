package startup

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	commandbuiltin "github.com/jonahgcarpenter/oswald-ai/internal/app/commands/builtin"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/profiles"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/compaction"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
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
}

// brokerWorkerCount is fixed: the upstream model backend owns request queuing,
// so the broker only needs one worker to drain its per-conversation lanes.
const brokerWorkerCount = 1

func runProfiles(ctx context.Context, cfg *config.Config, rootLog *config.Logger) error {
	return runProfilesWith(ctx, cfg, rootLog, profileDependencies{newClient: func(cfg *config.Config, log *config.Logger) llm.Chatter {
		return llm.NewGatewayClient(cfg.LLMGatewayURL, cfg.LLMGatewayAPIKey, cfg.LLMGatewayVirtualKey, log)
	}, newGateways: gateway.NewServicesFromConfig})
}

// cronGateway exposes scheduler delivery for one gateway.
type cronGateway interface {
	CronSender() cron.Sender
	CronGatewayName() string
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
		err := requestBroker.Submit(&broker.Request{
			RequestID:   config.NewRequestID(),
			Principal:   identity.Principal{CanonicalUserID: call.Profile, Gateway: "cron", ExternalID: "cron:" + call.Profile, Assurance: identity.AssuranceCronScheduler},
			DisplayName: call.DisplayName, SessionKey: call.SessionKey, Prompt: call.Prompt,
			SkipMemory: call.SkipMemory, EnabledToolsets: call.EnabledToolsets,
			ResponseChan: resp,
		})
		if err != nil {
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
			return cron.AgentResult{Text: result.Response.Response}, nil
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
	log := rootLog.Server("app")
	processor := &profileProcessor{agents: map[string]*agent.Agent{}}
	stores := map[string]*memory.ProfileStore{}
	summarizers := map[string]*compaction.Summarizer{}
	var pruneWorkers []*prune.Worker
	var cronTickers []*cron.Ticker
	cronStores := make(map[string]*cron.Store)
	cronRunners := make(map[string]*cron.Runner)
	var maintenanceStops []func()
	var requestBroker *broker.Broker
	var gateways []gateway.Service
	var mcpManagers []*mcp.Manager
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
		if requestBroker != nil {
			requestBroker.Shutdown()
		}
		for _, ticker := range cronTickers {
			ticker.Stop()
		}
		process.StopAll()
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
		store, err := memory.NewProfileStore(ctx, profile.ProfileRoot, name, rootLog)
		if err != nil {
			return &Error{Event: "app.profile_storage.init_failed", Message: "failed to open profile state", Cause: err}
		}
		stores[name] = store
		maintenanceStops = append(maintenanceStops, store.StartMaintenance())
		fileStore := files.NewProfileStore(profile.ProfileRoot, name)
		cache := imagecache.NewProfileCache(profile.ProfileRoot, name)
		registry, err := tools.NewRegistryWithImageCache(profile, fileStore, store, cache, rootLog, stores)
		if err != nil {
			return &Error{Event: "app.tools.init_failed", Message: "failed to initialize profile tools", Cause: err}
		}
		client := seams.newClient(profile, rootLog)
		summarizer, err := compaction.NewSummarizer(client, profile.LLMGatewayModel)
		if err != nil {
			return &Error{Event: "app.session_summarizer.init_failed", Message: "failed to initialize profile summarizer", Cause: err}
		}
		summarizers[name] = summarizer
		manager, err := mcp.NewProfileManager(name, profile.ProfileRoot, profile.MCPServers, rootLog)
		if err != nil {
			return &Error{Event: "app.mcp.init_failed", Message: "failed to initialize profile MCP configuration", Cause: err}
		}
		mcpManagers = append(mcpManagers, manager)
		engine := agent.NewAgent(client, registry, profile.LLMGatewayModel, profile.LLMGatewayProvider, soul.NewProfileStore(profile.ProfileRoot, name, filepath.Join(cfg.ProfileRoot, "SOUL.md")), store, budget.NewContextBudget(profile.ModelContextWindow), governance.DefaultGlobalPolicy(), rootLog, mcp.NewProvider(manager, registry.EnabledBuiltinNames()...))
		engine.SetBillingBaseURL(profile.LLMGatewayURL)
		engine.SetFileMemory(fileStore)
		engine.SetImageCache(cache)
		engine.SetMarkdownSummarizer(summarizer)
		engine.SetWorkspace(profile.ProfileRoot)
		processor.agents[name] = engine
		cleanup := &prune.Worker{}
		cleanup.Add(imagecache.SweepHook(cache, rootLog))
		cleanup.Add(workspace.SweepHook(profile.ProfileRoot, rootLog))
		cleanup.Add(process.Shared(profile.ProfileRoot).PruneHook(rootLog))
		cleanup.Add(workspace.CacheChildrenHook(profile.ProfileRoot, rootLog, map[string]struct{}{
			"scratch": {}, "images": {}, "processes": {},
		}))
		cleanup.Start()
		pruneWorkers = append(pruneWorkers, cleanup)
		cronRunner := &cron.Runner{Root: profile.ProfileRoot}
		if ws := workspace.NewWorkspace(profile.ProfileRoot, name); ws.Root() != "" {
			cronRunner.Resolve = func(raw string) (string, error) {
				abs, _, err := ws.Resolve(raw)
				return abs, err
			}
			cronRunner.ResolveDir = func(raw string) (string, error) { return ws.ResolveDir(raw) }
			if scratch, err := ws.EnsureScratch(); err == nil {
				cronRunner.Scratch = scratch
			}
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
		commands, err := commandbuiltin.NewProfileService(store, requestBroker, profile.ProfileRoot, summarizers[name], store)
		if err != nil {
			return &Error{Event: "app.commands.init_failed", Message: "failed to initialize profile commands", Cause: err}
		}
		profileDeps[name] = gatewayruntime.Dependencies{Broker: requestBroker, Commands: commands, Log: rootLog, Compaction: worker}
	}
	deps := gatewayruntime.Dependencies{Broker: requestBroker, Log: rootLog, ForProfile: func(name string) (gatewayruntime.Dependencies, bool) {
		value, ok := profileDeps[name]
		return value, ok
	}}
	gateways, err = seams.newGateways(cfg, directory, deps, rootLog)
	if err != nil {
		return &Error{Event: "app.gateways.init_failed", Message: "failed to initialize gateways", Cause: err}
	}
	wireCronDispatchers(names, cronStores, cronRunners, stores, requestBroker, gateways, cronTickers)
	for _, service := range gateways {
		go func(service gateway.Service) {
			if err := service.Start(requestBroker); err != nil {
				log.Error("app.gateway.stopped", "profile gateway stopped", config.ErrorField(err))
			}
		}(service)
	}
	log.Info("app.started", "profile application started", config.F("profile_count", len(stores)), config.F("gateway_count", len(gateways)))
	<-ctx.Done()
	return nil
}
