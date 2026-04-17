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

	g.Log.Info("iMessage webhook listener on port %s path %s", g.Port, path)
	return http.ListenAndServe(":"+g.Port, mux)
}

// handleWebhook validates and dispatches incoming BlueBubbles webhook events.
func (g *Gateway) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		g.Log.Warn("iMessage webhook read failed: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var event webhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		g.Log.Warn("iMessage webhook decode failed: %v", err)
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
			g.Log.Debug("iMessage webhook ignored: no text or attachments (guid=%s associated_type=%s)", event.Data.GUID, event.Data.AssociatedMessageType)
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
	chat := msg.primaryChat()
	if chat.GUID == "" || strings.TrimSpace(msg.Handle.Address) == "" {
		g.Log.Warn("iMessage webhook ignored: incomplete message payload")
		return
	}
	images, unsupported := g.loadImages(msg.Attachments)
	if strings.TrimSpace(msg.Text) == "" && len(images) == 0 {
		if len(unsupported) == 0 {
			g.Log.Warn("iMessage webhook ignored: incomplete message payload")
			return
		}
	}

	prompt := media.AugmentPromptWithUnsupportedFiles(strings.TrimSpace(msg.Text), unsupported)
	if prompt == "" && len(images) == 0 {
		return
	}

	normalizedSenderID, err := accountlink.NormalizeIdentifier("imessage", msg.Handle.Address)
	if err != nil {
		g.Log.Error("iMessage identifier normalization error: %v", err)
		return
	}
	displayName := normalizedSenderID
	if resolvedName, err := g.lookupContactDisplayName(normalizedSenderID); err != nil {
		g.Log.Debug("iMessage contact lookup failed for %s: %v", normalizedSenderID, err)
	} else if resolvedName != "" {
		displayName = resolvedName
	}

	canonicalUserID, err := g.Links.EnsureAccount("imessage", normalizedSenderID, displayName)
	if err != nil {
		g.Log.Error("iMessage account resolution error: %v", err)
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
				if replyCtx.IsFromBot {
					g.Log.Debug("iMessage reply context: quoted Oswald message %s in chat %s", replyGUID, chat.GUID)
				} else {
					g.Log.Debug("iMessage reply context: quoted non-bot message from %s in chat %s", replyName, chat.GUID)
				}
				prompt = fmt.Sprintf("[Replying to %s: %q]\n%s", replyName, replyCtx.Text, prompt)
			case replyName != "":
				g.Log.Debug("iMessage reply context: referenced message from %s is unavailable in chat %s", replyName, chat.GUID)
				prompt = fmt.Sprintf("[Replying to %s's message, but it is unavailable]\n%s", replyName, prompt)
			default:
				g.Log.Debug("iMessage reply context: referenced message %s is unavailable in chat %s", replyGUID, chat.GUID)
				prompt = fmt.Sprintf("[Replying to a message that is unavailable]\n%s", prompt)
			}
		} else {
			g.Log.Debug("iMessage reply context: unknown reply target %s; no cached context available", replyGUID)
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
		g.Log.Debug("iMessage start typing failed for chat %s: %v", chat.GUID, err)
	}

	if prompt == "" && len(images) == 0 {
		responseText := "What do you want idiot."
		messageGUID, err := g.sendTextReply(chat.GUID, responseText, selectedMessageGUID, 0)
		if err != nil {
			g.Log.Error("iMessage empty prompt response send failed: %v", err)
		} else {
			g.rememberBotMessage(messageGUID, sessionKey, chat.GUID, normalizedSenderID, responseText)
		}
		g.rememberInboundMessage(msg, sessionKey, normalizedSenderID, displayName)
		return
	}

	g.rememberInboundMessage(msg, sessionKey, normalizedSenderID, displayName)
	g.Log.Debug("iMessage request from %s (session=%s canonical=%s images=%d): %q", normalizedSenderID, sessionKey, canonicalUserID, len(images), truncate(prompt, 100))

	if commandResponse, handled, commandErr := g.Commands.Handle(canonicalUserID, prompt); handled {
		if commandErr != nil {
			g.Log.Error("iMessage account command error: %v", commandErr)
			commandResponse = "Failed to process account linking command."
		}
		messageGUID, err := g.sendTextReply(chat.GUID, commandResponse, selectedMessageGUID, 0)
		if err != nil {
			g.Log.Error("iMessage command response send failed: %v", err)
		} else {
			g.rememberBotMessage(messageGUID, sessionKey, chat.GUID, normalizedSenderID, commandResponse)
		}
		return
	}

	req := &broker.Request{
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
		g.Log.Error("iMessage agent process error: %v", result.Err)
		responseText = "Sorry, I encountered an internal error processing that."
	} else if result.Response != nil {
		responseText = strings.TrimSpace(result.Response.Response)
	}
	if responseText == "" {
		g.Log.Debug("iMessage agent returned empty response for chat %s", chat.GUID)
		return
	}

	messageGUID, err := g.sendTextReply(chat.GUID, responseText, selectedMessageGUID, 0)
	if err != nil {
		g.Log.Error("iMessage send failed for chat %s: %v", chat.GUID, err)
		return
	}
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
		if !media.SupportsMIMEType(attachment.MimeType) && attachment.MimeType != "" {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.TotalBytes > media.MaxImageBytes {
			unsupported = append(unsupported, label)
			continue
		}

		image, err := g.fetchAttachmentImage(attachment)
		if err != nil {
			g.Log.Warn("iMessage attachment rejected for %q: %v", attachment.TransferName, err)
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
		return ollama.InputImage{}, fmt.Errorf("download BlueBubbles attachment %q failed with status %d", attachment.TransferName, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("read BlueBubbles attachment %q: %w", attachment.TransferName, err)
	}
	if len(body) > media.MaxImageBytes {
		return ollama.InputImage{}, fmt.Errorf("attachment %q exceeds %d bytes", attachment.TransferName, media.MaxImageBytes)
	}

	mimeType := media.DetectMIMEType(resp.Header, body)
	if mimeType == "" && media.SupportsMIMEType(attachment.MimeType) {
		mimeType = attachment.MimeType
	}
	if mimeType == "" {
		return ollama.InputImage{}, nil
	}

	image, err := media.BuildInputImageFromBytes(mimeType, body, attachment.TransferName)
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("attachment %q rejected: %w", attachment.TransferName, err)
	}
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
	g.Log.Warn("iMessage %s send failed for chat %s: %v; retrying with %s", defaultSendMethod, chatGUID, err, fallbackSendMethod)
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
