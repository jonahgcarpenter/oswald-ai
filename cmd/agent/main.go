package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
)

func main() {
	// Load config
	cfg := config.Load()

	// Initialize logger — all components receive this instance
	log := config.NewLogger(cfg.LogLevel)
	log.Debug("Log level: %s", cfg.LogLevel)

	// Load worker agent registry (single GENERAL worker drives the agentic loop)
	workers, err := agent.LoadWorkers(cfg.WorkersConfig)
	if err != nil {
		log.Fatal("Failed to load workers config: %v", err)
	}

	responseWorker := agent.FindWorker(workers, "GENERAL")
	if responseWorker == nil {
		log.Fatal("Workers config is missing required GENERAL worker entry")
	}

	log.Info("Worker model: %s", responseWorker.ResolveModel())

	if cfg.OllamaURL == "" {
		log.Fatal("Missing required OLLAMA_URL configuration")
	}

	llmClient := ollama.NewClient(cfg.OllamaURL, log)
	workerBudgets := make(map[string]agent.ContextBudget, len(workers))
	for i := range workers {
		budget, budgetErr := agent.ResolveContextBudget(context.Background(), llmClient, &workers[i])
		if budgetErr != nil {
			log.Warn("Failed to discover context budget for worker %s (%s): %v", workers[i].Category, workers[i].ResolveModel(), budgetErr)
		}
		workerBudgets[workers[i].Category] = budget
		log.Info("Worker %s context budget: window=%d prompt_budget=%d source=%s", workers[i].Category, budget.ContextWindow, budget.PromptBudget(), budget.Source)
	}

	budget, ok := workerBudgets[responseWorker.Category]
	if !ok {
		log.Fatal("Missing context budget for GENERAL worker")
	}

	toolRegistry, err := tools.NewRegistryFromConfig(cfg, log)
	if err != nil {
		log.Fatal("Failed to initialize tools: %v", err)
	}

	activeGateways, err := gateway.NewServicesFromConfig(cfg, log)
	if err != nil {
		log.Fatal("Failed to initialize gateways: %v", err)
	}

	memoryStore := memory.NewStore(memory.Options{
		MaxTurns:      cfg.MemoryMaxTurns,
		MaxAge:        cfg.MemoryMaxAge,
		ContextWindow: budget.ContextWindow,
		PromptBudget:  budget.PromptBudget(),
	}, cfg.MemoryDebugDumpPath, log)
	log.Debug("Memory: retaining in-process session history until restart (max_turns=%d max_age=%s context_window=%d prompt_budget=%d)", cfg.MemoryMaxTurns, cfg.MemoryMaxAge, budget.ContextWindow, budget.PromptBudget())
	if cfg.MemoryDebugDumpPath != "" {
		log.Debug("Debug dump snapshots enabled at %s", cfg.MemoryDebugDumpPath)
	}

	agentEngine := agent.NewAgent(
		llmClient,
		toolRegistry,
		responseWorker,
		budget,
		cfg.MaxIterations,
		memoryStore,
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
				log.Error("%s gateway stopped/failed: %v", g.Name(), err)
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
