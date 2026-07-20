package websocket

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/websocketauth"
)

// Name returns the human-readable gateway name.
func (wg *Gateway) Name() string {
	return "Websocket"
}

// Start initializes the HTTP server and registers the WebSocket handler.
func (wg *Gateway) Start(b *broker.Broker) error {
	log := wg.log()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		wg.handleConnections(w, r, b)
	})
	mux.HandleFunc("/auth/device", wg.handleDeviceAuthorization)
	mux.HandleFunc("/auth/token", wg.handleToken)
	mux.HandleFunc("/auth/revoke", wg.handleRevoke)

	log.Info("gateway.listen", "websocket gateway listening", config.F("port", wg.Port))
	return http.ListenAndServe(":"+wg.Port, mux)
}

// handleConnections accepts WebSocket connections and routes prompts to the broker.
func (wg *Gateway) handleConnections(w http.ResponseWriter, r *http.Request, b *broker.Broker) {
	log := wg.log()
	if wg.Auth == nil {
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}
	authenticated, err := wg.Auth.Authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		log.Warn("gateway.authentication.failed", "websocket authentication failed", config.F("status", "rejected"))
		return
	}
	if !originAllowed(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		log.Warn("gateway.origin.rejected", "websocket origin rejected", config.F("status", "rejected"))
		return
	}
	normalizedUserID, err := accountlinking.NormalizeIdentifier("websocket", authenticated.Subject)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		log.Warn("gateway.authentication.subject_invalid", "websocket token subject is invalid", config.F("status", "rejected"))
		return
	}
	owner, ok, err := wg.Links.ResolveAccount("websocket", normalizedUserID)
	if err != nil || !ok || owner != authenticated.UserID {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		log.Warn("gateway.authentication.owner_invalid", "websocket client owner is unavailable", config.F("status", "rejected"))
		return
	}
	sessionKey := "websocket:" + normalizedUserID + ":" + authenticated.ClientID
	token := bearerToken(r)

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warn("gateway.connection.upgrade_failed", "websocket upgrade failed", config.ErrorField(err))
		return
	}
	defer conn.Close()
	tracked := &trackedConnection{conn: conn}
	wg.trackConnection(normalizedUserID, authenticated.ClientID, tracked)
	defer wg.untrackConnection(normalizedUserID, authenticated.ClientID, tracked)
	expiryTimer := time.AfterFunc(time.Until(authenticated.ExpiresAt), func() {
		log.Debug("gateway.authentication.expired", "websocket authentication expired", config.F("session_id", sessionKey))
		tracked.closeWithReason("authentication expired")
	})
	defer expiryTimer.Stop()

	remoteAddr := conn.RemoteAddr().String()
	principal := identity.Principal{CanonicalUserID: owner, Gateway: "websocket", ExternalID: normalizedUserID, Assurance: identity.AssuranceWebSocketSignedToken}
	if completion, completeErr := wg.Auth.CompleteBootstrapOnAdminConnection(r.Context(), owner); completeErr != nil {
		log.Warn("gateway.bootstrap.complete_failed", "failed to complete websocket bootstrap", config.ErrorField(completeErr), config.F("status", "degraded"))
	} else if completion != nil {
		wg.closeClient(completion.ClientID, "bootstrap completed")
	}

	for {
		requestID := config.NewRequestID()
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Debug("gateway.connection.closed", "websocket connection closed", config.F("chat_id", remoteAddr), config.ErrorField(err))
			break
		}
		fresh, authErr := wg.Auth.VerifyAccess(r.Context(), token)
		currentOwner, ownerOK, resolveErr := wg.Links.ResolveAccount("websocket", normalizedUserID)
		if authErr != nil || resolveErr != nil || !ownerOK || fresh.ClientID != authenticated.ClientID || fresh.Subject != normalizedUserID || fresh.UserID != currentOwner {
			log.Warn("gateway.authentication.revalidation_failed", "websocket client authorization is no longer valid", config.F("request_id", requestID), config.F("session_id", sessionKey), config.F("status", "rejected"))
			tracked.closeWithReason("authorization revoked")
			return
		}
		principal.CanonicalUserID = currentOwner

		// Attempt to decode a structured IncomingMessage. Plain-text prompts keep
		// working, but identity is always bound by the handshake token.
		var userPrompt string
		displayName := fresh.DisplayName
		var userImages []llm.InputImage
		var userUnsupported []string
		var incoming IncomingMessage
		if jsonErr := json.Unmarshal(message, &incoming); jsonErr == nil && (incoming.Prompt != "" || len(incoming.Images) > 0 || incoming.UserID != "" || incoming.DisplayName != "") {
			userPrompt = incoming.Prompt
			if incoming.UserID != "" {
				claimedUserID, claimErr := accountlinking.NormalizeIdentifier("websocket", incoming.UserID)
				if claimErr != nil || claimedUserID != normalizedUserID {
					log.Warn("gateway.authentication.identity_mismatch", "websocket message attempted to change authenticated identity", config.F("request_id", requestID), config.F("status", "rejected"))
					_ = conn.WriteControl(gorilla.CloseMessage, gorilla.FormatCloseMessage(gorilla.ClosePolicyViolation, "message identity does not match authenticated subject"), time.Now().Add(time.Second))
					return
				}
			}
			if displayName == "" {
				displayName = incoming.DisplayName
			}
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
			tracked.writeMessage(messageType, chunkBytes) // nolint: errcheck
		}

		gatewayruntime.Execute(gatewayruntime.Request{
			RequestID:   requestID,
			ChatID:      sessionKey,
			Principal:   principal,
			DisplayName: displayName,
			SessionKey:  sessionKey,
			ClientID:    authenticated.ClientID,
			IsDirect:    true,
			IsMention:   true,
			Text:        userPrompt,
			Images:      userImages,
			Unsupported: userUnsupported,
			StreamFunc:  streamFunc,
		}, wg.runtimeDependencies(b), &runtimeResponder{conn: tracked, messageType: messageType})
	}
}

