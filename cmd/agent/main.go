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
	log := config.NewLogger(cfg.LogLevel)
	log.Debug("Log level: value=%s", cfg.LogLevel)

	if cfg.OllamaModel == "" {
		log.Fatal("Missing required OLLAMA_MODEL environment variable")
	}
	log.Info("Model: name=%s", cfg.OllamaModel)

	if cfg.OllamaURL == "" {
		log.Fatal("Missing required OLLAMA_URL configuration")
	}

	llmClient := ollama.NewClient(cfg.OllamaURL, log)

	budget, budgetErr := memory.ResolveContextBudget(context.Background(), llmClient, cfg.OllamaModel)
	if budgetErr != nil {
		log.Warn("Context budget discovery failed: model=%s err=%v", cfg.OllamaModel, budgetErr)
	}
	log.Info("Context budget: window=%d prompt_budget=%d source=%s", budget.ContextWindow, budget.PromptBudget(), budget.Source)

	// The soul store is shared between the tool registry (so the agent can edit
	// its soul via the soul_memory tool) and the agent itself (so it can read
	// the current soul on every request as its system prompt).
	soulStore := soulmemory.NewStore(config.DefaultSoulPath, log)
	log.Debug("Soul file: path=%s", config.DefaultSoulPath)

	// The user memory store is owned by the tool registry so the persistent_memory
	// tool handler can remember, recall, and forget facts on behalf of the model.
	userMemStore := usermemory.NewStore(config.DefaultUserMemoryPath, log)
	log.Debug("User memory: path=%s", config.DefaultUserMemoryPath)

	accountLinkService := accountlink.NewService(config.DefaultAccountLinkPath, userMemStore, log)
	userMemStore.SetSpeakerLineResolver(accountLinkService.SpeakerLine)
	accountLinkCommands := accountlink.NewCommandHandler(accountLinkService)
	log.Debug("Account links: path=%s", config.DefaultAccountLinkPath)

	toolRegistry, err := tools.NewRegistryFromConfig(cfg, soulStore, userMemStore, llmClient, cfg.OllamaModel, log)
	if err != nil {
		log.Fatal("Failed to initialize tools: %v", err)
	}

	activeGateways, err := gateway.NewServicesFromConfig(cfg, accountLinkService, accountLinkCommands, log)
	if err != nil {
		log.Fatal("Failed to initialize gateways: %v", err)
	}

	memoryStore := memory.NewStore(memory.Options{
		MaxTurns:      cfg.MemoryMaxTurns,
		MaxAge:        cfg.MemoryMaxAge,
		ContextWindow: budget.ContextWindow,
		PromptBudget:  budget.PromptBudget(),
	}, log)
	log.Debug("Memory retention: max_turns=%d max_age=%s context_window=%d prompt_budget=%d", cfg.MemoryMaxTurns, cfg.MemoryMaxAge, budget.ContextWindow, budget.PromptBudget())

	if cfg.AgentTracePath != "" {
		log.Info("Agent trace dumps enabled: path=%s", cfg.AgentTracePath)
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
		log,
	)

	// Create the broker and start its worker pool.
	// All gateways submit requests through the broker; it enforces the concurrency
	// limit and routes responses back to the originating gateway.
	requestBroker := broker.NewBroker(agentEngine, cfg.WorkerPoolSize, log)
	requestBroker.Start()

	// Boot up all registered gateways dynamically
	log.Info("Starting Oswald AI...")
	for _, gw := range activeGateways {
		// Pass 'gw' into the closure to avoid loop variable capture bugs
		go func(g gateway.Service) {
			if err := g.Start(requestBroker); err != nil {
				log.Error("Gateway stopped: name=%s err=%v", g.Name(), err)
			}
		}(gw)
	}

	// This keeps main() alive while the gateways run in the background goroutines
	stop := make(chan os.Signal, 1)

	// Listen for standard termination signals (Ctrl+C, Docker stop, Kubernetes SIGTERM)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	<-stop // The main thread will pause here indefinitely until a signal is received

	log.Info("Shutting down Oswald AI gracefully...")

	// Drain the broker: stop accepting new requests and wait for all in-flight
	// Process() calls to complete before the process exits.
	requestBroker.Shutdown()
}
