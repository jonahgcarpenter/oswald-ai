package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/accountlink"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

const replyIndexTTL = time.Hour

// Name returns the human-readable gateway name.
func (dg *Gateway) Name() string {
	return "Discord"
}

// Start initializes the resilient connection loop.
// It blocks forever, automatically reconnecting if the websocket drops.
func (dg *Gateway) Start(b *broker.Broker) error {
	dg.Broker = b
	if dg.replyIndex == nil {
		dg.replyIndex = make(map[string]replyContext)
	}

	for {
		err := dg.connectAndListen()

		if err != nil {
			dg.Log.Warn("Discord connection dropped: %v", err)
		} else {
			dg.Log.Debug("Discord connection closed normally.")
		}

		dg.Log.Debug("Reconnecting to Discord Gateway in 5 seconds...")
		time.Sleep(5 * time.Second)
	}
}

func (dg *Gateway) rememberReply(messageID string, ctx replyContext) {
	if messageID == "" {
		return
	}

	dg.replyMu.Lock()
	dg.pruneReplyIndexLocked()
	dg.replyIndex[messageID] = ctx
	dg.replyMu.Unlock()
}

func (dg *Gateway) lookupReply(messageID string) (replyContext, bool) {
	if messageID == "" {
		return replyContext{}, false
	}

	dg.replyMu.Lock()
	dg.pruneReplyIndexLocked()
	ctx, ok := dg.replyIndex[messageID]
	dg.replyMu.Unlock()

	return ctx, ok
}

func (dg *Gateway) pruneReplyIndexLocked() int {
	cutoff := time.Now().Add(-replyIndexTTL)
	pruned := 0
	for id, ctx := range dg.replyIndex {
		if ctx.CreatedAt.Before(cutoff) {
			delete(dg.replyIndex, id)
			pruned++
		}
	}
	return pruned
}

// connectAndListen manages a single Discord gateway session.
func (dg *Gateway) connectAndListen() error {
	conn, _, err := gorilla.DefaultDialer.Dial(gatewayURL, nil)
	if err != nil {
		return fmt.Errorf("Failed to dial Discord Gateway: %w", err)
	}
	defer conn.Close()

	var helloPayload Payload
	if err := conn.ReadJSON(&helloPayload); err != nil || helloPayload.Op != 10 {
		return fmt.Errorf("Expected HELLO payload: %v", err)
	}

	var hello HelloEvent
	json.Unmarshal(helloPayload.D, &hello) // nolint: errcheck

	go dg.heartbeatLoop(conn, hello.HeartbeatInterval*time.Millisecond)

	if err := dg.identify(conn); err != nil {
		return fmt.Errorf("Failed to identify: %w", err)
	}

	return dg.listenLoop(conn)
}

// identify authenticates the bot with its token and intents.
func (dg *Gateway) identify(conn *gorilla.Conn) error {
	identifyData := map[string]interface{}{
		"token":   dg.Token,
		"intents": intents,
		"properties": map[string]string{
			"os":      "linux",
			"browser": "oswald-ai",
			"device":  "oswald-ai",
		},
	}

	identifyPayload := Payload{
		Op: 2,
		D:  marshalJSON(identifyData),
	}

	if err := conn.WriteJSON(identifyPayload); err != nil {
		return fmt.Errorf("Failed to send IDENTIFY: %w", err)
	}
	return nil
}

// heartbeatLoop sends heartbeat packets to Discord at the specified interval.
func (dg *Gateway) heartbeatLoop(conn *gorilla.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		hb := Payload{Op: 1, D: []byte("null")}
		if err := conn.WriteJSON(hb); err != nil {
			dg.Log.Error("Heartbeat failed: %v", err)
			return
		}
	}
}

