package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	commandbuiltin "github.com/jonahgcarpenter/oswald-ai/internal/commands/builtin"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/formationruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/indexruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/maintenanceruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/modelinfo"
	"github.com/jonahgcarpenter/oswald-ai/internal/privacyruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/sessionruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/globalmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/websocketauth"
)

func main() {
	// Load config
	cfg, err := config.Load()
	if err != nil {
		config.NewLogger(config.LevelInfo).Server("app").Fatal("app.config.invalid", "invalid runtime configuration", config.ErrorField(err))
	}

	// Initialize logger — all components receive this instance
	rootLog := config.NewLogger(cfg.LogLevel)
	log := rootLog.Server("app")
	log.Debug("app.config.loaded", "loaded runtime configuration", config.F("log_level", cfg.LogLevel.String()))

	if cfg.LLMGatewayModel == "" {
		log.Fatal("app.config.invalid", "missing required LLM_GATEWAY_MODEL environment variable")
	}
	log.Info("app.model.selected", "selected LLM gateway model", config.F("model", cfg.LLMGatewayModel))

	if cfg.LLMGatewayURL == "" {
		log.Fatal("app.config.invalid", "missing required LLM_GATEWAY_URL configuration")
	}

	llmHTTPTimeout := cfg.LLMGatewayTimeout + 10*time.Second
	agentRequestTimeout := cfg.LLMGatewayTimeout + 30*time.Second
	log.Info("app.timeout.configured", "configured request timeouts",
		config.F("llm_gateway_timeout", cfg.LLMGatewayTimeout.String()),
		config.F("llm_http_timeout", llmHTTPTimeout.String()),
		config.F("agent_request_timeout", agentRequestTimeout.String()),
	)

	llmClient := llm.NewGatewayClient(cfg.LLMGatewayURL, cfg.LLMGatewayAPIKey, cfg.LLMGatewayVirtualKey, llmHTTPTimeout, rootLog)

	details, budgetErr := modelinfo.Resolve(context.Background(), cfg, rootLog)
	budget := promptbudget.FromModelDetails(details)
	if budgetErr != nil {
		log.Warn("app.context_budget.resolve_failed", "failed to discover context budget",
			config.F("model", cfg.LLMGatewayModel),
			config.ErrorField(budgetErr),
		)
	}
	log.Info("app.context_budget.resolved", "resolved context budget",
		config.F("model", cfg.LLMGatewayModel),
		config.F("provider", details.Provider),
		config.F("context_window", budget.ContextWindow),
		config.F("prompt_budget", budget.PromptBudget()),
		config.F("source", budget.Source),
	)

	// The operator-managed soul file is read fresh for every request and used as
	// the agent's system prompt.
	soulStore := soul.NewStore(config.DefaultSoulPath)
	log.Debug("app.memory_soul.configured", "configured soul file path", config.F("path", config.DefaultSoulPath))

	// The user memory store shares the account-link database and migrates legacy
	// memory categories into the current canonical schema at startup.
	userMemStore, err := usermemory.NewSQLiteStore(config.DefaultAccountLinkPath, llmClient, cfg.LLMGatewayEmbeddingModel, rootLog.Server("memory.user"))
	if err != nil {
		log.Fatal("app.memory_user.init_failed", "failed to initialize user memory store", config.ErrorField(err))
	}
	defer userMemStore.Close() // nolint:errcheck
	userMemStore.SetRetentionPolicy(cfg.RetentionPolicy)
	log.Debug("app.memory_user.configured", "configured user memory database", config.F("path", config.DefaultAccountLinkPath))
	globalMemStore, err := globalmemory.NewStore(config.DefaultAccountLinkPath, rootLog.Server("memory.global"))
	if err != nil {
		log.Fatal("app.memory_global.init_failed", "failed to initialize global memory store", config.ErrorField(err))
	}
	defer globalMemStore.Close() // nolint:errcheck
	log.Debug("app.memory_global.configured", "configured global memory database", config.F("path", config.DefaultAccountLinkPath))
	mcpStore, err := mcp.NewStore(config.DefaultAccountLinkPath, cfg.MCPConfigEncryptionKey, rootLog.Server("mcp.store"))
	if err != nil {
		log.Fatal("app.mcp.init_failed", "failed to initialize MCP config store", config.ErrorField(err))
	}
	defer mcpStore.Close() // nolint:errcheck
	mcpManager := mcp.NewManagerFromStore(mcpStore, rootLog)
	mcpProvider := mcp.NewProvider(mcpManager)
	accountLinkService := accountlinking.NewService(config.DefaultAccountLinkPath, userMemStore, mcpManager, rootLog.Server("account_link"))
	if err := accountLinkService.Initialize(); err != nil {
		log.Fatal("app.account_link.init_failed", "failed to initialize account link store", config.ErrorField(err))
	}
	defer accountLinkService.Close() // nolint:errcheck
	userMemStore.SetSpeakerLineResolver(accountLinkService.SpeakerLine)
	webSocketDB, err := database.Open(config.DefaultAccountLinkPath, rootLog.Server("websocket.auth.store"))
	if err != nil {
		log.Fatal("app.websocket_auth.init_failed", "failed to open websocket authorization store", config.ErrorField(err))
	}
	defer webSocketDB.Close() // nolint:errcheck
	webSocketAuth, err := websocketauth.New(webSocketDB.SQL(), cfg.WebSocketAuthSigningKey, cfg.WebSocketAuthMaxTokenTTL)
	if err != nil {
		log.Fatal("app.websocket_auth.init_failed", "failed to initialize websocket authorization", config.ErrorField(err))
	}
	bootstrap, err := webSocketAuth.EnsureBootstrap(context.Background())
	if err != nil {
		log.Fatal("app.websocket_auth.bootstrap_failed", "failed to initialize websocket bootstrap", config.ErrorField(err))
	}
	if bootstrap != nil {
		fmt.Fprintf(os.Stdout, "\nOswald first-run administrator bootstrap\n\nWebSocket access token (expires %s):\n%s\n\n1. Connect to ws://localhost:%s/ws with Authorization: Bearer <token>.\n2. On the permanent client, request a device code with POST /auth/device.\n3. From this bootstrap connection, run /bootstrap admin <code> <display_name>.\n4. Poll POST /auth/token on the permanent client, then connect with its access token.\n5. After the permanent administrator connects, delete temporary user %s with /deleteuser %s.\n\n", bootstrap.ExpiresAt.Format(time.RFC3339), bootstrap.AccessToken, cfg.Port, bootstrap.DefaultUserID, bootstrap.DefaultUserID)
	}
	indexService := indexruntime.NewService(userMemStore, llmClient, cfg.LLMGatewayEmbeddingModel, rootLog)
	indexService.Start(context.Background())
	maintenanceService := maintenanceruntime.NewService(userMemStore, cfg.RetentionPolicy, rootLog)
	maintenanceService.Start(context.Background())
	commandService, err := commandbuiltin.NewServiceWithPrivacyAndClientAuth(accountLinkService, userMemStore, commandbuiltin.PrivacyDeps{Policy: cfg.RetentionPolicy, Logger: rootLog.Server("privacy")}, commandbuiltin.ClientAuthDeps{Service: webSocketAuth, Authorizer: accountLinkService}, commandbuiltin.MCPDeps{Store: mcpStore, Manager: mcpManager})
	if err != nil {
		log.Fatal("app.commands.init_failed", "failed to initialize command service", config.ErrorField(err))
	}
	log.Debug("app.account_link.configured", "configured account link database", config.F("path", config.DefaultAccountLinkPath))
	formationService := formationruntime.NewService(userMemStore, formationruntime.NewLLMExtractor(llmClient, cfg.LLMGatewayModel), cfg.LLMGatewayModel, rootLog)
	formationService.Start(context.Background())
	compactionService := sessionruntime.NewService(userMemStore, sessionruntime.NewLLMExtractor(llmClient, cfg.LLMGatewayModel), cfg.LLMGatewayModel, budget, cfg.LLMGatewayTimeout, rootLog)
	compactionService.Start(context.Background())

	if cfg.LLMGatewayEmbeddingModel != "" {
		log.Info("app.memory_vector.enabled", "enabled semantic durable-memory retrieval",
			config.F("embedding_model", cfg.LLMGatewayEmbeddingModel),
		)
	} else {
		log.Debug("app.memory_vector.disabled", "semantic durable-memory retrieval disabled")
	}

	toolRegistry, err := tools.NewRegistryFromConfig(cfg, userMemStore, globalMemStore, accountLinkService, rootLog)
	if err != nil {
		log.Fatal("app.tools.init_failed", "failed to initialize tools", config.ErrorField(err))
	}

	privacyBus := privacyruntime.NewBus()
	runtimeDeps := gatewayruntime.Dependencies{
		Commands:     commandService,
		Access:       accountLinkService,
		Log:          rootLog,
		Formation:    formationService,
		Compaction:   compactionService,
		GlobalMemory: globalMemStore,
		PrivacyBus:   privacyBus,
	}
	activeGateways, err := gateway.NewServicesFromConfig(cfg, accountLinkService, webSocketAuth, runtimeDeps, rootLog)
	if err != nil {
		log.Fatal("app.gateways.init_failed", "failed to initialize gateways", config.ErrorField(err))
	}
	privacyDispatcher := privacyruntime.NewService(userMemStore, privacyBus, rootLog)
	privacyDispatcher.Start(context.Background())

	agentEngine := agent.NewAgent(
		llmClient,
		toolRegistry,
		cfg.LLMGatewayModel,
		soulStore,
		userMemStore,
		globalMemStore,
		budget,
		cfg.MaxToolFailureRetries,
		agentRequestTimeout,
		rootLog,
		mcpProvider,
	)

	// Create the broker and start its worker pool.
	// All gateways submit requests through the broker; it enforces the concurrency
	// limit and routes responses back to the originating gateway.
	requestBroker := broker.NewBroker(agentEngine, cfg.WorkerPoolSize, rootLog.Server("broker"))
	requestBroker.Start()
	// Boot up all registered gateways dynamically
	log.Info("app.start", "starting application")
	for _, gw := range activeGateways {
		// Pass 'gw' into the closure to avoid loop variable capture bugs
		go func(g gateway.Service) {
			if err := g.Start(requestBroker); err != nil {
				log.Error("app.gateway.stopped", "gateway stopped", config.F("gateway", g.Name()), config.ErrorField(err))
			}
		}(gw)
	}

	// This keeps main() alive while the gateways run in the background goroutines
	stop := make(chan os.Signal, 1)

	// Listen for standard termination signals (Ctrl+C, Docker stop, Kubernetes SIGTERM)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	<-stop // The main thread will pause here indefinitely until a signal is received

	log.Info("app.shutdown", "shutting down application")
	maintenanceService.Stop()
	privacyDispatcher.Stop()

	// Drain the broker: stop accepting new requests and wait for all in-flight
	// Process() calls to complete before the process exits.
	requestBroker.Shutdown()
	indexService.Stop()
	formationService.Stop()
	compactionService.Stop()
	if err := mcpManager.Close(); err != nil {
		log.Warn("app.mcp.shutdown_failed", "failed to shut down MCP clients", config.ErrorField(err), config.F("status", "degraded"))
	}
}
