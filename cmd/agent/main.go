package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
)

func main() {
	// Load config
	cfg := config.Load()

	// Initialize logger — all components receive this instance
	log := config.NewLogger(cfg.LogLevel)
	log.Info("Log level: %s", cfg.LogLevel)

	// Load worker agent registry (single GENERAL worker drives the agentic loop)
	workers, err := agent.LoadWorkers(cfg.WorkersConfig)
	if err != nil {
		log.Fatal("Failed to load workers config: %v", err)
	}

	responseWorker := agent.FindWorker(workers, "GENERAL")
	if responseWorker == nil {
		log.Fatal("Workers config is missing required GENERAL worker entry")
	}

	log.Info("Worker: model=%s", responseWorker.ResolveModel())

	if cfg.OllamaURL == "" {
		log.Fatal("Missing required OLLAMA_URL configuration")
	}

	llmClient := ollama.NewClient(cfg.OllamaURL, log)

	toolRegistry, err := tools.NewRegistryFromConfig(cfg, log)
	if err != nil {
		log.Fatal("Failed to initialize tools: %v", err)
	}

	activeGateways, err := gateway.NewServicesFromConfig(cfg, log)
	if err != nil {
		log.Fatal("Failed to initialize gateways: %v", err)
	}

	agentEngine := agent.NewAgent(
		llmClient,
		toolRegistry,
		responseWorker,
		cfg.MaxIterations,
		log,
	)

	// Boot up all registered gateways dynamically
	log.Info("Starting Oswald AI...")
	for _, gw := range activeGateways {
		// Pass 'gw' into the closure to avoid loop variable capture bugs
		go func(g gateway.Service) {
			if err := g.Start(agentEngine); err != nil {
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
}