// listenLoop reads events from the Discord gateway and dispatches them.
func (dg *Gateway) listenLoop(conn *gorilla.Conn) error {
	for {
		var p Payload
		if err := conn.ReadJSON(&p); err != nil {
			return fmt.Errorf("Discord read error: %w", err)
		}

		switch p.Op {
		case 0:
			if p.T != nil {
				switch *p.T {
				case "READY":
					var ready ReadyEvent
					if err := json.Unmarshal(p.D, &ready); err == nil {
						dg.BotID = ready.User.ID
						dg.Log.Debug("Discord Bot connected as: %s (ID: %s)", ready.User.Username, dg.BotID)
					}
				case "RESUMED":
					dg.Log.Debug("Discord session resumed successfully.")
				case "MESSAGE_CREATE":
					var msg MessageCreate
					if err := json.Unmarshal(p.D, &msg); err == nil {
						go dg.handleMessage(msg)
					}
				}
			}
		case 7:
			return fmt.Errorf("Discord requested a reconnect")
		case 9:
			return fmt.Errorf("Discord session invalid, forcing reconnect")
		case 11:
		}
	}
}

// splitMessage breaks a large string into chunks respecting Discord's 2000-char limit.
func splitMessage(text string, limit int) []string {
	var chunks []string
	runes := []rune(text)

	for len(runes) > 0 {
		if len(runes) <= limit {
			chunks = append(chunks, string(runes))
			break
		}

		chunkRunes := runes[:limit]
		splitIdx := -1

		for i := len(chunkRunes) - 1; i >= 0; i-- {
			if chunkRunes[i] == '\n' {
				splitIdx = i
				break
			}
		}

		if splitIdx == -1 {
			for i := len(chunkRunes) - 1; i > 0; i-- {
				if (chunkRunes[i-1] == '.' || chunkRunes[i-1] == '!' || chunkRunes[i-1] == '?') && chunkRunes[i] == ' ' {
					splitIdx = i
					break
				}
			}
		}

		if splitIdx == -1 {
			for i := len(chunkRunes) - 1; i >= 0; i-- {
				if chunkRunes[i] == ' ' {
					splitIdx = i
					break
				}
			}
		}

		if splitIdx == -1 {
			splitIdx = limit
		}

		chunks = append(chunks, strings.TrimSpace(string(runes[:splitIdx])))
		runes = runes[splitIdx:]
		runes = []rune(strings.TrimLeft(string(runes), " \n\r"))
	}

	return chunks
}

// resolveMentions replaces every <@ID> and <@!ID> token in text with @username.
func resolveMentions(text string, mentions []struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}) string {
	lookup := make(map[string]string, len(mentions))
	for _, m := range mentions {
		lookup[m.ID] = m.Username
	}

	re := regexp.MustCompile(`<@!?(\d+)>`)
	return re.ReplaceAllStringFunc(text, func(match string) string {
		sub := re.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		if username, ok := lookup[sub[1]]; ok {
			return "@" + username
		}
		return match
	})
}

