package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/accountlink"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/soulmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/usermemory"
)

func main() {
	// Load config
	cfg := config.Load()

	// Initialize logger — all components receive this instance
	rootLog := config.NewLogger(cfg.LogLevel)
	log := rootLog.Server("app")
	log.Debug("app.config.loaded", "loaded runtime configuration", config.F("log_level", cfg.LogLevel.String()))

	if cfg.OllamaModel == "" {
		log.Fatal("app.config.invalid", "missing required OLLAMA_MODEL environment variable")
	}
	log.Info("app.model.selected", "selected Ollama model", config.F("model", cfg.OllamaModel))

	if cfg.OllamaURL == "" {
		log.Fatal("app.config.invalid", "missing required OLLAMA_URL configuration")
	}

	llmClient := ollama.NewClient(cfg.OllamaURL, rootLog.Server("provider.ollama"))

	budget, budgetErr := memory.ResolveContextBudget(context.Background(), llmClient, cfg.OllamaModel)
	if budgetErr != nil {
		log.Warn("app.context_budget.resolve_failed", "failed to discover context budget",
			config.F("model", cfg.OllamaModel),
			config.ErrorField(budgetErr),
		)
	}
	log.Info("app.context_budget.resolved", "resolved context budget",
		config.F("model", cfg.OllamaModel),
		config.F("context_window", budget.ContextWindow),
		config.F("prompt_budget", budget.PromptBudget()),
		config.F("source", budget.Source),
	)

	// The soul store is shared between the tool registry (so the agent can edit
	// its soul via the soul_memory tool) and the agent itself (so it can read
	// the current soul on every request as its system prompt).
	soulStore := soulmemory.NewStore(config.DefaultSoulPath, rootLog.Server("memory.soul"))
	log.Debug("app.memory_soul.configured", "configured soul file path", config.F("path", config.DefaultSoulPath))

	// The user memory store is owned by the tool registry so the persistent_memory
	// tool handler can remember, recall, and forget facts on behalf of the model.
	userMemStore := usermemory.NewStore(config.DefaultUserMemoryPath, rootLog.Server("memory.user"))
	log.Debug("app.memory_user.configured", "configured user memory path", config.F("path", config.DefaultUserMemoryPath))

	accountLinkService := accountlink.NewService(config.DefaultAccountLinkPath, userMemStore, rootLog.Server("account_link"))
	userMemStore.SetSpeakerLineResolver(accountLinkService.SpeakerLine)
	accountLinkCommands := accountlink.NewCommandHandler(accountLinkService)
	log.Debug("app.account_link.configured", "configured account link store", config.F("path", config.DefaultAccountLinkPath))

	toolRegistry, err := tools.NewRegistryFromConfig(cfg, soulStore, userMemStore, llmClient, cfg.OllamaModel, rootLog)
	if err != nil {
		log.Fatal("app.tools.init_failed", "failed to initialize tools", config.ErrorField(err))
	}

	activeGateways, err := gateway.NewServicesFromConfig(cfg, accountLinkService, accountLinkCommands, rootLog)
	if err != nil {
		log.Fatal("app.gateways.init_failed", "failed to initialize gateways", config.ErrorField(err))
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

	if cfg.AgentTracePath != "" {
		log.Info("app.trace.enabled", "enabled agent trace dumps", config.F("path", cfg.AgentTracePath))
	}

	agentEngine := agent.NewAgent(
		llmClient,
		toolRegistry,
		cfg.OllamaModel,
		soulStore,
		userMemStore,
		budget,
		cfg.MaxToolFailureRetries,
		memoryStore,
		cfg.AgentTracePath,
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
}
