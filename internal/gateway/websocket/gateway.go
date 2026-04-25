package websocket

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

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
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	})
	if wg.Metrics != nil {
		mux.Handle("/metrics", wg.Metrics.Handler())
	}

	wg.Log.Info("WebSocket server listening on port %s", wg.Port)
	return http.ListenAndServe(":"+wg.Port, mux)
}

// handleConnections accepts WebSocket connections and routes prompts to the broker.
func (wg *Gateway) handleConnections(w http.ResponseWriter, r *http.Request, b *broker.Broker) {
	log := wg.Log
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warn("WebSocket upgrade failed: %v", err)
		wg.Metrics.ObserveError("websocket", "upgrade")
		return
	}
	wg.Metrics.IncWebsocketConnections()
	defer wg.Metrics.DecWebsocketConnections()
	defer conn.Close()

	// remoteAddr is used as the fallback identity for clients that send plain text.
	remoteAddr := conn.RemoteAddr().String()

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Debug("Websocket connection closed: %v", err)
			wg.Metrics.ObserveGatewayIgnored("websocket", "connection_closed")
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
			images, unsupported := wg.decodeIncomingImages(incoming.Images)
			userImages = images
			if len(incoming.Images) > 0 {
				log.Debug("WebSocket attachments: chat=%s accepted=%d downgraded=%d declared_formats=%q", remoteAddr, len(images), len(unsupported), attachmentFormats(incoming.Images))
			}
			userPrompt = media.AugmentPromptWithUnsupportedFiles(userPrompt, unsupported)
		} else {
			userPrompt = string(message)
			userID = remoteAddr
		}
		isCommand := strings.HasPrefix(strings.TrimSpace(userPrompt), "/connect") || strings.HasPrefix(strings.TrimSpace(userPrompt), "/disconnect")
		wg.Metrics.ObserveGatewayReceived("websocket", requestKind(userPrompt, len(userImages), isCommand))

		// Build the session key from the gateway identity while keeping
		// persistent memory keyed separately by canonical user ID.
		sessionIdentity := userID
		if sessionIdentity == "" {
			sessionIdentity = remoteAddr
			userID = remoteAddr
		}
		normalizedUserID, normErr := accountlink.NormalizeIdentifier("websocket", userID)
		if normErr != nil {
			wg.Metrics.ObserveGatewaySent("websocket", "error")
			wg.Metrics.ObserveGatewaySendFailure("websocket", "bad_identity")
			wg.Metrics.ObserveError("websocket", "bad_identity")
			errorPayload := agent.AgentResponse{Error: normErr.Error()}
			errBytes, _ := json.Marshal(errorPayload)
			writeStartedAt := time.Now()
			if err := conn.WriteMessage(messageType, errBytes); err != nil { // nolint: govet
				wg.Metrics.ObserveWebsocketWriteFailure()
			}
			wg.Metrics.ObserveGatewaySendDuration("websocket", "error", time.Since(writeStartedAt))
			continue
		}
		sessionKey := "websocket:" + sessionIdentity

		canonicalUserID, err := wg.Links.EnsureAccount("websocket", normalizedUserID, displayName)
		if err != nil {
			log.Error("Websocket account resolution error: %v", err)
			wg.Metrics.ObserveGatewaySent("websocket", "error")
			wg.Metrics.ObserveGatewaySendFailure("websocket", "account_resolution")
			wg.Metrics.ObserveError("websocket", "account_resolution")
			errorPayload := agent.AgentResponse{Error: "Failed to resolve account identity"}
			errBytes, _ := json.Marshal(errorPayload)
			writeStartedAt := time.Now()
			if err := conn.WriteMessage(messageType, errBytes); err != nil { // nolint: govet
				wg.Metrics.ObserveWebsocketWriteFailure()
			}
			wg.Metrics.ObserveGatewaySendDuration("websocket", "error", time.Since(writeStartedAt))
			continue
		}

		if commandResponse, handled, commandErr := wg.Commands.Handle(canonicalUserID, userPrompt); handled {
			if commandErr != nil {
				log.Error("Websocket account command error: %v", commandErr)
				wg.Metrics.ObserveError("websocket", "command")
				commandResponse = "Failed to process account linking command."
			}
			wg.Metrics.ObserveGatewaySent("websocket", "success")
			payload := agent.AgentResponse{Response: commandResponse}
			respBytes, _ := json.Marshal(payload)
			writeStartedAt := time.Now()
			if err := conn.WriteMessage(messageType, respBytes); err != nil { // nolint: govet
				wg.Metrics.ObserveWebsocketWriteFailure()
				wg.Metrics.ObserveGatewaySendFailure("websocket", "write")
			}
			wg.Metrics.ObserveGatewaySendDuration("websocket", "success", time.Since(writeStartedAt))
			continue
		}

		log.Debug("Websocket request (session=%s sender=%s canonical=%s): %q", sessionKey, normalizedUserID, canonicalUserID, truncate(userPrompt, 100))

		firstChunk := true
		streamFunc := func(chunk agent.StreamChunk) {
			if firstChunk {
				log.Debug("Websocket: streaming response started (type=%s)", chunk.Type)
				firstChunk = false
			}
			wg.Metrics.ObserveWebsocketStreamChunk(string(chunk.Type))
			chunkBytes, err := json.Marshal(chunk)
			if err != nil {
				log.Warn("Websocket: failed to marshal stream chunk: %v", err)
				wg.Metrics.ObserveError("websocket", "stream_marshal")
				return
			}
			if err := conn.WriteMessage(messageType, chunkBytes); err != nil { // nolint: govet
				wg.Metrics.ObserveWebsocketWriteFailure()
				wg.Metrics.ObserveGatewaySendFailure("websocket", "stream_write")
			}
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
			wg.Metrics.ObserveGatewaySent("websocket", "error")
			wg.Metrics.ObserveGatewaySendFailure("websocket", "internal")
			wg.Metrics.ObserveError("websocket", "internal")
			errorPayload := agent.AgentResponse{Error: "Internal engine timeout or failure"}
			errBytes, _ := json.Marshal(errorPayload)
			writeStartedAt := time.Now()
			if err := conn.WriteMessage(messageType, errBytes); err != nil { // nolint: govet
				wg.Metrics.ObserveWebsocketWriteFailure()
			}
			wg.Metrics.ObserveGatewaySendDuration("websocket", "error", time.Since(writeStartedAt))
			continue
		}

		jsonBytes, err := json.Marshal(result.Response)
		if err != nil {
			log.Error("Failed to marshal JSON payload: %v", err)
			wg.Metrics.ObserveError("websocket", "marshal")
			continue
		}

		log.Debug("Websocket: sending final payload (%d bytes, model=%s)", len(jsonBytes), result.Response.Model)
		writeStartedAt := time.Now()
		if err = conn.WriteMessage(messageType, jsonBytes); err != nil {
			log.Debug("Websocket write error: %v", err)
			wg.Metrics.ObserveGatewaySent("websocket", "error")
			wg.Metrics.ObserveGatewaySendFailure("websocket", "write")
			wg.Metrics.ObserveGatewaySendDuration("websocket", "error", time.Since(writeStartedAt))
			wg.Metrics.ObserveWebsocketWriteFailure()
			break
		}
		wg.Metrics.ObserveGatewaySent("websocket", "success")
		wg.Metrics.ObserveGatewaySendDuration("websocket", "success", time.Since(writeStartedAt))
	}
}

