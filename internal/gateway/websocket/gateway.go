package websocket

import (
	"encoding/json"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/accountlink"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

// Name returns the human-readable gateway name.
func (wg *Gateway) Name() string {
	return "Websocket"
}

// Start initializes the HTTP server and registers the WebSocket handler.
func (wg *Gateway) Start(b *broker.Broker) error {
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	})

	wg.Log.Info("WebSocket server listening on port %s", wg.Port)
	return http.ListenAndServe(":"+wg.Port, nil)
}

// handleConnections accepts WebSocket connections and routes prompts to the broker.
func (wg *Gateway) handleConnections(w http.ResponseWriter, r *http.Request, b *broker.Broker) {
	log := wg.Log
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warn("WebSocket upgrade failed: %v", err)
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
		var userPrompt, userID, displayName string
		var userImages []ollama.InputImage
		var incoming IncomingMessage
		if jsonErr := json.Unmarshal(message, &incoming); jsonErr == nil && (incoming.Prompt != "" || len(incoming.Images) > 0 || incoming.UserID != "" || incoming.DisplayName != "") {
			userPrompt = incoming.Prompt
			userID = incoming.UserID
			displayName = incoming.DisplayName
			images, unsupported := decodeIncomingImages(incoming.Images)
			userImages = images
			if len(incoming.Images) > 0 {
				log.Debug("WebSocket attachments: accepted=%d downgraded=%d", len(images), len(unsupported))
			}
			userPrompt = media.AugmentPromptWithUnsupportedFiles(userPrompt, unsupported)
		} else {
			userPrompt = string(message)
			userID = remoteAddr
		}

		// Build the session key from the gateway identity while keeping
		// persistent memory keyed separately by canonical user ID.
		sessionIdentity := userID
		if sessionIdentity == "" {
			sessionIdentity = remoteAddr
			userID = remoteAddr
		}
		normalizedUserID, normErr := accountlink.NormalizeIdentifier("websocket", userID)
		if normErr != nil {
			errorPayload := agent.AgentResponse{Error: normErr.Error()}
			errBytes, _ := json.Marshal(errorPayload)
			conn.WriteMessage(messageType, errBytes) // nolint: errcheck
			continue
		}
		sessionKey := "websocket:" + sessionIdentity

		canonicalUserID, err := wg.Links.EnsureAccount("websocket", normalizedUserID, displayName)
		if err != nil {
			log.Error("Websocket account resolution error: %v", err)
			errorPayload := agent.AgentResponse{Error: "Failed to resolve account identity"}
			errBytes, _ := json.Marshal(errorPayload)
			conn.WriteMessage(messageType, errBytes) // nolint: errcheck
			continue
		}

		if commandResponse, handled, commandErr := wg.Commands.Handle(canonicalUserID, userPrompt); handled {
			if commandErr != nil {
				log.Error("Websocket account command error: %v", commandErr)
				commandResponse = "Failed to process account linking command."
			}
			payload := agent.AgentResponse{Response: commandResponse}
			respBytes, _ := json.Marshal(payload)
			conn.WriteMessage(messageType, respBytes) // nolint: errcheck
			continue
		}

		log.Debug("Websocket request (session=%s sender=%s canonical=%s): %q", sessionKey, normalizedUserID, canonicalUserID, truncate(userPrompt, 100))

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
			SenderID:     canonicalUserID,
			DisplayName:  displayName,
			SessionKey:   sessionKey,
			Prompt:       userPrompt,
			Images:       userImages,
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

func decodeIncomingImages(images []IncomingImage) ([]ollama.InputImage, []string) {
	if len(images) == 0 {
		return nil, nil
	}

	validated := make([]ollama.InputImage, 0, len(images))
	unsupported := make([]string, 0)
	for _, image := range images {
		if len(validated) >= media.MaxImagesPerRequest {
			unsupported = append(unsupported, media.AttachmentLabel(image.Source, image.MimeType))
			continue
		}
		inputImage, err := media.BuildInputImage(image.MimeType, image.Data, image.Source)
		if err != nil {
			unsupported = append(unsupported, media.AttachmentLabel(image.Source, image.MimeType))
			continue
		}
		validated = append(validated, inputImage)
	}
	return validated, unsupported
}

// truncate returns s shortened to at most max runes, appending "..." if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
