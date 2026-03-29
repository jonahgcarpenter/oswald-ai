package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/search/searxng"
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

	// Initialize the SearXNG search client
	searchClient := searxng.NewClient(cfg.SearxngURL, log)
	log.Info("SearXNG search client configured: %s", cfg.SearxngURL)

	// Load tool definitions from markdown files and register execution handlers.
	// To add a new tool: create config/tools/<name>.md and register a handler below.
	toolRegistry := agent.NewToolRegistry(log)
	if err := toolRegistry.LoadFromDirectory(cfg.ToolsConfig); err != nil {
		log.Fatal("Failed to load tool definitions: %v", err)
	}

	if err := toolRegistry.RegisterHandler("web_search", agent.NewWebSearchHandler(searchClient, log)); err != nil {
		log.Fatal("Failed to register web_search handler: %v", err)
	}

	log.Info("Tool registry: %d tool(s) loaded from %s", toolRegistry.Count(), cfg.ToolsConfig)

	agentEngine := agent.NewAgent(
		llmClient,
		toolRegistry,
		responseWorker,
		cfg.MaxIterations,
		log,
	)

	// Initialize a slice of enabled gateways
	var activeGateways []gateway.Service

	// Register Websocket
	activeGateways = append(activeGateways, &gateway.WebsocketGateway{
		Port: cfg.Port,
		Log:  log,
	})

	// Conditionally register Discord
	if cfg.DiscordToken != "" {
		activeGateways = append(activeGateways, &gateway.DiscordGateway{
			Token: cfg.DiscordToken,
			Log:   log,
		})
	}

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
