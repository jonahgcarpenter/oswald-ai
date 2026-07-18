package websocket

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

// Name returns the human-readable gateway name.
func (wg *Gateway) Name() string {
	return "Websocket"
}

// Start initializes the HTTP server and registers the WebSocket handler.
func (wg *Gateway) Start(b *broker.Broker) error {
	log := wg.log()
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	})

	log.Info("gateway.listen", "websocket gateway listening", config.F("port", wg.Port))
	return http.ListenAndServe(":"+wg.Port, nil)
}

// handleConnections accepts WebSocket connections and routes prompts to the broker.
func (wg *Gateway) handleConnections(w http.ResponseWriter, r *http.Request, b *broker.Broker) {
	log := wg.log()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warn("gateway.connection.upgrade_failed", "websocket upgrade failed", config.ErrorField(err))
		return
	}
	defer conn.Close()

	// remoteAddr is used as the fallback identity for clients that send plain text.
	remoteAddr := conn.RemoteAddr().String()

	for {
		requestID := config.NewRequestID()
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Debug("gateway.connection.closed", "websocket connection closed", config.F("chat_id", remoteAddr), config.ErrorField(err))
			break
		}

		// Attempt to decode a structured IncomingMessage. Fall back to treating
		// the raw bytes as a plain-text prompt (legacy behaviour) so existing
		// clients keep working without modification.
		var userPrompt, userID, displayName string
		var userImages []llm.InputImage
		var userUnsupported []string
		var incoming IncomingMessage
		if jsonErr := json.Unmarshal(message, &incoming); jsonErr == nil && (incoming.Prompt != "" || len(incoming.Images) > 0 || incoming.UserID != "" || incoming.DisplayName != "") {
			userPrompt = incoming.Prompt
			userID = incoming.UserID
			displayName = incoming.DisplayName
			images, unsupported := wg.decodeIncomingImages(incoming.Images)
			userImages = images
			userUnsupported = unsupported
			if len(incoming.Images) > 0 {
				log.Info("gateway.attachment.processed", "processed websocket attachments",
					config.F("request_id", requestID),
					config.F("chat_id", remoteAddr),
					config.F("accepted_count", len(images)),
					config.F("downgraded_count", len(unsupported)),
					config.F("declared_format_count", len(incoming.Images)),
				)
			}
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
		normalizedUserID, normErr := accountlinking.NormalizeIdentifier("websocket", userID)
		if normErr != nil {
			errorPayload := agent.AgentResponse{Error: normErr.Error()}
			errBytes, _ := json.Marshal(errorPayload)
			conn.WriteMessage(messageType, errBytes) // nolint: errcheck
			continue
		}
		sessionKey := "websocket:" + sessionIdentity

		canonicalUserID, err := wg.Links.EnsureAccount("websocket", normalizedUserID, displayName)
		if err != nil {
			log.Error("gateway.account.resolve_failed", "failed to resolve websocket account",
				config.F("request_id", requestID),
				config.F("session_id", sessionKey),
				config.F("user_id", normalizedUserID),
				config.ErrorField(err),
			)
			errorPayload := agent.AgentResponse{Error: "Failed to resolve account identity"}
			errBytes, _ := json.Marshal(errorPayload)
			conn.WriteMessage(messageType, errBytes) // nolint: errcheck
			continue
		}

		firstChunk := true
		streamFunc := func(chunk agent.StreamChunk) {
			if firstChunk {
				log.Debug("gateway.stream.started", "started websocket stream", config.F("request_id", requestID), config.F("stream_type", string(chunk.Type)))
				firstChunk = false
			}
			chunkBytes, err := json.Marshal(chunk)
			if err != nil {
				log.Warn("gateway.stream.marshal_failed", "failed to marshal websocket stream chunk", config.F("request_id", requestID), config.F("status", "degraded"), config.ErrorField(err))
				return
			}
			conn.WriteMessage(messageType, chunkBytes) // nolint: errcheck
		}

		gatewayruntime.Execute(gatewayruntime.Request{
			RequestID: requestID,
			ChatID:    sessionKey,
			Principal: identity.Principal{
				CanonicalUserID: canonicalUserID,
				Gateway:         "websocket",
				ExternalID:      normalizedUserID,
				Assurance:       identity.AssuranceSelfAsserted,
			},
			DisplayName: displayName,
			SessionKey:  sessionKey,
			IsDirect:    true,
			IsMention:   true,
			Text:        userPrompt,
			Images:      userImages,
			Unsupported: userUnsupported,
			StreamFunc:  streamFunc,
		}, wg.runtimeDependencies(b), &runtimeResponder{conn: conn, messageType: messageType})
	}
}

func (wg *Gateway) runtimeDependencies(b *broker.Broker) gatewayruntime.Dependencies {
	deps := wg.Runtime
	deps.Broker = b
	if deps.Access == nil {
		deps.Access = wg.Links
	}
	if deps.Log == nil {
		deps.Log = wg.Log
	}
	return deps
}

func (wg *Gateway) decodeIncomingImages(images []IncomingImage) ([]llm.InputImage, []string) {
	if len(images) == 0 {
		return nil, nil
	}

	validated := make([]llm.InputImage, 0, len(images))
	unsupported := make([]string, 0)
	for _, image := range images {
		if len(validated) >= media.MaxImagesPerRequest {
			unsupported = append(unsupported, media.AttachmentLabel(image.Source, image.MimeType))
			continue
		}
		result, err := normalizeIncomingImage(image)
		if err != nil {
			unsupported = append(unsupported, media.AttachmentLabel(image.Source, image.MimeType))
			continue
		}
		wg.log().Debug(
			"gateway.attachment.normalized",
			"normalized websocket attachment",
			config.F("source", image.Source),
			config.F("declared_mime", strings.TrimSpace(image.MimeType)),
			config.F("detected_mime", result.DetectedMIME),
			config.F("normalized_mime", result.Image.MimeType),
			config.F("content_chars", decodedLen(image.Data)),
			config.F("original_width", result.OriginalWidth),
			config.F("original_height", result.OriginalHeight),
			config.F("width", result.Width),
			config.F("height", result.Height),
			config.F("is_resized", result.WasResized),
			config.F("normalized_bytes", result.NormalizedBytes),
			config.F("base64_chars", result.Base64Chars),
			config.F("preserved_alpha", result.PreservedAlpha),
			config.F("used_declared_mime", result.UsedDeclaredMIME),
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

// truncate returns s shortened to at most max runes, appending "..." if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
