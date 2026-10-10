package startup

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	commandbuiltin "github.com/jonahgcarpenter/oswald-ai/internal/commands/builtin"
	"github.com/jonahgcarpenter/oswald-ai/internal/compaction"
	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/profiles"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
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

func runProfilesWith(ctx context.Context, cfg *config.Config, rootLog *config.Logger, seams profileDependencies) (resultErr error) {
	if ctx.Err() != nil {
		return nil
	}
	log := rootLog.Server("app")
	processor := &profileProcessor{agents: map[string]*agent.Agent{}}
	stores := map[string]*memory.ProfileStore{}
	compactors := map[string]*compaction.LLMCompactor{}
	var imageWorkers []*imagecache.Worker
	var compressionWorkers []*compaction.ProfileService
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
		for _, worker := range imageWorkers {
			worker.Stop()
		}
		for _, stop := range maintenanceStops {
			stop()
		}
		if requestBroker != nil {
			requestBroker.Shutdown()
		}
		for _, worker := range compressionWorkers {
			worker.Stop()
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
		compactor, err := compaction.NewLLMCompactor(client, profile.LLMGatewayModel, rootLog)
		if err != nil {
			return &Error{Event: "app.session_compactor.init_failed", Message: "failed to initialize profile compactor", Cause: err}
		}
		compactor.SetBillingBaseURL(profile.LLMGatewayURL)
		compactors[name] = compactor
		manager, err := mcp.NewProfileManager(name, profile.MCPServers, rootLog)
		if err != nil {
			return &Error{Event: "app.mcp.init_failed", Message: "failed to initialize profile MCP configuration", Cause: err}
		}
		mcpManagers = append(mcpManagers, manager)
		engine := agent.NewAgent(client, registry, profile.LLMGatewayModel, profile.LLMGatewayProvider, soul.NewProfileStore(profile.ProfileRoot, name, filepath.Join(cfg.ProfileRoot, "SOUL.md")), store, budget.NewContextBudget(profile.ModelContextWindow), governance.DefaultGlobalPolicy(), rootLog, mcp.NewProvider(manager))
		engine.SetBillingBaseURL(profile.LLMGatewayURL)
		engine.SetFileMemory(fileStore)
		engine.SetImageCache(cache)
		engine.SetForegroundCompactor(compactor)
		processor.agents[name] = engine
		worker := imagecache.NewWorker(cache, rootLog)
		worker.Start()
		imageWorkers = append(imageWorkers, worker)
		log.Debug("app.profile.initialized", "initialized profile runtime", config.F("profile", name))
	}
	requestBroker = broker.NewBroker(processor, brokerWorkerCount, rootLog.Server("broker"))
	requestBroker.Start()
	profileDeps := map[string]gatewayruntime.Dependencies{}
	for name, store := range stores {
		commands, err := commandbuiltin.NewProfileService(store, requestBroker)
		if err != nil {
			return &Error{Event: "app.commands.init_failed", Message: "failed to initialize profile commands", Cause: err}
		}
		profile, _ := directory.Config(name)
		worker := compaction.NewProfileService(store, compactors[name], requestBroker, budget.NewContextBudget(profile.ModelContextWindow), rootLog)
		worker.Start()
		compressionWorkers = append(compressionWorkers, worker)
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
