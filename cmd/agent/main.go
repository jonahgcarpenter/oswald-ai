package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/ws"
)

func main() {
	// Load config
	cfg := config.Load()

	// Expose /ws endpoint
	http.HandleFunc("/ws", ws.HandleConnections)

	fmt.Printf("Websocket server starting on :%s\n", cfg.Port)

	// Start server
	err := http.ListenAndServe(":"+cfg.Port, nil)
	if err != nil {
		log.Fatal("ListenAndServe error:", err)
	}
}
