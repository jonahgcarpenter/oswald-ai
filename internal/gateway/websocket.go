package gateway

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type WebsocketGateway struct {
	Port string
	Log  *config.Logger
}

func (wg *WebsocketGateway) Name() string {
	return "Websocket"
}

func (wg *WebsocketGateway) Start(aiAgent *agent.Agent) error {
	// Map the route to your existing handler
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		HandleConnections(w, r, aiAgent, wg.Log)
	})

	// Start the server
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

// HandleConnections only relies on the agent Engine now
func HandleConnections(w http.ResponseWriter, r *http.Request, aiAgent *agent.Agent, log *config.Logger) {
	// Upgrade init request to Websocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error("Upgrader error: %v", err)
		return
	}
	// Close if err
	defer conn.Close()

	// Loop for read and write
	for {
		// Read
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Warn("Read error: %v", err)
			break
		}

		userPrompt := string(message)
		log.Info("Websocket request: %q", truncate(userPrompt, 100))

		// Define the callback to stream chunks to the client.
		// Log once on the very first chunk so we know streaming has started.
		firstChunk := true
		streamFunc := func(chunk string) {
			if firstChunk {
				log.Debug("Websocket: streaming response started")
				firstChunk = false
			}
			conn.WriteMessage(messageType, []byte(chunk)) // nolint: errcheck
		}

		// Delegate ALL logic to the agent engine
		finalPayload, err := aiAgent.Process(userPrompt, streamFunc)
		if err != nil {
			log.Error("Engine processing error: %v", err)
			errorPayload := agent.AgentResponse{
				Category: "ERROR",
				Error:    "Internal engine timeout or failure",
			}
			errBytes, _ := json.Marshal(errorPayload)
			conn.WriteMessage(messageType, errBytes) // nolint: errcheck
			continue
		}

		// Marshal the struct into a JSON byte array
		jsonBytes, err := json.Marshal(finalPayload)
		if err != nil {
			log.Error("Failed to marshal JSON payload: %v", err)
			continue
		}

		// Return the structured JSON to the client
		log.Debug("Websocket: sending final payload (%d bytes, model: %s)", len(jsonBytes), finalPayload.Model)
		err = conn.WriteMessage(messageType, jsonBytes)
		if err != nil {
			log.Warn("Write error: %v", err)
			break
		}
	}
}
