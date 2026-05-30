package discord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/accountlink"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
)

const replyIndexTTL = time.Hour

var (
	errDiscordReconnectRequested = errors.New("discord requested reconnect")
	errDiscordInvalidSession     = errors.New("discord invalid session")
	errDiscordMissedHeartbeatAck = errors.New("discord missed heartbeat ack")
)

// Name returns the human-readable gateway name.
func (dg *Gateway) Name() string {
	return "Discord"
}

// Start initializes the resilient connection loop.
// It blocks forever, automatically reconnecting if the websocket drops.
func (dg *Gateway) Start(b *broker.Broker) error {
	dg.Broker = b
	log := dg.Log.Server("gateway.discord", config.F("gateway", "discord"))
	if dg.replyIndex == nil {
		dg.replyIndex = make(map[string]replyContext)
	}
	dg.setHeartbeatAcked(true)

	for {
		err := dg.connectAndListen()

		if err != nil {
			switch {
			case errors.Is(err, errDiscordReconnectRequested):
				log.Info("gateway.session.resume_requested", "discord requested session resume")
			case errors.Is(err, errDiscordInvalidSession):
				log.Warn("gateway.session.invalid", "discord session invalid, reidentifying")
			case errors.Is(err, errDiscordMissedHeartbeatAck):
				log.Warn("gateway.heartbeat.missed_ack", "discord heartbeat ack missing, reconnecting")
			default:
				log.Warn("gateway.connection.dropped", "discord connection dropped", config.ErrorField(err))
			}
		} else {
			log.Debug("gateway.connection.closed", "discord connection closed normally")
		}

		log.Debug("gateway.reconnect.scheduled", "scheduled discord reconnect", config.F("delay_ms", 5000))
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
	resumeURL, shouldResume := dg.resumeGatewayURL()
	conn, _, err := gorilla.DefaultDialer.Dial(resolveGatewayURL(resumeURL), nil)
	if err != nil {
		return fmt.Errorf("Failed to dial Discord Gateway: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	defer conn.Close()

	var helloPayload Payload
	if err := conn.ReadJSON(&helloPayload); err != nil || helloPayload.Op != 10 {
		return fmt.Errorf("Expected HELLO payload: %v", err)
	}

	var hello HelloEvent
	json.Unmarshal(helloPayload.D, &hello) // nolint: errcheck

	dg.setHeartbeatAcked(true)
	hbErrCh := make(chan error, 1)
	go dg.heartbeatLoop(conn, hello.HeartbeatInterval*time.Millisecond, done, hbErrCh)

	if shouldResume {
		if err := dg.resume(conn); err != nil {
			return fmt.Errorf("Failed to resume: %w", err)
		}
		dg.Log.Server("gateway.discord", config.F("gateway", "discord")).Debug("gateway.session.resume_attempt", "attempted discord session resume")
	} else {
		if err := dg.identify(conn); err != nil {
			return fmt.Errorf("Failed to identify: %w", err)
		}
	}

	err = dg.listenLoop(conn)
	select {
	case hbErr := <-hbErrCh:
		if hbErr != nil {
			return hbErr
		}
	default:
	}

	return err
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

// resume attempts to resume a prior Discord gateway session.
func (dg *Gateway) resume(conn *gorilla.Conn) error {
	sessionID, seq, ok := dg.resumeState()
	if !ok {
		return errors.New("resume state unavailable")
	}

	resumeData := map[string]interface{}{
		"token":      dg.Token,
		"session_id": sessionID,
		"seq":        seq,
	}

	resumePayload := Payload{
		Op: 6,
		D:  marshalJSON(resumeData),
	}

	if err := conn.WriteJSON(resumePayload); err != nil {
		return fmt.Errorf("Failed to send RESUME: %w", err)
	}

	return nil
}

// heartbeatLoop sends heartbeat packets to Discord at the specified interval.
func (dg *Gateway) heartbeatLoop(conn *gorilla.Conn, interval time.Duration, done <-chan struct{}, errCh chan<- error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			select {
			case errCh <- nil:
			default:
			}
			return
		case <-ticker.C:
		}

		if !dg.heartbeatAcked() {
			select {
			case errCh <- errDiscordMissedHeartbeatAck:
			default:
			}
			_ = conn.Close()
			return
		}

		dg.setHeartbeatAcked(false)

		hb := Payload{Op: 1, D: dg.heartbeatPayload()}
		if err := conn.WriteJSON(hb); err != nil {
			select {
			case <-done:
				return
			default:
			}

			if isClosedConnError(err) {
				return
			}

			select {
			case errCh <- fmt.Errorf("discord heartbeat failed: %w", err):
			default:
			}
			_ = conn.Close()
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

		if p.S != nil {
			dg.setLastSequence(*p.S)
		}

		switch p.Op {
		case 0:
			if p.T != nil {
				switch *p.T {
				case "READY":
					var ready ReadyEvent
					if err := json.Unmarshal(p.D, &ready); err == nil {
						dg.BotID = ready.User.ID
						dg.setReadySession(ready.SessionID, ready.ResumeGatewayURL)
						dg.setHeartbeatAcked(true)
						dg.Log.Server("gateway.discord", config.F("gateway", "discord")).Info("gateway.session.ready", "discord gateway ready", config.F("bot_id", dg.BotID), config.F("bot_username", ready.User.Username))
					}
				case "RESUMED":
					dg.setHeartbeatAcked(true)
					dg.Log.Server("gateway.discord", config.F("gateway", "discord")).Debug("gateway.session.resumed", "discord session resumed")
				case "MESSAGE_CREATE":
					var msg MessageCreate
					if err := json.Unmarshal(p.D, &msg); err == nil {
						go dg.handleMessage(msg)
					}
				}
			}
		case 7:
			return errDiscordReconnectRequested
		case 9:
			dg.clearResumeState()
			return errDiscordInvalidSession
		case 11:
			dg.setHeartbeatAcked(true)
		}
	}
}

func (dg *Gateway) setReadySession(sessionID, resumeURL string) {
	dg.sessionMu.Lock()
	dg.sessionID = sessionID
	dg.resumeURL = resumeURL
	dg.sessionMu.Unlock()
}

func (dg *Gateway) clearResumeState() {
	dg.sessionMu.Lock()
	dg.sessionID = ""
	dg.resumeURL = ""
	dg.lastSeq = nil
	dg.hbAcked = true
	dg.sessionMu.Unlock()
}

func (dg *Gateway) setLastSequence(seq int) {
	dg.sessionMu.Lock()
	seqCopy := seq
	dg.lastSeq = &seqCopy
	dg.sessionMu.Unlock()
}

func (dg *Gateway) heartbeatPayload() json.RawMessage {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	if dg.lastSeq == nil {
		return json.RawMessage("null")
	}
	return marshalJSON(*dg.lastSeq)
}

func (dg *Gateway) setHeartbeatAcked(acked bool) {
	dg.sessionMu.Lock()
	dg.hbAcked = acked
	dg.sessionMu.Unlock()
}

func (dg *Gateway) heartbeatAcked() bool {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	return dg.hbAcked
}

func (dg *Gateway) resumeState() (string, int, bool) {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	if dg.sessionID == "" || dg.lastSeq == nil {
		return "", 0, false
	}
	return dg.sessionID, *dg.lastSeq, true
}

func (dg *Gateway) resumeGatewayURL() (string, bool) {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	return dg.resumeURL, dg.sessionID != "" && dg.lastSeq != nil
}

func resolveGatewayURL(resumeURL string) string {
	if resumeURL == "" {
		return gatewayURL
	}
	if strings.HasPrefix(resumeURL, "ws://") || strings.HasPrefix(resumeURL, "wss://") {
		return resumeURL
	}
	return fmt.Sprintf("wss://%s/?v=10&encoding=json", resumeURL)
}

func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "closed network connection")
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
	log := dg.Log.Server("gateway.discord", config.F("gateway", "discord"))
	if msg.Author.Bot {
		return
	}
	requestID := config.NewRequestID()

	replyToID := ""
	images, unsupported := dg.loadImages(msg.Attachments)
	embedImageCount := 0
	if len(msg.Attachments) > 0 {
		log.Debug("gateway.attachment.processed", "processed discord attachments", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("accepted_count", len(images)), config.F("downgraded_count", len(unsupported)), config.F("declared_format_count", len(msg.Attachments)))
	}
	if len(msg.Embeds) > 0 {
		embedImages, embedUnsupported := dg.loadEmbedImagesLimit(msg.Embeds, media.MaxImagesPerRequest-len(images))
		embedImageCount = len(embedImages)
		images = append(images, embedImages...)
		unsupported = append(unsupported, embedUnsupported...)
		log.Debug("gateway.embed.processed", "processed discord embeds", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("accepted_count", len(embedImages)), config.F("downgraded_count", len(embedUnsupported)), config.F("declared_embed_count", len(msg.Embeds)))
	}

	mention1 := fmt.Sprintf("<@%s>", dg.BotID)
	mention2 := fmt.Sprintf("<@!%s>", dg.BotID)
	mentionsBot := strings.Contains(msg.Content, mention1) || strings.Contains(msg.Content, mention2)
	if msg.GuildID != "" {
		replyToID = msg.ID
	}

	re := regexp.MustCompile(`<a?:([^:]+):\d+>`)
	text := strings.ReplaceAll(msg.Content, mention1, "")
	text = strings.ReplaceAll(text, mention2, "")
	text = strings.TrimSpace(text)
	text = re.ReplaceAllString(text, ":$1:")
	text = resolveMentions(text, msg.Mentions)
	if embedImageCount > 0 {
		text = stripEmbedURLsFromText(text, msg.Embeds)
	}

	// Compute the session key using the hybrid strategy:
	//   DMs (no GuildID):      SenderID           — continuous per-user memory
	//   Guild channels/threads: ChannelID:SenderID — per-user isolation, prevents cross-talk
	var sessionKey string
	if msg.GuildID == "" {
		sessionKey = "discord:dm:" + msg.Author.ID
	} else {
		sessionKey = "discord:" + msg.ChannelID + ":" + msg.Author.ID
	}
	isReplyToBot := msg.ReferencedMessage != nil && msg.ReferencedMessage.Author.ID == dg.BotID
	if msg.GuildID != "" && !mentionsBot && !isReplyToBot && !isAccountCommand(text) {
		dg.rememberReply(msg.ID, replyContext{
			SessionKey:  sessionKey,
			ChannelID:   msg.ChannelID,
			SenderID:    msg.Author.ID,
			DisplayName: msg.Author.Username,
			Text:        text,
			Attachments: msg.Attachments,
			Embeds:      msg.Embeds,
			IsFromBot:   false,
			CreatedAt:   time.Now(),
		})
		return
	}

	normalizedAuthorID, normErr := accountlink.NormalizeIdentifier("discord", msg.Author.ID)
	if normErr != nil {
		log.Error("gateway.account.normalize_failed", "failed to normalize discord account", config.F("request_id", requestID), config.ErrorField(normErr))
		_, _ = dg.sendMessage(msg.ChannelID, "Sorry, I could not resolve your Discord account identity.", replyToID)
		return
	}

	canonicalUserID, err := dg.Links.EnsureAccount("discord", normalizedAuthorID, msg.Author.Username)
	if err != nil {
		log.Error("gateway.account.resolve_failed", "failed to resolve discord account", config.F("request_id", requestID), config.F("user_id", normalizedAuthorID), config.ErrorField(err))
		_, _ = dg.sendMessage(msg.ChannelID, "Sorry, I could not resolve your account identity.", replyToID)
		return
	}

	var reply *routing.ReplyContext
	if msg.ReferencedMessage != nil {
		reply = dg.resolveReplyContext(msg, re, images, requestID)
	}

	decision := routing.Decide(routing.Input{
		Gateway:            "discord",
		ChatID:             msg.ChannelID,
		SenderID:           canonicalUserID,
		DisplayName:        msg.Author.Username,
		SessionKey:         sessionKey,
		IsDirect:           msg.GuildID == "",
		IsGroup:            msg.GuildID != "",
		MentionsBot:        mentionsBot,
		IsAccountCommand:   isAccountCommand(text),
		Text:               text,
		CurrentImages:      images,
		CurrentUnsupported: unsupported,
		Reply:              reply,
	})
	dg.rememberReply(msg.ID, replyContext{
		SessionKey:  sessionKey,
		ChannelID:   msg.ChannelID,
		SenderID:    msg.Author.ID,
		DisplayName: msg.Author.Username,
		Text:        text,
		Attachments: msg.Attachments,
		Embeds:      msg.Embeds,
		IsFromBot:   false,
		CreatedAt:   time.Now(),
	})
	if decision.Action == routing.ActionIgnore {
		return
	}
	if decision.Action == routing.ActionGatewayFallback {
		_, _ = dg.sendMessage(msg.ChannelID, decision.ResponseText, replyToID)
		return
	}
	if decision.Action == routing.ActionCommand {
		commandResponse, handled, commandErr := dg.Commands.Handle(canonicalUserID, decision.Prompt)
		if !handled {
			return
		}
		if commandErr != nil {
			log.Error("gateway.command.failed", "discord account command failed", config.F("request_id", requestID), config.F("user_id", canonicalUserID), config.ErrorField(commandErr))
			commandResponse = "Failed to process account linking command."
		}
		_, _ = dg.sendMessage(msg.ChannelID, commandResponse, replyToID)
		return
	}

	log.Debug("gateway.request.received", "received discord request", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("session_id", sessionKey), config.F("user_id", canonicalUserID), config.F("image_count", len(decision.Images)), config.F("is_dm", msg.GuildID == ""), config.F("is_reply", msg.ReferencedMessage != nil), config.F("prompt_chars", len(decision.Prompt)))

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
		RequestID:    requestID,
		Channel:      "discord",
		ChatID:       msg.ChannelID,
		SenderID:     canonicalUserID,
		DisplayName:  msg.Author.Username,
		SessionKey:   sessionKey,
		Prompt:       decision.Prompt,
		Images:       decision.Images,
		StreamFunc:   nil,
		ResponseChan: make(chan broker.Result, 1),
	}
	dg.Broker.Submit(req)
	result := <-req.ResponseChan

	if result.Err != nil {
		log.Error("gateway.response.failed", "discord agent processing failed", config.F("request_id", requestID), config.ErrorField(result.Err))
		_, _ = dg.sendMessage(msg.ChannelID, "Sorry, I encountered an internal error processing that.", replyToID)
		return
	}

	finalPayload := result.Response
	responseText := finalPayload.Response
	chunks := splitMessage(responseText, 2000)
	originCtx := replyContext{
		SessionKey:  sessionKey,
		ChannelID:   msg.ChannelID,
		SenderID:    msg.Author.ID,
		DisplayName: "Oswald",
		Text:        responseText,
		IsFromBot:   true,
		CreatedAt:   time.Now(),
	}

	log.Debug("gateway.response.prepared", "prepared discord response", config.F("request_id", requestID), config.F("chunk_count", len(chunks)), config.F("response_chars", len(responseText)), config.F("model", finalPayload.Model))

	sentCount := 0
	for i, chunk := range chunks {
		currentReplyID := ""
		if i == 0 {
			currentReplyID = replyToID
		}

		sentMessageID, err := dg.sendMessage(msg.ChannelID, chunk, currentReplyID)
		if err != nil {
			log.Error("gateway.send.failed", "failed to send discord response chunk", config.F("request_id", requestID), config.F("chunk_index", i+1), config.ErrorField(err))
			break
		}
		sentCount++

		chunkCtx := originCtx
		chunkCtx.Text = chunk
		dg.rememberReply(sentMessageID, chunkCtx)
	}
	if sentCount == len(chunks) {
		log.Debug("gateway.response.sent", "sent discord response", config.F("request_id", requestID), config.F("chunk_count", sentCount), config.F("status", "ok"))
	}
}

