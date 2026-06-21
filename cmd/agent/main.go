package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/modelinfo"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func main() {
	// Load config
	cfg := config.Load()

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

	llmClient := llm.NewGatewayClient(cfg.LLMGatewayURL, cfg.LLMGatewayAPIKey, cfg.LLMGatewayVirtualKey, llmHTTPTimeout, rootLog.Server("provider.gateway"))

	details, budgetErr := modelinfo.Resolve(context.Background(), cfg, rootLog)
	budget := memory.ContextBudgetFromModelDetails(details)
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

	// The soul store is shared between the tool registry (so the agent can edit
	// its soul via the soul.* tools) and the agent itself (so it can read
	// the current soul on every request as its system prompt).
	soulStore := soul.NewStore(config.DefaultSoulPath, rootLog.Server("memory.soul"))
	log.Debug("app.memory_soul.configured", "configured soul file path", config.F("path", config.DefaultSoulPath))

	// The user memory store is owned by the tool registry so the memory.* tool
	// handlers can remember, recall, and forget facts on behalf of the model.
	userMemStore, err := usermemory.NewSQLiteStore(config.DefaultAccountLinkPath, config.DefaultUserMemoryPath, llmClient, cfg.LLMGatewayEmbeddingModel, rootLog.Server("memory.user"))
	if err != nil {
		log.Fatal("app.memory_user.init_failed", "failed to initialize user memory store", config.ErrorField(err))
	}
	defer userMemStore.Close() // nolint:errcheck
	log.Debug("app.memory_user.configured", "configured user memory database", config.F("path", config.DefaultAccountLinkPath), config.F("legacy_path", config.DefaultUserMemoryPath))

	accountLinkService := accountlinking.NewService(config.DefaultAccountLinkPath, userMemStore, rootLog.Server("account_link"))
	if err := accountLinkService.Initialize(); err != nil {
		log.Fatal("app.account_link.init_failed", "failed to initialize account link store", config.ErrorField(err))
	}
	userMemStore.SetSpeakerLineResolver(accountLinkService.SpeakerLine)
	if err := userMemStore.MigrateLegacyMarkdown(); err != nil {
		log.Fatal("app.memory_user.legacy_migration_failed", "failed to migrate legacy user memory", config.ErrorField(err))
	}
	if cfg.LLMGatewayEmbeddingModel != "" {
		if err := userMemStore.BackfillEmbeddings(context.Background()); err != nil {
			log.Warn("app.memory_user.embedding_backfill_failed", "failed to backfill user memory embeddings", config.F("status", "degraded"), config.ErrorField(err))
		}
	}
	accountLinkCommands := accountlinking.NewCommandHandler(accountLinkService)
	commandRouter := commands.NewRouter(accountLinkCommands)
	log.Debug("app.account_link.configured", "configured account link database", config.F("path", config.DefaultAccountLinkPath))

	mcpManager, err := mcp.NewManagerFromConfig(context.Background(), cfg, rootLog)
	if err != nil {
		log.Fatal("app.mcp.init_failed", "failed to initialize MCP clients", config.ErrorField(err))
	}

	memoryStore := memory.NewStore(memory.Options{
		MaxTurns:      cfg.MemoryMaxTurns,
		MaxAge:        cfg.MemoryMaxAge,
		ContextWindow: budget.ContextWindow,
		PromptBudget:  budget.PromptBudget(),
	}, rootLog.Server("memory.session"))
	log.Debug("app.memory_retention.configured", "configured memory retention",
		config.F("max_turn_count", cfg.MemoryMaxTurns),
		config.F("max_age", cfg.MemoryMaxAge.String()),
		config.F("context_window", budget.ContextWindow),
		config.F("prompt_budget", budget.PromptBudget()),
	)
	if cfg.LLMGatewayEmbeddingModel != "" {
		log.Info("app.memory_vector.enabled", "enabled semantic session-memory retrieval",
			config.F("embedding_model", cfg.LLMGatewayEmbeddingModel),
			config.F("recent_turn_count", 0),
			config.F("max_relevant_turn_count", 3),
			config.F("min_similarity", 0.70),
			config.F("recent_policy", "tool_only"),
		)
	} else {
		log.Debug("app.memory_vector.disabled", "semantic session-memory retrieval disabled")
	}

	toolRegistry, err := tools.NewRegistryFromConfig(cfg, soulStore, userMemStore, memoryStore, llmClient, cfg.LLMGatewayModel, mcpManager, rootLog)
	if err != nil {
		log.Fatal("app.tools.init_failed", "failed to initialize tools", config.ErrorField(err))
	}

	activeGateways, err := gateway.NewServicesFromConfig(cfg, accountLinkService, commandRouter, rootLog)
	if err != nil {
		log.Fatal("app.gateways.init_failed", "failed to initialize gateways", config.ErrorField(err))
	}

	agentEngine := agent.NewAgent(
		llmClient,
		llmClient,
		toolRegistry,
		cfg.LLMGatewayModel,
		cfg.LLMGatewayEmbeddingModel,
		soulStore,
		userMemStore,
		budget,
		cfg.MaxToolFailureRetries,
		agentRequestTimeout,
		memoryStore,
		rootLog,
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

	// Drain the broker: stop accepting new requests and wait for all in-flight
	// Process() calls to complete before the process exits.
	requestBroker.Shutdown()
	if err := mcpManager.Close(); err != nil {
		log.Warn("app.mcp.shutdown_failed", "failed to shut down MCP clients", config.ErrorField(err), config.F("status", "degraded"))
	}
}
