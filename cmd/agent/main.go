package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/ws"
)

func main() {
	// Expose /ws endpoint
	http.HandleFunc("/ws", ws.HandleConnections)

	fmt.Println("Websocket server starting on :8080...")

	// Start server
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe error:", err)
	}
}
