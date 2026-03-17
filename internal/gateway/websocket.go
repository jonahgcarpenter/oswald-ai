package gateway

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// WebsocketGateway handles local WebSocket connections for testing and client access.
type WebsocketGateway struct {
	Port string
	Log  *config.Logger
}

func (wg *WebsocketGateway) Name() string {
	return "Websocket"
}

// Start initializes the HTTP server and registers the WebSocket handler.
// Blocks indefinitely while the server is running; returns on server error.
func (wg *WebsocketGateway) Start(aiAgent *agent.Agent) error {
	// Register WebSocket upgrade handler
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		HandleConnections(w, r, aiAgent, wg.Log)
	})

	// Start the HTTP server
	wg.Log.Info("Websocket server listening on port %s", wg.Port)
	return http.ListenAndServe(":"+wg.Port, nil)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// HandleConnections accepts WebSocket connections and routes incoming prompts to the agent.
// Streams partial responses as they arrive and sends final structured JSON when complete.
// Closes the connection on read error or client disconnect.
func HandleConnections(w http.ResponseWriter, r *http.Request, aiAgent *agent.Agent, log *config.Logger) {
	// Upgrade HTTP connection to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error("Upgrader error: %v", err)
		return
	}
	defer conn.Close()

	// Message loop
	for {
		// Read incoming message
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Warn("Read error: %v", err)
			break
		}

		userPrompt := string(message)
		log.Info("Websocket request: %q", truncate(userPrompt, 100))

		// Set up streaming callback for partial responses
		firstChunk := true
		streamFunc := func(chunk string) {
			if firstChunk {
				log.Debug("Websocket: streaming response started")
				firstChunk = false
			}
			conn.WriteMessage(messageType, []byte(chunk)) // nolint: errcheck
		}

		// Route prompt to agent for query generation and response
		finalPayload, err := aiAgent.Process(userPrompt, streamFunc)
		if err != nil {
			log.Error("Engine processing error: %v", err)
			errorPayload := agent.AgentResponse{
				Error: "Internal engine timeout or failure",
			}
			errBytes, _ := json.Marshal(errorPayload)
			conn.WriteMessage(messageType, errBytes) // nolint: errcheck
			continue
		}

		// Send final structured response as JSON
		jsonBytes, err := json.Marshal(finalPayload)
		if err != nil {
			log.Error("Failed to marshal JSON payload: %v", err)
			continue
		}

		log.Debug("Websocket: sending final payload (%d bytes, model=%s search=%v)", len(jsonBytes), finalPayload.Model, finalPayload.SearchSummary != "")
		err = conn.WriteMessage(messageType, jsonBytes)
		if err != nil {
			log.Warn("Write error: %v", err)
			break
		}
	}
}