func (dg *Gateway) resolveReplyContext(msg MessageCreate, emojiRE *regexp.Regexp, currentImages []ollama.InputImage, requestID string) *routing.ReplyContext {
	log := dg.Log.Server("gateway.discord", config.F("gateway", "discord"))
	referenced := msg.ReferencedMessage
	if referenced == nil {
		return nil
	}

	if cached, ok := dg.lookupReply(referenced.ID); ok {
		reply := &routing.ReplyContext{
			SenderName: strings.TrimSpace(cached.DisplayName),
			Text:       strings.TrimSpace(cached.Text),
			IsFromBot:  cached.IsFromBot,
		}
		if reply.SenderName == "" && cached.IsFromBot {
			reply.SenderName = "Oswald"
		}
		if len(cached.Attachments) > 0 {
			remainingImageSlots := media.MaxImagesPerRequest - len(currentImages)
			if remainingImageSlots > 0 {
				reply.Images, reply.Unsupported = dg.loadImagesLimit(cached.Attachments, remainingImageSlots)
			} else {
				reply.Unsupported = discordAttachmentLabels(cached.Attachments)
			}
		}
		if len(cached.Embeds) > 0 {
			remainingImageSlots := media.MaxImagesPerRequest - len(currentImages) - len(reply.Images)
			if remainingImageSlots > 0 {
				embedImages, embedUnsupported := dg.loadEmbedImagesLimit(cached.Embeds, remainingImageSlots)
				reply.Images = append(reply.Images, embedImages...)
				reply.Unsupported = append(reply.Unsupported, embedUnsupported...)
				if len(embedImages) > 0 {
					reply.Text = stripEmbedURLsFromText(reply.Text, cached.Embeds)
				}
			} else {
				reply.Unsupported = append(reply.Unsupported, discordEmbedLabels(cached.Embeds)...)
			}
		}
		log.Debug("gateway.reply_context.applied", "applied discord cached reply context", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("is_bot_reply", cached.IsFromBot), config.F("reply_image_count", len(reply.Images)))
		return reply
	}

	replyName := strings.TrimSpace(referenced.Author.Username)
	if replyName == "" && referenced.Author.ID == dg.BotID {
		replyName = "Oswald"
	}
	quotedContent := strings.TrimSpace(emojiRE.ReplaceAllString(referenced.Content, ":$1:"))
	reply := &routing.ReplyContext{
		SenderName: replyName,
		Text:       quotedContent,
		IsFromBot:  referenced.Author.ID == dg.BotID,
	}
	if len(referenced.Attachments) > 0 {
		remainingImageSlots := media.MaxImagesPerRequest - len(currentImages)
		if remainingImageSlots > 0 {
			reply.Images, reply.Unsupported = dg.loadImagesLimit(referenced.Attachments, remainingImageSlots)
		} else {
			reply.Unsupported = discordAttachmentLabels(referenced.Attachments)
		}
	}
	if len(referenced.Embeds) > 0 {
		remainingImageSlots := media.MaxImagesPerRequest - len(currentImages) - len(reply.Images)
		if remainingImageSlots > 0 {
			embedImages, embedUnsupported := dg.loadEmbedImagesLimit(referenced.Embeds, remainingImageSlots)
			reply.Images = append(reply.Images, embedImages...)
			reply.Unsupported = append(reply.Unsupported, embedUnsupported...)
			if len(embedImages) > 0 {
				reply.Text = stripEmbedURLsFromText(reply.Text, referenced.Embeds)
			}
		} else {
			reply.Unsupported = append(reply.Unsupported, discordEmbedLabels(referenced.Embeds)...)
		}
	}
	if quotedContent == "" && len(reply.Images) == 0 && len(reply.Unsupported) == 0 {
		if fetched, ok := dg.fetchMessage(msg.ChannelID, referenced.ID, requestID); ok {
			reply.SenderName = strings.TrimSpace(fetched.Author.Username)
			if reply.SenderName == "" && fetched.Author.ID == dg.BotID {
				reply.SenderName = "Oswald"
			}
			reply.Text = strings.TrimSpace(emojiRE.ReplaceAllString(fetched.Content, ":$1:"))
			reply.IsFromBot = fetched.Author.ID == dg.BotID
			if len(fetched.Attachments) > 0 {
				remainingImageSlots := media.MaxImagesPerRequest - len(currentImages)
				if remainingImageSlots > 0 {
					reply.Images, reply.Unsupported = dg.loadImagesLimit(fetched.Attachments, remainingImageSlots)
				} else {
					reply.Unsupported = discordAttachmentLabels(fetched.Attachments)
				}
			}
			if len(fetched.Embeds) > 0 {
				remainingImageSlots := media.MaxImagesPerRequest - len(currentImages) - len(reply.Images)
				if remainingImageSlots > 0 {
					embedImages, embedUnsupported := dg.loadEmbedImagesLimit(fetched.Embeds, remainingImageSlots)
					reply.Images = append(reply.Images, embedImages...)
					reply.Unsupported = append(reply.Unsupported, embedUnsupported...)
					if len(embedImages) > 0 {
						reply.Text = stripEmbedURLsFromText(reply.Text, fetched.Embeds)
					}
				} else {
					reply.Unsupported = append(reply.Unsupported, discordEmbedLabels(fetched.Embeds)...)
				}
			}
		}
	}
	if strings.TrimSpace(reply.Text) == "" && len(reply.Images) == 0 && len(reply.Unsupported) == 0 {
		reply.IsUnavailable = true
	}
	status := "ok"
	if reply.IsUnavailable {
		status = "degraded"
	}
	log.Debug("gateway.reply_context.applied", "applied discord reply context", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("is_bot_reply", reply.IsFromBot), config.F("reply_image_count", len(reply.Images)), config.F("status", status))
	return reply
}

