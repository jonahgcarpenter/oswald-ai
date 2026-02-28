package ws

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/router"
)

// Upgrades HTTP connection to WebSocket
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func HandleConnections(w http.ResponseWriter, r *http.Request, client *ollama.Client, routerModel string) {
	// Upgrade init request to Websocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrader error:", err)
		return
	}
	// Close if err
	defer conn.Close()

	fmt.Println("New client connected")

	// Loop for read and write
	for {
		// Read
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error", err)
			break
		}

		// Print message
		userPrompt := string(message)
		fmt.Printf("Received: %s\n", userPrompt)

		// Create a context with a strict timeout for the fast router model
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		// Ask the triage model to classify the prompt
		decision, err := router.DetermineRoute(ctx, client, routerModel, userPrompt)
		cancel()

		if err != nil {
			log.Println("Routing error:", err)
			conn.WriteMessage(messageType, []byte("Error: Failed to route the request."))
			continue
		}

		// Log the routing decision to your terminal
		fmt.Printf("Routed to [%s] model because: %s\n", decision.Category, decision.Reason)

		// TODO: The Dispatcher Step (Switch statement for different router decisions)

		// For now, let's just echo the router's JSON decision back to the client for testing
		replyMessage := fmt.Sprintf("Router Decision -> Category: %s | Reason: %s", decision.Category, decision.Reason)

		// Return message to client
		err = conn.WriteMessage(messageType, []byte(replyMessage))
		if err != nil {
			log.Println("Write error:", err)
			break
		}
	}
}