// handleMessage processes an incoming Discord message.
func (dg *Gateway) handleMessage(msg MessageCreate) {
	if msg.Author.Bot {
		return
	}

	replyToID := ""
	prompt := msg.Content
	images, unsupported := dg.loadImages(msg.Attachments)

	if msg.GuildID != "" {
		mention1 := fmt.Sprintf("<@%s>", dg.BotID)
		mention2 := fmt.Sprintf("<@!%s>", dg.BotID)
		isReplyToBot := msg.ReferencedMessage != nil && msg.ReferencedMessage.Author.ID == dg.BotID
		isAccountCommand := strings.HasPrefix(strings.TrimSpace(msg.Content), "/connect") || strings.HasPrefix(strings.TrimSpace(msg.Content), "/disconnect")

		if !isReplyToBot && !isAccountCommand && !strings.Contains(msg.Content, mention1) && !strings.Contains(msg.Content, mention2) {
			return
		}
		replyToID = msg.ID

		prompt = strings.ReplaceAll(prompt, mention1, "")
		prompt = strings.ReplaceAll(prompt, mention2, "")
		prompt = strings.TrimSpace(prompt)
	}

	prompt = strings.TrimSpace(prompt)
	prompt = media.AugmentPromptWithUnsupportedFiles(prompt, unsupported)
	if prompt == "" && len(images) == 0 {
		_, _ = dg.sendMessage(msg.ChannelID, "What do you want idiot.", replyToID)
		return
	}

	re := regexp.MustCompile(`<a?:([^:]+):\d+>`)
	prompt = re.ReplaceAllString(prompt, ":$1:")
	prompt = resolveMentions(prompt, msg.Mentions)

	// Compute the session key using the hybrid strategy:
	//   DMs (no GuildID):      SenderID           — continuous per-user memory
	//   Guild channels/threads: ChannelID:SenderID — per-user isolation, prevents cross-talk
	var sessionKey string
	if msg.GuildID == "" {
		sessionKey = "discord:dm:" + msg.Author.ID
	} else {
		sessionKey = "discord:" + msg.ChannelID + ":" + msg.Author.ID
	}

	normalizedAuthorID, normErr := accountlink.NormalizeIdentifier("discord", msg.Author.ID)
	if normErr != nil {
		dg.Log.Error("Discord account normalization error: %v", normErr)
		_, _ = dg.sendMessage(msg.ChannelID, "Sorry, I could not resolve your Discord account identity.", replyToID)
		return
	}

	canonicalUserID, err := dg.Links.EnsureAccount("discord", normalizedAuthorID, msg.Author.Username)
	if err != nil {
		dg.Log.Error("Discord account resolution error: %v", err)
		_, _ = dg.sendMessage(msg.ChannelID, "Sorry, I could not resolve your account identity.", replyToID)
		return
	}

	if commandResponse, handled, commandErr := dg.Commands.Handle(canonicalUserID, prompt); handled {
		if commandErr != nil {
			dg.Log.Error("Discord account command error: %v", commandErr)
			commandResponse = "Failed to process account linking command."
		}
		_, _ = dg.sendMessage(msg.ChannelID, commandResponse, replyToID)
		return
	}

	if msg.ReferencedMessage != nil && msg.ReferencedMessage.Content != "" {
		quotedContent := re.ReplaceAllString(msg.ReferencedMessage.Content, ":$1:")

		if msg.ReferencedMessage.Author.ID != dg.BotID {
			dg.Log.Debug("Discord reply context: quoted non-bot message from %s in channel %s", msg.ReferencedMessage.Author.Username, msg.ChannelID)
			prompt = fmt.Sprintf("[Replying to %s: \"%s\"]\n%s",
				msg.ReferencedMessage.Author.Username,
				quotedContent,
				prompt,
			)
		} else {
			replyCtx, ok := dg.lookupReply(msg.ReferencedMessage.ID)
			switch {
			case ok && replyCtx.SessionKey == sessionKey:
				dg.Log.Debug("Discord reply context: same-session reply to Oswald message %s in session %s; using memory only", msg.ReferencedMessage.ID, sessionKey)
			case ok && replyCtx.SessionKey != sessionKey:
				dg.Log.Debug("Discord reply context: cross-session reply to Oswald message %s (from %s to %s); injecting quoted context", msg.ReferencedMessage.ID, replyCtx.SessionKey, sessionKey)
				prompt = fmt.Sprintf("[Replying to Oswald's previous message in this channel: \"%s\"]\n%s",
					quotedContent,
					prompt,
				)
			default:
				dg.Log.Debug("Discord reply context: unknown Oswald reply target %s; injecting quoted fallback context", msg.ReferencedMessage.ID)
				prompt = fmt.Sprintf("[Replying to Oswald's previous message in this channel: \"%s\"]\n%s",
					quotedContent,
					prompt,
				)
			}
		}
	}

	dg.Log.Debug("Discord request from %s (session=%s canonical=%s): %q", msg.Author.Username, sessionKey, canonicalUserID, truncate(prompt, 100))

	stopTyping := make(chan struct{})
	defer close(stopTyping)

	go func() {
		_ = dg.sendTyping(msg.ChannelID)
		ticker := time.NewTicker(9 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				_ = dg.sendTyping(msg.ChannelID)
			case <-stopTyping:
				return
			}
		}
	}()

	req := &broker.Request{
		Channel:      "discord",
		ChatID:       msg.ChannelID,
		SenderID:     canonicalUserID,
		DisplayName:  msg.Author.Username,
		SessionKey:   sessionKey,
		Prompt:       prompt,
		Images:       images,
		StreamFunc:   nil,
		ResponseChan: make(chan broker.Result, 1),
	}
	dg.Broker.Submit(req)
	result := <-req.ResponseChan

	if result.Err != nil {
		dg.Log.Error("Agent process error: %v", result.Err)
		_, _ = dg.sendMessage(msg.ChannelID, "Sorry, I encountered an internal error processing that.", replyToID)
		return
	}

	finalPayload := result.Response
	responseText := finalPayload.Response
	chunks := splitMessage(responseText, 2000)
	originCtx := replyContext{
		SessionKey: sessionKey,
		ChannelID:  msg.ChannelID,
		SenderID:   msg.Author.ID,
		CreatedAt:  time.Now(),
	}

	dg.Log.Debug("Discord response to %s: %d chunk(s), %d chars, model: %s",
		msg.Author.Username, len(chunks), len(responseText), finalPayload.Model)

	for i, chunk := range chunks {
		currentReplyID := ""
		if i == 0 {
			currentReplyID = replyToID
		}

		sentMessageID, err := dg.sendMessage(msg.ChannelID, chunk, currentReplyID)
		if err != nil {
			dg.Log.Error("Failed to send chunk %d to Discord: %v", i+1, err)
			break
		}

		if i == 0 {
			dg.rememberReply(sentMessageID, originCtx)
		}
	}
}

