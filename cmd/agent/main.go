package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm/ollama"
)

func main() {
	// Load config
	cfg := config.Load()

	// Initialize logger — all components receive this instance
	log := config.NewLogger(cfg.LogLevel)
	log.Info("Log level: %s", cfg.LogLevel)

	// Load worker agent registry
	workers, err := agent.LoadWorkers(cfg.WorkersConfig)
	if err != nil {
		log.Fatal("Failed to load workers config: %v", err)
	}
	log.Info("Loaded %d worker agents from %s", len(workers), cfg.WorkersConfig)

	// NOTE: I dont like this, I will only setup Ollama but leave the door open for others

	// Determine and initialize the LLM Provider (The Factory Logic)
	var llmProvider llm.Provider

	if cfg.OllamaURL != "" {
		llmProvider = ollama.NewClient(cfg.OllamaURL, log)
	} else {
		// Later, add `else if cfg.OpenAIKey != ""` here
		log.Fatal("No valid LLM provider configured (missing Ollama URL or API keys)")
	}

	agentEngine := agent.NewAgent(llmProvider, cfg.OllamaRouterModel, workers, log)

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
