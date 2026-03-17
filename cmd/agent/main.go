package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	"github.com/jonahgcarpenter/oswald-ai/internal/provider"
	"github.com/jonahgcarpenter/oswald-ai/internal/provider/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/search/searxng"
)

func main() {
	// Load config
	cfg := config.Load()

	// Initialize logger — all components receive this instance
	log := config.NewLogger(cfg.LogLevel)
	log.Info("Log level: %s", cfg.LogLevel)

	// Load worker agent registry (query generator + uncensored response model)
	workers, err := agent.LoadWorkers(cfg.WorkersConfig)
	if err != nil {
		log.Fatal("Failed to load workers config: %v", err)
	}

	queryWorker := agent.FindWorker(workers, "QUERY")
	if queryWorker == nil {
		log.Fatal("Workers config is missing required QUERY worker entry")
	}

	responseWorker := agent.FindWorker(workers, "GENERAL")
	if responseWorker == nil {
		log.Fatal("Workers config is missing required GENERAL worker entry")
	}

	log.Info("Query worker: model=%s", queryWorker.ResolveModel())
	log.Info("Response worker: model=%s", responseWorker.ResolveModel())

	// NOTE: Only Ollama is supported; leave the door open for future providers
	var llmProvider provider.Provider
	if cfg.OllamaURL != "" {
		llmProvider = ollama.NewClient(cfg.OllamaURL, log)
	} else {
		// Later, add `else if cfg.OpenAIKey != ""` here
		log.Fatal("No valid LLM provider configured (missing OLLAMA_URL)")
	}

	// Initialize the SearXNG search client
	searchClient := searxng.NewClient(cfg.SearxngURL, log)
	log.Info("SearXNG search client configured: %s", cfg.SearxngURL)

	agentEngine := agent.NewAgent(
		llmProvider,
		searchClient,
		queryWorker,
		responseWorker,
		cfg.QueryMaxIterations,
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
