package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/ws"
)

func main() {
	// Load config
	cfg := config.Load()

	ollamaClient := ollama.NewClient(cfg.OllamaURL)

	// Expose /ws endpoint
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		// We pass the client and the router model into the websocket handler
		ws.HandleConnections(w, r, ollamaClient, cfg)
	})

	fmt.Printf("Websocket server starting on :%s\n", cfg.Port)

	// Start server
	err := http.ListenAndServe(":"+cfg.Port, nil)
	if err != nil {
		log.Fatal("ListenAndServe error:", err)
	}
}