func (dg *Gateway) fetchMessage(channelID, messageID, requestID string) (messageResponse, bool) {
	log := dg.Log.Server("gateway.discord", config.F("gateway", "discord"))
	if strings.TrimSpace(channelID) == "" || strings.TrimSpace(messageID) == "" {
		return messageResponse{}, false
	}

	url := fmt.Sprintf("%s/channels/%s/messages/%s", apiBaseURL, channelID, messageID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to build discord reply lookup request", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	req.Header.Set("Authorization", "Bot "+dg.Token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to fetch discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to read discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Debug("gateway.reply_lookup.failed", "discord reply lookup failed", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("http_status", resp.StatusCode), config.F("status", "degraded"), config.F("body_preview", trimResponseBody(respBody)))
		return messageResponse{}, false
	}

	var result messageResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to decode discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	log.Debug("gateway.reply_lookup.fetched", "fetched discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("attachment_count", len(result.Attachments)), config.F("status", "ok"))
	return result, true
}

func isAccountCommand(input string) bool {
	trimmed := strings.TrimSpace(input)
	return strings.HasPrefix(trimmed, "/connect") || strings.HasPrefix(trimmed, "/disconnect")
}

func (dg *Gateway) loadImages(attachments []Attachment) ([]ollama.InputImage, []string) {
	return dg.loadImagesLimit(attachments, media.MaxImagesPerRequest)
}

func (dg *Gateway) loadImagesLimit(attachments []Attachment, maxImages int) ([]ollama.InputImage, []string) {
	if len(attachments) == 0 {
		return nil, nil
	}
	if maxImages <= 0 {
		return nil, discordAttachmentLabels(attachments)
	}

	images := make([]ollama.InputImage, 0, len(attachments))
	unsupported := make([]string, 0)
	for _, attachment := range attachments {
		label := media.AttachmentLabel(attachment.Filename, attachment.ContentType)
		if len(images) >= maxImages {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.ContentType != "" && !media.LooksLikeImageMIME(attachment.ContentType) {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.Size > media.MaxImageBytes {
			unsupported = append(unsupported, label)
			continue
		}

		image, err := dg.fetchAttachmentImage(attachment.ID, attachment.URL, attachment.ContentType, attachment.Filename)
		if err != nil {
			dg.Log.Server("gateway.discord", config.F("gateway", "discord")).Warn("gateway.attachment.rejected", "rejected discord attachment", config.F("filename", attachment.Filename), config.F("status", "degraded"), config.ErrorField(err))
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

func discordAttachmentLabels(attachments []Attachment) []string {
	labels := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		labels = append(labels, media.AttachmentLabel(attachment.Filename, attachment.ContentType))
	}
	return labels
}

func (dg *Gateway) loadEmbedImagesLimit(embeds []Embed, maxImages int) ([]ollama.InputImage, []string) {
	if len(embeds) == 0 {
		return nil, nil
	}
	if maxImages <= 0 {
		return nil, discordEmbedLabels(embeds)
	}

	images := make([]ollama.InputImage, 0, len(embeds))
	unsupported := make([]string, 0)
	for _, embed := range embeds {
		label := discordEmbedLabel(embed)
		if len(images) >= maxImages {
			unsupported = append(unsupported, label)
			continue
		}

		assetURL := discordEmbedImageURL(embed)
		if assetURL == "" {
			continue
		}
		image, err := dg.fetchAttachmentImage("", assetURL, "", label)
		if err != nil {
			dg.Log.Server("gateway.discord", config.F("gateway", "discord")).Warn("gateway.embed.rejected", "rejected discord embed image", config.F("embed_type", strings.TrimSpace(embed.Type)), config.F("status", "degraded"), config.ErrorField(err))
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

func discordEmbedLabels(embeds []Embed) []string {
	labels := make([]string, 0, len(embeds))
	for _, embed := range embeds {
		if discordEmbedImageURL(embed) != "" {
			labels = append(labels, discordEmbedLabel(embed))
		}
	}
	return labels
}

func discordEmbedLabel(embed Embed) string {
	embedType := strings.TrimSpace(embed.Type)
	if embedType == "" {
		embedType = "link"
	}
	return media.AttachmentLabel("discord embed", embedType)
}

func discordEmbedImageURL(embed Embed) string {
	if url := strings.TrimSpace(embed.Image.ProxyURL); url != "" {
		return url
	}
	if url := strings.TrimSpace(embed.Image.URL); url != "" {
		return url
	}
	if url := strings.TrimSpace(embed.Thumbnail.ProxyURL); url != "" {
		return url
	}
	return strings.TrimSpace(embed.Thumbnail.URL)
}

func stripEmbedURLsFromText(text string, embeds []Embed) string {
	for _, rawURL := range discordEmbedSourceURLs(embeds) {
		text = strings.ReplaceAll(text, rawURL, "")
	}
	return strings.Join(strings.Fields(text), " ")
}

func discordEmbedSourceURLs(embeds []Embed) []string {
	urls := make([]string, 0, len(embeds)*5)
	seen := make(map[string]struct{}, len(embeds)*5)
	for _, embed := range embeds {
		for _, rawURL := range []string{
			embed.URL,
			embed.Image.URL,
			embed.Image.ProxyURL,
			embed.Thumbnail.URL,
			embed.Thumbnail.ProxyURL,
		} {
			rawURL = strings.TrimSpace(rawURL)
			if rawURL == "" {
				continue
			}
			if _, ok := seen[rawURL]; ok {
				continue
			}
			seen[rawURL] = struct{}{}
			urls = append(urls, rawURL)
		}
	}
	return urls
}

func (dg *Gateway) fetchAttachmentImage(attachmentID, rawURL, declaredMIME, filename string) (ollama.InputImage, error) {
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		dg.Log.Server("gateway.discord", config.F("gateway", "discord")).Warn("gateway.attachment.fetch_failed", "failed to fetch discord attachment", config.F("filename", filename), config.F("http_status", resp.StatusCode), config.F("status", "degraded"), config.F("body_preview", strings.TrimSpace(string(body))))
		return ollama.InputImage{}, fmt.Errorf("download attachment %q: unexpected status %d", filename, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("read attachment %q: %w", filename, err)
	}
	if len(body) > media.MaxImageBytes {
		return ollama.InputImage{}, fmt.Errorf("attachment %q exceeds %d bytes", filename, media.MaxImageBytes)
	}

	result, err := media.NormalizeInputImageFromBytes(resp.Header, declaredMIME, body, filename)
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("attachment %q rejected: %w", filename, err)
	}
	dg.Log.Server("gateway.discord", config.F("gateway", "discord")).Debug("gateway.attachment.normalized", "normalized discord attachment", config.F("filename", filename), config.F("attachment_id", attachmentID), config.F("declared_mime", strings.TrimSpace(declaredMIME)), config.F("detected_mime", result.DetectedMIME), config.F("normalized_mime", result.Image.MimeType), config.F("content_chars", len(body)), config.F("width", result.Width), config.F("height", result.Height), config.F("preserved_alpha", result.PreservedAlpha), config.F("used_declared_mime", result.UsedDeclaredMIME))
	return result.Image, nil
}

func attachmentFormats(attachments []Attachment) string {
	formats := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		format := strings.TrimSpace(attachment.ContentType)
		if format == "" {
			format = "unknown"
		}
		formats = append(formats, format)
	}
	return strings.Join(formats, ",")
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		dg.Log.Server("gateway.discord", config.F("gateway", "discord")).Warn("gateway.typing.failed", "discord typing request failed", config.F("chat_id", channelID), config.F("http_status", resp.StatusCode), config.F("status", "degraded"), config.F("body_preview", strings.TrimSpace(string(body))))
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
		dg.Log.Server("gateway.discord", config.F("gateway", "discord")).Warn("gateway.send.failed", "discord send request failed", config.F("chat_id", channelID), config.F("http_status", resp.StatusCode), config.F("status", "error"), config.F("body_preview", trimResponseBody(respBody)))
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

func trimResponseBody(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) <= 512 {
		return trimmed
	}
	return trimmed[:512] + "..."
}