func (wg *Gateway) trackConnection(subject, clientID string, conn *trackedConnection) {
	wg.connectionsMu.Lock()
	if wg.connections == nil {
		wg.connections = make(map[string]map[*trackedConnection]struct{})
	}
	if wg.clients == nil {
		wg.clients = make(map[string]map[*trackedConnection]struct{})
	}
	if wg.connections[subject] == nil {
		wg.connections[subject] = make(map[*trackedConnection]struct{})
	}
	if wg.clients[clientID] == nil {
		wg.clients[clientID] = make(map[*trackedConnection]struct{})
	}
	wg.connections[subject][conn] = struct{}{}
	wg.clients[clientID][conn] = struct{}{}
	wg.connectionsMu.Unlock()
}

func (wg *Gateway) untrackConnection(subject, clientID string, conn *trackedConnection) {
	wg.connectionsMu.Lock()
	delete(wg.connections[subject], conn)
	delete(wg.clients[clientID], conn)
	if len(wg.connections[subject]) == 0 {
		delete(wg.connections, subject)
	}
	if len(wg.clients[clientID]) == 0 {
		delete(wg.clients, clientID)
	}
	wg.connectionsMu.Unlock()
}

func (wg *Gateway) closeClient(clientID, reason string) {
	wg.connectionsMu.Lock()
	connections := make([]*trackedConnection, 0, len(wg.clients[clientID]))
	for conn := range wg.clients[clientID] {
		connections = append(connections, conn)
	}
	delete(wg.clients, clientID)
	wg.connectionsMu.Unlock()
	for _, conn := range connections {
		conn.closeWithReason(reason)
	}
}

func (wg *Gateway) handleDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAuthError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	var request struct {
		ClientName string `json:"client_name"`
	}
	if err := decodeAuthJSON(r, &request); err != nil {
		writeAuthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	device, err := wg.Auth.RequestDevice(r.Context(), request.ClientName)
	if err != nil {
		writeAuthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"device_code": device.DeviceCode,
		"user_code":   device.UserCode,
		"expires_in":  int(time.Until(device.ExpiresAt).Seconds()),
		"interval":    int(device.PollInterval.Seconds()),
	})
}

func (wg *Gateway) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAuthError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	var request struct {
		GrantType    string `json:"grant_type"`
		DeviceCode   string `json:"device_code"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeAuthJSON(r, &request); err != nil {
		writeAuthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var pair websocketauth.TokenPair
	var err error
	switch request.GrantType {
	case "urn:ietf:params:oauth:grant-type:device_code", "device_code":
		pair, err = wg.Auth.PollDevice(r.Context(), request.DeviceCode)
	case "refresh_token":
		pair, err = wg.Auth.Refresh(r.Context(), request.RefreshToken)
	default:
		writeAuthError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	if err != nil {
		status := http.StatusBadRequest
		code := "invalid_grant"
		switch {
		case errors.Is(err, websocketauth.ErrAuthorizationPending):
			code = "authorization_pending"
		case errors.Is(err, websocketauth.ErrSlowDown):
			code = "slow_down"
		case errors.Is(err, websocketauth.ErrExpired):
			code = "expired_token"
		case !errors.Is(err, websocketauth.ErrInvalidGrant):
			status = http.StatusInternalServerError
			code = "server_error"
		}
		writeAuthError(w, status, code)
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"access_token":  pair.AccessToken,
		"token_type":    "Bearer",
		"expires_in":    int(time.Until(pair.ExpiresAt).Seconds()),
		"refresh_token": pair.RefreshToken,
		"client_id":     pair.ClientID,
	})
}

func (wg *Gateway) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAuthError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	var request struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeAuthJSON(r, &request); err != nil {
		writeAuthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	clientID, err := wg.Auth.RevokeRefreshClient(r.Context(), request.RefreshToken)
	if err != nil && !errors.Is(err, websocketauth.ErrInvalidGrant) {
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if err == nil {
		wg.closeClient(clientID, "client revoked")
	}
	w.WriteHeader(http.StatusOK)
}

func decodeAuthJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeAuthError(w http.ResponseWriter, status int, code string) {
	writeAuthJSON(w, status, map[string]string{"error": code})
}

func writeAuthJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func bearerToken(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
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
