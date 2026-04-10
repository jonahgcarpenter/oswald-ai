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

func (g *Gateway) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()

	// TEMPORARY: Log raw webhook for debugging
	body, err := io.ReadAll(r.Body)
	if err != nil {
		g.Log.Warn("iMessage webhook read failed: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// g.Log.Debug("iMessage webhook received: %s", string(body))

	var event webhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		g.Log.Warn("iMessage webhook decode failed: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	switch strings.TrimSpace(strings.ToLower(event.Type)) {
	case "new-message":
		if event.Data.IsFromMe {
			g.Log.Debug("Ignoring self-message (isFromMe=true)")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.TrimSpace(event.Data.Text) == "" {
			g.Log.Debug("Ignoring message with empty text content")
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

func (g *Gateway) processIncomingMessage(msg webhookMessage) {
	chat := msg.primaryChat()
	if chat.GUID == "" || strings.TrimSpace(msg.Text) == "" || strings.TrimSpace(msg.Handle.Address) == "" {
		g.Log.Warn("iMessage webhook ignored: incomplete message payload")
		return
	}

	normalizedSenderID, err := accountlink.NormalizeIdentifier("imessage", msg.Handle.Address)
	if err != nil {
		g.Log.Error("iMessage identifier normalization error: %v", err)
		return
	}

	canonicalUserID, err := g.Links.EnsureAccount("imessage", normalizedSenderID, msg.accountDisplayName())
	if err != nil {
		g.Log.Error("iMessage account resolution error: %v", err)
		return
	}

	sessionKey := g.sessionKey(chat, normalizedSenderID)
	prompt := strings.TrimSpace(msg.Text)
	replyGUID := msg.replyTargetGUID()
	isReplyToBot := false
	if replyGUID != "" {
		if replyCtx, ok := g.lookupMessage(replyGUID); ok {
			isReplyToBot = replyCtx.IsFromBot
			switch {
			case replyCtx.IsFromBot && replyCtx.SessionKey == sessionKey:
				g.Log.Debug("iMessage reply context: same-session reply to Oswald message %s in session %s; using memory only", replyGUID, sessionKey)
			case replyCtx.IsFromBot && replyCtx.SessionKey != sessionKey:
				g.Log.Debug("iMessage reply context: cross-session reply to Oswald message %s (from %s to %s); injecting quoted context", replyGUID, replyCtx.SessionKey, sessionKey)
				prompt = fmt.Sprintf("[Replying to Oswald's previous message in this chat: %q]\n%s", replyCtx.Text, prompt)
			case !replyCtx.IsFromBot:
				g.Log.Debug("iMessage reply context: quoted non-bot message from %s in chat %s", replyCtx.DisplayName, chat.GUID)
				prompt = fmt.Sprintf("[Replying to %s: %q]\n%s", replyCtx.DisplayName, replyCtx.Text, prompt)
			}
		} else {
			g.Log.Debug("iMessage reply context: unknown reply target %s; no cached context available", replyGUID)
		}
	}

	isAccountCommand := isAccountCommand(prompt)
	isGroup := chat.Style == chatStyleGroup || strings.Contains(chat.GUID, ";+;")
	if isGroup && !isReplyToBot && !isAccountCommand && !mentionRE.MatchString(prompt) {
		g.Log.Debug("iMessage group message ignored without @oswald mention in chat %s", chat.GUID)
		g.rememberInboundMessage(msg, sessionKey, normalizedSenderID)
		return
	}
	if isGroup {
		prompt = strings.TrimSpace(mentionRE.ReplaceAllString(prompt, ""))
	}
	if prompt == "" {
		responseText := "What do you want idiot."
		messageGUID, err := g.sendTextReply(chat.GUID, responseText, "", 0)
		if err != nil {
			g.Log.Error("iMessage empty prompt response send failed: %v", err)
		} else {
			g.rememberBotMessage(messageGUID, sessionKey, chat.GUID, normalizedSenderID, responseText)
		}
		g.rememberInboundMessage(msg, sessionKey, normalizedSenderID)
		return
	}

	g.rememberInboundMessage(msg, sessionKey, normalizedSenderID)
	g.Log.Debug("iMessage request from %s (session=%s canonical=%s): %q", msg.displayName(), sessionKey, canonicalUserID, truncate(prompt, 100))

	stopTyping := make(chan struct{})
	defer close(stopTyping)
	go g.typingLoop(chat.GUID, stopTyping)

	if commandResponse, handled, commandErr := g.Commands.Handle(canonicalUserID, prompt); handled {
		if commandErr != nil {
			g.Log.Error("iMessage account command error: %v", commandErr)
			commandResponse = "Failed to process account linking command."
		}
		messageGUID, err := g.sendTextReply(chat.GUID, commandResponse, "", 0)
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
		DisplayName:  msg.accountDisplayName(),
		SessionKey:   sessionKey,
		Prompt:       prompt,
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
		g.Log.Debug("iMessage response empty for chat %s", chat.GUID)
		return
	}

	messageGUID, err := g.sendTextReply(chat.GUID, responseText, "", 0)
	if err != nil {
		g.Log.Error("iMessage send failed for chat %s: %v", chat.GUID, err)
		return
	}
	g.rememberBotMessage(messageGUID, sessionKey, chat.GUID, normalizedSenderID, responseText)
}

func (g *Gateway) sessionKey(chat messageChat, normalizedSenderID string) string {
	if chat.Style == chatStyleDirect {
		return "imessage:dm:" + normalizedSenderID
	}
	return "imessage:" + chat.GUID + ":" + normalizedSenderID
}

func (g *Gateway) typingLoop(chatGUID string, stop <-chan struct{}) {
	_ = g.startTyping(chatGUID)
	ticker := time.NewTicker(9 * time.Second)
	defer ticker.Stop()
	defer func() {
		if err := g.stopTyping(chatGUID); err != nil {
			g.Log.Debug("iMessage stop typing failed for chat %s: %v", chatGUID, err)
		}
	}()

	for {
		select {
		case <-ticker.C:
			_ = g.startTyping(chatGUID)
		case <-stop:
			return
		}
	}
}

func (g *Gateway) startTyping(chatGUID string) error {
	return g.sendTypingRequest(http.MethodPost, chatGUID)
}

func (g *Gateway) stopTyping(chatGUID string) error {
	return g.sendTypingRequest(http.MethodDelete, chatGUID)
}

func (g *Gateway) sendTypingRequest(method, chatGUID string) error {
	// BlueBubbles expects the raw chat GUID, not URL-encoded
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/chat/"+chatGUID+"/typing", g.BlueBubblesPassword)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, endpoint, nil)
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

func (g *Gateway) sendTextReply(chatGUID, text, selectedMessageGUID string, partIndex int) (string, error) {
	messageGUID, err := g.sendText(chatGUID, text, selectedMessageGUID, partIndex, defaultSendMethod)
	if err == nil {
		return messageGUID, nil
	}
	if defaultSendMethod == fallbackSendMethod {
		return g.sendText(chatGUID, text, selectedMessageGUID, partIndex, fallbackSendMethod)
	}
	g.Log.Warn("iMessage %s send failed; retrying with %s", defaultSendMethod, fallbackSendMethod)
	return g.sendText(chatGUID, text, selectedMessageGUID, partIndex, fallbackSendMethod)
}

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

func (g *Gateway) rememberInboundMessage(msg webhookMessage, sessionKey, normalizedSenderID string) {
	if msg.GUID == "" {
		return
	}
	g.rememberMessage(msg.GUID, messageContext{
		SessionKey:  sessionKey,
		ChatGUID:    msg.primaryChat().GUID,
		SenderID:    normalizedSenderID,
		DisplayName: msg.displayName(),
		Text:        strings.TrimSpace(msg.Text),
		IsFromBot:   false,
		CreatedAt:   time.Now(),
	})
}

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

func (g *Gateway) rememberMessage(messageGUID string, ctx messageContext) {
	if messageGUID == "" {
		return
	}
	g.messageMu.Lock()
	defer g.messageMu.Unlock()
	g.pruneMessageIndexLocked()
	g.messageIndex[messageGUID] = ctx
}

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

func (g *Gateway) pruneMessageIndexLocked() {
	cutoff := time.Now().Add(-messageIndexTTL)
	for guid, ctx := range g.messageIndex {
		if ctx.CreatedAt.Before(cutoff) {
			delete(g.messageIndex, guid)
		}
	}
}

func (m webhookMessage) primaryChat() messageChat {
	if len(m.Chats) == 0 {
		return messageChat{}
	}
	return m.Chats[0]
}

func (m webhookMessage) displayName() string {
	if chat := m.primaryChat(); chat.DisplayName != "" {
		return chat.DisplayName
	}
	if m.GroupTitle != "" {
		return m.GroupTitle
	}
	if m.Handle.Address != "" {
		return m.Handle.Address
	}
	return "iMessage"
}

func (m webhookMessage) accountDisplayName() string {
	if chat := m.primaryChat(); chat.DisplayName != "" {
		return chat.DisplayName
	}
	if m.GroupTitle != "" {
		return m.GroupTitle
	}
	return m.displayName()
}

func (m webhookMessage) replyTargetGUID() string {
	if m.ThreadOriginatorGUID != "" {
		return m.ThreadOriginatorGUID
	}
	if m.ReplyToGUID != "" {
		return m.ReplyToGUID
	}
	return ""
}

func isAccountCommand(input string) bool {
	trimmed := strings.TrimSpace(input)
	return strings.HasPrefix(trimmed, "/connect") || strings.HasPrefix(trimmed, "/disconnect")
}

func newTempGUID() string {
	return fmt.Sprintf("oswald-%d", time.Now().UnixNano())
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
