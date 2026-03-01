package gateway

import (
	"log"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/discord"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/websocket"
)

// StartAll initializes and runs all configured gateways based on the environment variables.
// It blocks indefinitely while the HTTP/WS server is running.
func StartAll(cfg *config.Config, aiAgent *agent.Agent) error {

	// Initialize Discord Gateway (if configured)
	if cfg.DiscordToken != "" {
		discordBot, err := discord.NewBot(cfg, aiAgent)
		if err != nil {
			log.Printf("Warning: Failed to create Discord bot: %v", err)
		} else {
			go func() {
				if err := discordBot.Start(); err != nil {
					log.Printf("Warning: Failed to start Discord bot: %v", err)
				}
			}()
			// Note: We are relying on the process exiting to kill this right now.
			// In a production app, you might want to pass a context or a stop channel here.
		}
	} else {
		log.Println("DISCORD_TOKEN not set, skipping Discord bot setup.")
	}

	// Initialize WebSocket Gateway
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		websocket.HandleConnections(w, r, aiAgent)
	})

	log.Printf("Websocket server starting on :%s\n", cfg.Port)

	// Start the HTTP server (This is a blocking call)
	return http.ListenAndServe(":"+cfg.Port, nil)
}

