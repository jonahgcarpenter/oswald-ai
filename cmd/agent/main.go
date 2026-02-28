package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/discord"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/websocket"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm/ollama"
)

func main() {
	// Load config
	cfg := config.Load()

	ollamaClient := ollama.NewClient(cfg.OllamaURL)

	if cfg.DiscordToken != "" {
		discordBot, err := discord.NewBot(cfg, ollamaClient)
		if err != nil {
			// We log the error but don't Fatal, so the WS server can still run
			log.Printf("Warning: Failed to create Discord bot: %v", err)
		} else {
			go func() {
				if err := discordBot.Start(); err != nil {
					log.Printf("Warning: Failed to start Discord bot: %v", err)
				}
			}()
			defer discordBot.Stop()
		}
	} else {
		log.Println("Info: DISCORD_TOKEN not set, skipping Discord bot setup.")
	}

	// Expose /ws endpoint
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		// We pass the client and the router model into the websocket handler
		websocket.HandleConnections(w, r, ollamaClient, cfg)
	})

	fmt.Printf("Websocket server starting on :%s\n", cfg.Port)

	// Start server
	err := http.ListenAndServe(":"+cfg.Port, nil)
	if err != nil {
		log.Fatal("ListenAndServe error:", err)
	}
}
