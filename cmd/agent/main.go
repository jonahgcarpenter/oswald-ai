package main

import (
	"log"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm/ollama"
)

func main() {
	// Load config
	cfg := config.Load()

	ollamaClient := ollama.NewClient(cfg.OllamaURL)
	agentEngine := agent.NewEngine(ollamaClient, cfg)

	// Start gateways
	log.Println("Starting Oswald AI gateways...")
	if err := gateway.StartAll(cfg, agentEngine); err != nil {
		log.Fatalf("Gateway server crashed: %v", err)
	}
}