func (wg *Gateway) decodeIncomingImages(images []IncomingImage) ([]ollama.InputImage, []string) {
	if len(images) == 0 {
		return nil, nil
	}

	validated := make([]ollama.InputImage, 0, len(images))
	unsupported := make([]string, 0)
	for _, image := range images {
		wg.Metrics.ObserveAttachmentDeclaredMIME("websocket", strings.TrimSpace(image.MimeType))
		if len(validated) >= media.MaxImagesPerRequest {
			wg.Metrics.ObserveUnsupportedAttachment("websocket", "too_many")
			unsupported = append(unsupported, media.AttachmentLabel(image.Source, image.MimeType))
			continue
		}
		result, err := normalizeIncomingImage(image)
		if err != nil {
			wg.Metrics.ObserveUnsupportedAttachment("websocket", classifyIncomingImageError(err))
			unsupported = append(unsupported, media.AttachmentLabel(image.Source, image.MimeType))
			continue
		}
		wg.Metrics.ObserveAttachmentDetectedMIME("websocket", result.Image.MimeType)
		wg.Log.Debug(
			"WebSocket attachment normalized: source=%q declared_mime=%q detected_mime=%q normalized_mime=%q bytes=%d width=%d height=%d preserved_alpha=%t used_declared_mime=%t",
			image.Source,
			strings.TrimSpace(image.MimeType),
			result.DetectedMIME,
			result.Image.MimeType,
			decodedLen(image.Data),
			result.Width,
			result.Height,
			result.PreservedAlpha,
			result.UsedDeclaredMIME,
		)
		validated = append(validated, result.Image)
	}
	return validated, unsupported
}

func normalizeIncomingImage(image IncomingImage) (media.NormalizationResult, error) {
	data, err := decodeIncomingImageData(image.Data)
	if err != nil {
		return media.NormalizationResult{}, err
	}
	return media.NormalizeInputImageFromBytes(nil, image.MimeType, data, image.Source)
}

func decodeIncomingImageData(encoded string) ([]byte, error) {
	payload := strings.TrimSpace(encoded)
	if payload == "" {
		return nil, fmt.Errorf("image payload is empty")
	}
	if comma := strings.Index(payload, ","); comma >= 0 && strings.HasPrefix(payload[:comma], "data:") {
		payload = payload[comma+1:]
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("image payload is not valid base64")
	}
	if len(decoded) > media.MaxImageBytes {
		return nil, fmt.Errorf("image payload exceeds %d bytes", media.MaxImageBytes)
	}
	return decoded, nil
}

func decodedLen(encoded string) int {
	decoded, err := decodeIncomingImageData(encoded)
	if err != nil {
		return 0
	}
	return len(decoded)
}

func attachmentFormats(images []IncomingImage) string {
	formats := make([]string, 0, len(images))
	for _, image := range images {
		format := strings.TrimSpace(image.MimeType)
		if format == "" {
			format = "unknown"
		}
		formats = append(formats, format)
	}
	return strings.Join(formats, ",")
}

func requestKind(prompt string, imageCount int, isCommand bool) string {
	if isCommand {
		return "command"
	}
	hasText := strings.TrimSpace(prompt) != ""
	switch {
	case hasText && imageCount > 0:
		return "text_and_image"
	case imageCount > 0:
		return "image_only"
	default:
		return "text_only"
	}
}

func classifyIncomingImageError(err error) string {
	if err == nil {
		return "validation_failed"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "base64"):
		return "invalid_base64"
	case strings.Contains(text, "exceeds"):
		return "too_large"
	case strings.Contains(text, "empty"):
		return "empty"
	default:
		return "validation_failed"
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
