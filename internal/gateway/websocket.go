package gateway

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
)

type WebsocketGateway struct {
	Port string
}

func (wg *WebsocketGateway) Name() string {
	return "Websocket"
}

func (wg *WebsocketGateway) Start(aiAgent *agent.Agent) error {
	// Map the route to your existing handler
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		HandleConnections(w, r, aiAgent)
	})

	// Start the server
	log.Printf("Websocket server listening on port %s", wg.Port)
	return http.ListenAndServe(":"+wg.Port, nil)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// HandleConnections only relies on the agent Engine now
func HandleConnections(w http.ResponseWriter, r *http.Request, aiAgent *agent.Agent) {
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

		// Define the callback to stream chunks to the client
		streamFunc := func(chunk string) {
			// You might want to wrap this in a JSON struct like {"type": "stream", "chunk": chunk}
			conn.WriteMessage(messageType, []byte(chunk))
		}

		// Delegate ALL logic to the agent engine
		finalPayload, err := aiAgent.Process(userPrompt, streamFunc)
		if err != nil {
			log.Println("Engine processing error:", err)
			errorPayload := agent.AgentResponse{
				Category: "ERROR",
				Error:    "Internal engine timeout or failure",
			}
			errBytes, _ := json.Marshal(errorPayload)
			conn.WriteMessage(messageType, errBytes)
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
