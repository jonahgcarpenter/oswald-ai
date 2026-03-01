package websocket

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// HandleConnections only relies on the agent Engine now
func HandleConnections(w http.ResponseWriter, r *http.Request, engine *agent.Engine) {
	// Upgrade init request to Websocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrader error:", err)
		return
	}
	// Close if err
	defer conn.Close()

	// Loop for read and write
	for {
		// Read
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error", err)
			break
		}

		userPrompt := string(message)

		// Delegate ALL logic to the agent engine
		finalPayload, err := engine.Process(userPrompt)
		if err != nil {
			log.Println("Engine processing error:", err)
			conn.WriteMessage(messageType, []byte("Error: Failed to process request."))
			continue
		}

		// Marshal the struct into a JSON byte array
		jsonBytes, err := json.Marshal(finalPayload)
		if err != nil {
			log.Println("Failed to marshal JSON payload:", err)
			continue
		}

		// Return the structured JSON to the client
		err = conn.WriteMessage(messageType, jsonBytes)
		if err != nil {
			log.Println("Write error:", err)
			break
		}
	}
}