func (dg *Gateway) loadImages(attachments []struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Size        int    `json:"size,omitempty"`
	URL         string `json:"url,omitempty"`
	ProxyURL    string `json:"proxy_url,omitempty"`
}) ([]ollama.InputImage, []string) {
	if len(attachments) == 0 {
		return nil, nil
	}

	images := make([]ollama.InputImage, 0, len(attachments))
	unsupported := make([]string, 0)
	for _, attachment := range attachments {
		label := media.AttachmentLabel(attachment.Filename, attachment.ContentType)
		if len(images) >= media.MaxImagesPerRequest {
			unsupported = append(unsupported, label)
			continue
		}
		if !media.SupportsMIMEType(attachment.ContentType) && attachment.ContentType != "" {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.Size > media.MaxImageBytes {
			unsupported = append(unsupported, label)
			continue
		}

		image, err := dg.fetchAttachmentImage(attachment.URL, attachment.Filename)
		if err != nil {
			dg.Log.Warn("Discord attachment rejected for %q: %v", attachment.Filename, err)
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

func (dg *Gateway) fetchAttachmentImage(rawURL, filename string) (ollama.InputImage, error) {
	if strings.TrimSpace(rawURL) == "" {
		return ollama.InputImage{}, nil
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("download attachment %q: %w", filename, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ollama.InputImage{}, fmt.Errorf("download attachment %q: unexpected status %d", filename, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("read attachment %q: %w", filename, err)
	}
	if len(body) > media.MaxImageBytes {
		return ollama.InputImage{}, fmt.Errorf("attachment %q exceeds %d bytes", filename, media.MaxImageBytes)
	}

	mimeType := media.DetectMIMEType(resp.Header, body)
	if mimeType == "" {
		return ollama.InputImage{}, nil
	}

	image, err := media.BuildInputImageFromBytes(mimeType, body, filename)
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("attachment %q rejected: %w", filename, err)
	}
	return image, nil
}

// sendTyping posts a typing indicator to Discord.
func (dg *Gateway) sendTyping(channelID string) error {
	url := fmt.Sprintf("%s/channels/%s/typing", apiBaseURL, channelID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bot "+dg.Token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

// sendMessage posts a message to a Discord channel and returns the created
// Discord message ID when available.
func (dg *Gateway) sendMessage(channelID, content, replyToID string) (string, error) {
	url := fmt.Sprintf("%s/channels/%s/messages", apiBaseURL, channelID)

	payload := map[string]interface{}{
		"content": content,
	}

	if replyToID != "" {
		payload["message_reference"] = map[string]string{
			"message_id": replyToID,
		}
	}

	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bot "+dg.Token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var created createMessageResponse
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", err
	}

	return created.ID, nil
}

// marshalJSON converts any value to a json.RawMessage, ignoring marshal errors.
func marshalJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// truncate returns s shortened to at most max runes, appending "..." if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
