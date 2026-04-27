package imessage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accountlink"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

// Name returns the human-readable gateway name.
func (g *Gateway) Name() string {
	return "iMessage"
}

// Start initializes the BlueBubbles webhook listener.
func (g *Gateway) Start(b *broker.Broker) error {
	g.Broker = b
	log := g.Log.Server("gateway.imessage", config.F("gateway", "imessage"))
	if g.messageIndex == nil {
		g.messageIndex = make(map[string]messageContext)
	}
	if g.contactNames == nil {
		g.contactNames = make(map[string]contactNameCacheEntry)
	}

	path := strings.TrimSpace(g.WebhookPath)
	if path == "" {
		path = defaultWebhookPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, g.handleWebhook)

	log.Info("gateway.listen", "imessage gateway listening", config.F("port", g.Port), config.F("path", path))
	return http.ListenAndServe(":"+g.Port, mux)
}

// handleWebhook validates and dispatches incoming BlueBubbles webhook events.
func (g *Gateway) handleWebhook(w http.ResponseWriter, r *http.Request) {
	log := g.Log.Server("gateway.imessage", config.F("gateway", "imessage"))
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("gateway.webhook.read_failed", "failed to read imessage webhook body", config.ErrorField(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var event webhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		log.Warn("gateway.webhook.decode_failed", "failed to decode imessage webhook body", config.ErrorField(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	switch strings.TrimSpace(strings.ToLower(event.Type)) {
	case "new-message":
		if event.Data.IsFromMe {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !hasMessageContent(event.Data) {
			log.Debug("gateway.webhook.ignored", "ignored imessage webhook without content", config.F("message_guid", event.Data.GUID), config.F("associated_type", event.Data.AssociatedMessageType))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		go g.processIncomingMessage(event.Data)
		w.WriteHeader(http.StatusAccepted)
	case "typing-indicator":
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func hasMessageContent(msg webhookMessage) bool {
	return strings.TrimSpace(msg.Text) != "" || len(msg.Attachments) > 0
}

// processIncomingMessage normalizes an inbound iMessage and routes it to the broker.
func (g *Gateway) processIncomingMessage(msg webhookMessage) {
	log := g.Log.Server("gateway.imessage", config.F("gateway", "imessage"))
	requestID := config.NewRequestID()
	chat := msg.primaryChat()
	if chat.GUID == "" || strings.TrimSpace(msg.Handle.Address) == "" {
		log.Debug("gateway.webhook.ignored", "ignored imessage webhook with incomplete payload")
		return
	}
	images, unsupported := g.loadImages(msg.Attachments)
	if len(msg.Attachments) > 0 {
		log.Debug("gateway.attachment.processed", "processed imessage attachments", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("accepted_count", len(images)), config.F("downgraded_count", len(unsupported)), config.F("declared_format_count", len(msg.Attachments)))
	}
	if strings.TrimSpace(msg.Text) == "" && len(images) == 0 {
		if len(unsupported) == 0 {
			log.Debug("gateway.webhook.ignored", "ignored imessage webhook with incomplete payload")
			return
		}
	}

	prompt := media.AugmentPromptWithUnsupportedFiles(strings.TrimSpace(msg.Text), unsupported)
	if prompt == "" && len(images) == 0 {
		return
	}

	normalizedSenderID, err := accountlink.NormalizeIdentifier("imessage", msg.Handle.Address)
	if err != nil {
		log.Error("gateway.account.normalize_failed", "failed to normalize imessage account", config.F("request_id", requestID), config.ErrorField(err))
		return
	}
	displayName := normalizedSenderID
	if resolvedName, err := g.lookupContactDisplayName(normalizedSenderID); err != nil {
		log.Debug("gateway.contact_lookup.failed", "imessage contact lookup failed", config.F("request_id", requestID), config.F("user_id", normalizedSenderID), config.F("status", "degraded"), config.ErrorField(err))
	} else if resolvedName != "" {
		displayName = resolvedName
	}

	canonicalUserID, err := g.Links.EnsureAccount("imessage", normalizedSenderID, displayName)
	if err != nil {
		log.Error("gateway.account.resolve_failed", "failed to resolve imessage account", config.F("request_id", requestID), config.F("user_id", normalizedSenderID), config.ErrorField(err))
		return
	}

	sessionKey := g.sessionKey(chat, normalizedSenderID)
	prompt = strings.TrimSpace(prompt)
	replyGUID := msg.replyTargetGUID()
	isReplyToBot := false
	if replyGUID != "" {
		if replyCtx, ok := g.lookupMessage(replyGUID); ok {
			isReplyToBot = replyCtx.IsFromBot
			replyName := strings.TrimSpace(replyCtx.DisplayName)
			if replyName == "" && replyCtx.IsFromBot {
				replyName = "Oswald"
			}
			switch {
			case strings.TrimSpace(replyCtx.Text) != "" && replyName != "":
				log.Debug("gateway.reply_context.applied", "applied imessage reply context", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("is_bot_reply", replyCtx.IsFromBot))
				prompt = fmt.Sprintf("[Replying to %s: %q]\n%s", replyName, replyCtx.Text, prompt)
			case replyName != "":
				log.Debug("gateway.reply_context.applied", "imessage reply target unavailable", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("status", "degraded"))
				prompt = fmt.Sprintf("[Replying to %s's message, but it is unavailable]\n%s", replyName, prompt)
			default:
				log.Debug("gateway.reply_context.applied", "imessage reply target unavailable", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("status", "degraded"))
				prompt = fmt.Sprintf("[Replying to a message that is unavailable]\n%s", prompt)
			}
		} else {
			log.Debug("gateway.reply_context.applied", "imessage reply target missing from cache", config.F("request_id", requestID), config.F("status", "degraded"))
			prompt = fmt.Sprintf("[Replying to a message that is unavailable]\n%s", prompt)
		}
	}

	isAccountCommand := isAccountCommand(prompt)
	isGroup := chat.Style == chatStyleGroup || strings.Contains(chat.GUID, ";+;")
	selectedMessageGUID := ""
	if isGroup {
		selectedMessageGUID = msg.GUID
	}
	if isGroup && !isReplyToBot && !isAccountCommand && !mentionRE.MatchString(prompt) {
		g.rememberInboundMessage(msg, sessionKey, normalizedSenderID, displayName)
		return
	}
	if isGroup {
		prompt = strings.TrimSpace(mentionRE.ReplaceAllString(prompt, ""))
	}

	if err := g.startTyping(chat.GUID); err != nil {
		log.Debug("gateway.typing.start_failed", "failed to start imessage typing indicator", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("status", "degraded"), config.ErrorField(err))
	}

	if prompt == "" && len(images) == 0 {
		responseText := "What do you want idiot."
		messageGUID, err := g.sendTextReply(chat.GUID, responseText, selectedMessageGUID, 0)
		if err != nil {
			log.Error("gateway.response.failed", "failed to send imessage empty prompt response", config.F("request_id", requestID), config.ErrorField(err))
		} else {
			g.rememberBotMessage(messageGUID, sessionKey, chat.GUID, normalizedSenderID, responseText)
		}
		g.rememberInboundMessage(msg, sessionKey, normalizedSenderID, displayName)
		return
	}

	g.rememberInboundMessage(msg, sessionKey, normalizedSenderID, displayName)
	log.Debug("gateway.request.received", "received imessage request", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("session_id", sessionKey), config.F("user_id", canonicalUserID), config.F("image_count", len(images)), config.F("is_group", isGroup), config.F("is_reply", replyGUID != ""), config.F("prompt_chars", len(prompt)))

	if commandResponse, handled, commandErr := g.Commands.Handle(canonicalUserID, prompt); handled {
		if commandErr != nil {
			log.Error("gateway.command.failed", "imessage account command failed", config.F("request_id", requestID), config.F("user_id", canonicalUserID), config.ErrorField(commandErr))
			commandResponse = "Failed to process account linking command."
		}
		messageGUID, err := g.sendTextReply(chat.GUID, commandResponse, selectedMessageGUID, 0)
		if err != nil {
			log.Error("gateway.response.failed", "failed to send imessage command response", config.F("request_id", requestID), config.ErrorField(err))
		} else {
			g.rememberBotMessage(messageGUID, sessionKey, chat.GUID, normalizedSenderID, commandResponse)
		}
		return
	}

	req := &broker.Request{
		RequestID:    requestID,
		Channel:      "imessage",
		ChatID:       chat.GUID,
		SenderID:     canonicalUserID,
		DisplayName:  displayName,
		SessionKey:   sessionKey,
		Prompt:       prompt,
		Images:       images,
		StreamFunc:   nil,
		ResponseChan: make(chan broker.Result, 1),
	}
	g.Broker.Submit(req)
	result := <-req.ResponseChan

	responseText := ""
	if result.Err != nil {
		log.Error("gateway.response.failed", "imessage agent processing failed", config.F("request_id", requestID), config.ErrorField(result.Err))
		responseText = "Sorry, I encountered an internal error processing that."
	} else if result.Response != nil {
		responseText = strings.TrimSpace(result.Response.Response)
	}
	if responseText == "" {
		log.Debug("gateway.response.empty", "imessage agent returned empty response", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("status", "degraded"))
		return
	}

	messageGUID, err := g.sendTextReply(chat.GUID, responseText, selectedMessageGUID, 0)
	if err != nil {
		log.Error("gateway.send.failed", "imessage send failed", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.ErrorField(err))
		return
	}
	log.Debug("gateway.response.sent", "sent imessage response", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("response_chars", len(responseText)), config.F("status", "ok"))
	g.rememberBotMessage(messageGUID, sessionKey, chat.GUID, normalizedSenderID, responseText)
}

func (g *Gateway) lookupContactDisplayName(normalizedSenderID string) (string, error) {
	if normalizedSenderID == "" {
		return "", nil
	}

	if cachedName, ok := g.cachedContactDisplayName(normalizedSenderID); ok {
		return cachedName, nil
	}

	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/contact/query", g.BlueBubblesPassword)
	if err != nil {
		return "", err
	}

	payload, err := json.Marshal(contactQueryRequest{Addresses: []string{normalizedSenderID}})
	if err != nil {
		return "", fmt.Errorf("marshal BlueBubbles contact query: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build BlueBubbles contact query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("send BlueBubbles contact query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		g.Log.Server("gateway.imessage", config.F("gateway", "imessage")).Warn("gateway.contact_lookup.failed", "BlueBubbles contact query failed", config.F("user_id", normalizedSenderID), config.F("http_status", resp.StatusCode), config.F("status", "degraded"), config.F("body_preview", strings.TrimSpace(string(body))))
		return "", fmt.Errorf("BlueBubbles contact query failed with status %d", resp.StatusCode)
	}

	var result contactQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode BlueBubbles contact query response: %w", err)
	}

	name := chooseContactDisplayName(result.Data)
	g.cacheContactDisplayName(normalizedSenderID, name)
	return name, nil
}

func chooseContactDisplayName(contacts []contactRecord) string {
	for _, contact := range contacts {
		if name := strings.TrimSpace(contact.DisplayName); name != "" {
			return name
		}
		fullName := strings.TrimSpace(strings.TrimSpace(contact.FirstName) + " " + strings.TrimSpace(contact.LastName))
		if fullName != "" {
			return fullName
		}
		if nickname := strings.TrimSpace(contact.Nickname); nickname != "" {
			return nickname
		}
	}
	return ""
}

func (g *Gateway) cachedContactDisplayName(normalizedSenderID string) (string, bool) {
	g.contactMu.Lock()
	defer g.contactMu.Unlock()
	g.pruneContactNamesLocked()
	entry, ok := g.contactNames[normalizedSenderID]
	if !ok {
		return "", false
	}
	return entry.DisplayName, true
}

func (g *Gateway) cacheContactDisplayName(normalizedSenderID, displayName string) {
	g.contactMu.Lock()
	defer g.contactMu.Unlock()
	g.pruneContactNamesLocked()
	g.contactNames[normalizedSenderID] = contactNameCacheEntry{
		DisplayName: displayName,
		ExpiresAt:   time.Now().Add(contactCacheTTL),
	}
}

func (g *Gateway) pruneContactNamesLocked() {
	now := time.Now()
	for senderID, entry := range g.contactNames {
		if !entry.ExpiresAt.After(now) {
			delete(g.contactNames, senderID)
		}
	}
}

func (g *Gateway) loadImages(attachments []attachment) ([]ollama.InputImage, []string) {
	if len(attachments) == 0 {
		return nil, nil
	}

	images := make([]ollama.InputImage, 0, len(attachments))
	unsupported := make([]string, 0)
	for _, attachment := range attachments {
		label := media.AttachmentLabel(attachment.TransferName, attachment.MimeType)
		if len(images) >= media.MaxImagesPerRequest {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.MimeType != "" && !media.LooksLikeImageMIME(attachment.MimeType) {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.TotalBytes > media.MaxImageBytes {
			unsupported = append(unsupported, label)
			continue
		}

		image, err := g.fetchAttachmentImage(attachment)
		if err != nil {
			g.Log.Server("gateway.imessage", config.F("gateway", "imessage")).Warn("gateway.attachment.rejected", "rejected imessage attachment", config.F("filename", attachment.TransferName), config.F("status", "degraded"), config.ErrorField(err))
			unsupported = append(unsupported, label)
			continue
		}
		if image.Data == "" {
			unsupported = append(unsupported, label)
			continue
		}
		images = append(images, image)
	}

	if len(images) == 0 {
		return nil, unsupported
	}
	return images, unsupported
}

func (g *Gateway) fetchAttachmentImage(attachment attachment) (ollama.InputImage, error) {
	if strings.TrimSpace(attachment.GUID) == "" {
		return ollama.InputImage{}, nil
	}

	endpoint, err := buildBlueBubblesAttachmentEndpoint(g.BlueBubblesURL, attachment.GUID, g.BlueBubblesPassword)
	if err != nil {
		return ollama.InputImage{}, err
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("build BlueBubbles attachment request: %w", err)
	}

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("download BlueBubbles attachment %q: %w", attachment.TransferName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		g.Log.Server("gateway.imessage", config.F("gateway", "imessage")).Warn("gateway.attachment.fetch_failed", "failed to fetch imessage attachment", config.F("filename", attachment.TransferName), config.F("http_status", resp.StatusCode), config.F("status", "degraded"), config.F("body_preview", strings.TrimSpace(string(body))))
		return ollama.InputImage{}, fmt.Errorf("download BlueBubbles attachment %q failed with status %d", attachment.TransferName, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("read BlueBubbles attachment %q: %w", attachment.TransferName, err)
	}
	if len(body) > media.MaxImageBytes {
		return ollama.InputImage{}, fmt.Errorf("attachment %q exceeds %d bytes", attachment.TransferName, media.MaxImageBytes)
	}

	result, err := media.NormalizeInputImageFromBytes(resp.Header, attachment.MimeType, body, attachment.TransferName)
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("attachment %q rejected: %w", attachment.TransferName, err)
	}
	g.Log.Server("gateway.imessage", config.F("gateway", "imessage")).Debug("gateway.attachment.normalized", "normalized imessage attachment", config.F("filename", attachment.TransferName), config.F("attachment_id", attachment.GUID), config.F("declared_mime", strings.TrimSpace(attachment.MimeType)), config.F("detected_mime", result.DetectedMIME), config.F("normalized_mime", result.Image.MimeType), config.F("content_chars", len(body)), config.F("width", result.Width), config.F("height", result.Height), config.F("preserved_alpha", result.PreservedAlpha), config.F("used_declared_mime", result.UsedDeclaredMIME))

	image := result.Image
	return image, nil
}

// sessionKey returns the session identifier for a direct or group iMessage chat.
func (g *Gateway) sessionKey(chat messageChat, normalizedSenderID string) string {
	if chat.Style == chatStyleDirect {
		return "imessage:dm:" + normalizedSenderID
	}
	return "imessage:" + chat.GUID + ":" + normalizedSenderID
}

// startTyping enables the typing indicator for the given chat.
func (g *Gateway) startTyping(chatGUID string) error {
	return g.sendTypingRequest(chatGUID)
}

// sendTypingRequest sends a BlueBubbles typing request for the given chat.
func (g *Gateway) sendTypingRequest(chatGUID string) error {
	// BlueBubbles expects the raw chat GUID, not URL-encoded
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/chat/"+chatGUID+"/typing", g.BlueBubblesPassword)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build BlueBubbles typing request: %w", err)
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("send BlueBubbles typing request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		g.Log.Server("gateway.imessage", config.F("gateway", "imessage")).Warn("gateway.typing.failed", "BlueBubbles typing request failed", config.F("chat_id", chatGUID), config.F("http_status", resp.StatusCode), config.F("status", "degraded"), config.F("body_preview", strings.TrimSpace(string(body))))
		return fmt.Errorf("BlueBubbles typing request failed with status %d", resp.StatusCode)
	}
	return nil
}

// sendTextReply sends a text reply, retrying with the fallback method if needed.
func (g *Gateway) sendTextReply(chatGUID, text, selectedMessageGUID string, partIndex int) (string, error) {
	messageGUID, err := g.sendText(chatGUID, text, selectedMessageGUID, partIndex, defaultSendMethod)
	if err == nil {
		return messageGUID, nil
	}
	if defaultSendMethod == fallbackSendMethod {
		return g.sendText(chatGUID, text, selectedMessageGUID, partIndex, fallbackSendMethod)
	}
	g.Log.Server("gateway.imessage", config.F("gateway", "imessage")).Warn("gateway.send.retry", "retrying imessage send with fallback method", config.F("chat_id", chatGUID), config.F("default_method", defaultSendMethod), config.F("fallback_method", fallbackSendMethod), config.F("status", "retry"), config.ErrorField(err))
	return g.sendText(chatGUID, text, selectedMessageGUID, partIndex, fallbackSendMethod)
}

// sendText posts a text message to BlueBubbles and returns the created message GUID.
func (g *Gateway) sendText(chatGUID, text, selectedMessageGUID string, partIndex int, method string) (string, error) {
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/message/text", g.BlueBubblesPassword)
	if err != nil {
		return "", err
	}

	payload := sendTextRequest{
		ChatGUID:            chatGUID,
		Message:             text,
		Method:              method,
		SelectedMessageGUID: selectedMessageGUID,
		PartIndex:           partIndex,
		TempGUID:            newTempGUID(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal BlueBubbles send payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build BlueBubbles request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("send BlueBubbles request: %w", err)
	}
	defer resp.Body.Close()

	var result sendTextResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode BlueBubbles send response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if result.Error != nil {
			g.Log.Server("gateway.imessage", config.F("gateway", "imessage")).Warn("gateway.send.provider_failed", "BlueBubbles send failed", config.F("chat_id", chatGUID), config.F("method", method), config.F("http_status", resp.StatusCode), config.F("status", "error"), config.F("error", result.Error.Error))
		} else {
			g.Log.Server("gateway.imessage", config.F("gateway", "imessage")).Warn("gateway.send.provider_failed", "BlueBubbles send failed", config.F("chat_id", chatGUID), config.F("method", method), config.F("http_status", resp.StatusCode), config.F("status", "error"))
		}
		if result.Error != nil {
			return "", fmt.Errorf("BlueBubbles send failed (%d): %s", resp.StatusCode, result.Error.Error)
		}
		return "", fmt.Errorf("BlueBubbles send failed with status %d", resp.StatusCode)
	}
	return result.Data.GUID, nil
}

// buildBlueBubblesEndpoint constructs an authenticated BlueBubbles REST endpoint.
func buildBlueBubblesEndpoint(baseURL, path, password string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles URL: %w", err)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	query := parsed.Query()
	query.Set("guid", password)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func buildBlueBubblesAttachmentEndpoint(baseURL, attachmentGUID, password string) (string, error) {
	endpoint, err := buildBlueBubblesEndpoint(baseURL, "/api/v1/attachment/"+attachmentGUID+"/download", password)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles attachment URL: %w", err)
	}
	query := parsed.Query()
	query.Set("original", "true")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// rememberInboundMessage caches inbound message context for reply reconstruction.
func (g *Gateway) rememberInboundMessage(msg webhookMessage, sessionKey, normalizedSenderID, displayName string) {
	if msg.GUID == "" {
		return
	}
	g.rememberMessage(msg.GUID, messageContext{
		SessionKey:  sessionKey,
		ChatGUID:    msg.primaryChat().GUID,
		SenderID:    normalizedSenderID,
		DisplayName: displayName,
		Text:        strings.TrimSpace(msg.Text),
		IsFromBot:   false,
		CreatedAt:   time.Now(),
	})
}

// rememberBotMessage caches bot-authored message context for reply reconstruction.
func (g *Gateway) rememberBotMessage(messageGUID, sessionKey, chatGUID, senderID, text string) {
	g.rememberMessage(messageGUID, messageContext{
		SessionKey:  sessionKey,
		ChatGUID:    chatGUID,
		SenderID:    senderID,
		DisplayName: "Oswald",
		Text:        strings.TrimSpace(text),
		IsFromBot:   true,
		CreatedAt:   time.Now(),
	})
}

// rememberMessage stores reply context in the in-memory message index.
func (g *Gateway) rememberMessage(messageGUID string, ctx messageContext) {
	if messageGUID == "" {
		return
	}
	g.messageMu.Lock()
	defer g.messageMu.Unlock()
	g.pruneMessageIndexLocked()
	g.messageIndex[messageGUID] = ctx
}

// lookupMessage returns cached reply context for a prior message GUID.
func (g *Gateway) lookupMessage(messageGUID string) (messageContext, bool) {
	if messageGUID == "" {
		return messageContext{}, false
	}
	g.messageMu.Lock()
	defer g.messageMu.Unlock()
	g.pruneMessageIndexLocked()
	ctx, ok := g.messageIndex[messageGUID]
	return ctx, ok
}

// pruneMessageIndexLocked removes expired entries from the in-memory message index.
func (g *Gateway) pruneMessageIndexLocked() {
	cutoff := time.Now().Add(-messageIndexTTL)
	for guid, ctx := range g.messageIndex {
		if ctx.CreatedAt.Before(cutoff) {
			delete(g.messageIndex, guid)
		}
	}
}

// primaryChat returns the first chat attached to the webhook payload.
func (m webhookMessage) primaryChat() messageChat {
	if len(m.Chats) == 0 {
		return messageChat{}
	}
	return m.Chats[0]
}

// replyTargetGUID returns the GUID of the message this inbound message references.
func (m webhookMessage) replyTargetGUID() string {
	if m.ThreadOriginatorGUID != "" {
		return m.ThreadOriginatorGUID
	}
	if m.ReplyToGUID != "" {
		return m.ReplyToGUID
	}
	return ""
}

// isAccountCommand reports whether input is an account-link management command.
func isAccountCommand(input string) bool {
	trimmed := strings.TrimSpace(input)
	return strings.HasPrefix(trimmed, "/connect") || strings.HasPrefix(trimmed, "/disconnect")
}

func attachmentFormats(attachments []attachment) string {
	formats := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		format := strings.TrimSpace(attachment.MimeType)
		if format == "" {
			format = "unknown"
		}
		formats = append(formats, format)
	}
	return strings.Join(formats, ",")
}

// newTempGUID returns a temporary GUID for outbound BlueBubbles send requests.
func newTempGUID() string {
	return fmt.Sprintf("oswald-%d", time.Now().UnixNano())
}

// truncate returns s shortened to at most max runes, appending "..." if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
