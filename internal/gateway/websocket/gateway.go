package websocket

import (
	"encoding/json"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Name returns the human-readable gateway name.
func (wg *Gateway) Name() string {
	return "Websocket"
}

// Start initializes the HTTP server and registers the WebSocket handler.
func (wg *Gateway) Start(aiAgent *agent.Agent) error {
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleConnections(w, r, aiAgent, wg.Log)
	})

	wg.Log.Info("Websocket server listening on port %s", wg.Port)
	return http.ListenAndServe(":"+wg.Port, nil)
}

// handleConnections accepts WebSocket connections and routes prompts to the agent.
func handleConnections(w http.ResponseWriter, r *http.Request, aiAgent *agent.Agent, log *config.Logger) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error("Upgrader error: %v", err)
		return
	}
	defer conn.Close()

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Warn("Read error: %v", err)
			break
		}

		userPrompt := string(message)
		log.Info("Websocket request: %q", truncate(userPrompt, 100))

		firstChunk := true
		streamFunc := func(chunk agent.StreamChunk) {
			if firstChunk {
				log.Debug("Websocket: streaming response started (type=%s)", chunk.Type)
				firstChunk = false
			}
			chunkBytes, err := json.Marshal(chunk)
			if err != nil {
				log.Warn("Websocket: failed to marshal stream chunk: %v", err)
				return
			}
			conn.WriteMessage(messageType, chunkBytes) // nolint: errcheck
		}

		finalPayload, err := aiAgent.Process(userPrompt, streamFunc)
		if err != nil {
			log.Error("Engine processing error: %v", err)
			errorPayload := agent.AgentResponse{Error: "Internal engine timeout or failure"}
			errBytes, _ := json.Marshal(errorPayload)
			conn.WriteMessage(messageType, errBytes) // nolint: errcheck
			continue
		}

		jsonBytes, err := json.Marshal(finalPayload)
		if err != nil {
			log.Error("Failed to marshal JSON payload: %v", err)
			continue
		}

		log.Debug("Websocket: sending final payload (%d bytes, model=%s)", len(jsonBytes), finalPayload.Model)
		err = conn.WriteMessage(messageType, jsonBytes)
		if err != nil {
			log.Warn("Write error: %v", err)
			break
		}
	}
}

// truncate returns s shortened to at most max runes, appending "..." if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
