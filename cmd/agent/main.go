package main

import (
	"log"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm/ollama"
)

func main() {
	// Load config
	cfg := config.Load()

	// Determine and initialize the LLM Provider (The Factory Logic)
	var llmProvider llm.Provider

	if cfg.OllamaURL != "" {
		llmProvider = ollama.NewClient(cfg.OllamaURL)
	} else {
		// Later, add `else if cfg.OpenAIKey != ""` here
		log.Fatal("No valid LLM provider configured (missing Ollama URL or API keys)")
	}

	agentEngine := agent.NewAgent(llmProvider, cfg)

	// Start gateways
	log.Println("Starting Oswald AI gateways...")
	if err := gateway.StartAll(cfg, agentEngine); err != nil {
		log.Fatalf("Gateway server crashed: %v", err)
	}
}
