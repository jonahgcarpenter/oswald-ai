package websocket

import (
	"encoding/json"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/broker"
) // Name returns the human-readable gateway name.
func (wg *Gateway) Name() string {
	return "Websocket"
}

// Start initializes the HTTP server and registers the WebSocket handler.
func (wg *Gateway) Start(b *broker.Broker) error {
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleConnections(w, r, b, wg.Log)
	})

	wg.Log.Info("Websocket server listening on port %s", wg.Port)
	return http.ListenAndServe(":"+wg.Port, nil)
}

// handleConnections accepts WebSocket connections and routes prompts to the broker.
func handleConnections(w http.ResponseWriter, r *http.Request, b *broker.Broker, log *config.Logger) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error("Upgrader error: %v", err)
		return
	}
	defer conn.Close()

	// remoteAddr is used as the fallback identity for clients that send plain text.
	remoteAddr := conn.RemoteAddr().String()

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Debug("Websocket connection closed: %v", err)
			break
		}

		// Attempt to decode a structured IncomingMessage. Fall back to treating
		// the raw bytes as a plain-text prompt (legacy behaviour) so existing
		// clients keep working without modification.
		var userPrompt, userID string
		var incoming IncomingMessage
		if jsonErr := json.Unmarshal(message, &incoming); jsonErr == nil && incoming.Prompt != "" {
			userPrompt = incoming.Prompt
			userID = incoming.UserID
		} else {
			userPrompt = string(message)
			userID = remoteAddr
		}

		// Build the session key from the user ID for persistent per-user
		// conversation memory. Fall back to the remote address if no user ID
		// was provided so anonymous connections still get session isolation.
		sessionKey := userID
		if sessionKey == "" {
			sessionKey = remoteAddr
			userID = remoteAddr
		}

		log.Debug("Websocket request (session=%s sender=%s): %q", sessionKey, userID, truncate(userPrompt, 100))

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

		req := &broker.Request{
			Channel:      "websocket",
			ChatID:       sessionKey,
			SenderID:     userID,
			SessionKey:   sessionKey,
			Prompt:       userPrompt,
			StreamFunc:   streamFunc,
			ResponseChan: make(chan broker.Result, 1),
		}
		b.Submit(req)
		result := <-req.ResponseChan

		if result.Err != nil {
			log.Error("Engine processing error: %v", result.Err)
			errorPayload := agent.AgentResponse{Error: "Internal engine timeout or failure"}
			errBytes, _ := json.Marshal(errorPayload)
			conn.WriteMessage(messageType, errBytes) // nolint: errcheck
			continue
		}

		jsonBytes, err := json.Marshal(result.Response)
		if err != nil {
			log.Error("Failed to marshal JSON payload: %v", err)
			continue
		}

		log.Debug("Websocket: sending final payload (%d bytes, model=%s)", len(jsonBytes), result.Response.Model)
		if err = conn.WriteMessage(messageType, jsonBytes); err != nil {
			log.Debug("Websocket write error: %v", err)
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
