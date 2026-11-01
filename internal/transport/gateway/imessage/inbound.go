package imessage

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	toolclarify "github.com/jonahgcarpenter/oswald-ai/internal/tools/clarify"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway/routing"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

// processIncomingMessage normalizes an inbound iMessage and routes it to the broker.
func (g *Gateway) processIncomingMessage(msg webhookMessage) {
	g.processReceivedMessage(msg, config.NewRequestID(), time.Now())
}

func (g *Gateway) processReceivedMessage(msg webhookMessage, requestID string, receivedAt time.Time) {
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: requestID})
	log := g.log().With(config.F("request_id", requestID))
	chat := msg.primaryChat()
	if chat.GUID == "" {
		g.logIgnoredMessage("missing_chat_guid", "new-message", msg, config.F("request_id", requestID))
		return
	}
	if strings.TrimSpace(msg.Handle.Address) == "" {
		g.logIgnoredMessage("missing_sender", "new-message", msg, config.F("request_id", requestID))
		return
	}
	if strings.TrimSpace(msg.Text) == "" && len(msg.Attachments) == 0 {
		g.logIgnoredMessage("empty_text_and_no_attachments", "new-message", msg, config.F("request_id", requestID))
		return
	}

	publicUserText := msg.Text
	text := strings.TrimSpace(msg.Text)
	replyGUID := msg.replyTargetGUID()
	isGroup := chat.Style == chatStyleGroup || strings.Contains(chat.GUID, ";+;")
	principal, err := g.Links.Resolve("imessage", msg.Handle.Address, !isGroup)
	if err != nil {
		return
	}
	selectedMessageGUID := ""
	if isGroup {
		selectedMessageGUID = msg.GUID
	}
	mentionEnd := g.leadingMention(text)
	hasLeadingMention := mentionEnd >= 0
	mentionsBot := hasLeadingMention
	if isGroup && !g.Links.RequiresMention("imessage") {
		mentionsBot = true
	}
	textWithoutMention := text
	if hasLeadingMention {
		textWithoutMention = strings.TrimSpace(text[mentionEnd:])
	}
	currentIsCommandAttempt := routing.IsCommandAttempt(textWithoutMention)
	// Group-thread admission and enrichment share the newest prior message,
	// including when a mention independently permits invocation.
	allowPredecessor := isGroup
	replyLookupAttempted := false
	currentIsReplyToBot := false
	var resolvedReply messageContext
	var replyFound bool
	if replyGUID != "" {
		replyLookupAttempted = true
		resolvedReply, replyFound = g.resolveReply(ctx, msg, allowPredecessor, requestID)
		currentIsReplyToBot = replyFound && resolvedReply.IsFromBot
	}
	preflight := routing.Preflight(routing.PreflightInput{
		IsGroup:      isGroup,
		IsMention:    mentionsBot,
		IsReplyToBot: currentIsReplyToBot,
		Text:         textWithoutMention,
	})
	if preflight.Action == routing.ActionIgnore {
		g.logIgnoredMessage(preflight.Reason, "new-message", msg,
			config.F("request_id", requestID),
			config.F("is_group", isGroup),
			config.F("is_mention", mentionsBot),
			config.F("is_reply", replyGUID != ""),
			config.F("has_thread_root", msg.ThreadOriginatorGUID != ""),
			config.F("has_explicit_target", msg.ReplyToGUID != ""),
			config.F("reply_lookup_attempted", replyLookupAttempted),
			config.F("reply_found", replyFound),
			config.F("reply_is_bot", currentIsReplyToBot),
			config.F("allow_predecessor", allowPredecessor),
			config.F("is_command", currentIsCommandAttempt),
			config.F("message_chars", len(msg.Text)),
		)
		return
	}

	normalizationStarted := time.Now()
	images, unsupported := g.loadImages(msg.Attachments, log)
	documents, docUnsupported := g.loadDocuments(msg.Attachments, principal.CanonicalUserID, log)
	unsupported = append(unsupported, docUnsupported...)
	if len(msg.Attachments) > 0 {
		status := "ok"
		if len(unsupported) > 0 {
			status = "degraded"
		}
		log.Debug("gateway.attachment.processed", "normalized imessage input attachments", config.F("accepted_count", len(images)), config.F("downgraded_count", len(unsupported)), config.F("accepted_document_count", len(documents)), config.F("downgraded_document_count", len(docUnsupported)), config.F("declared_format_count", len(msg.Attachments)), config.F("duration_ms", time.Since(normalizationStarted).Milliseconds()), config.F("status", status))
	}
	if strings.TrimSpace(msg.Text) == "" && len(images) == 0 && len(documents) == 0 {
		if len(unsupported) == 0 {
			g.logIgnoredMessage("no_supported_content", "new-message", msg, config.F("request_id", requestID))
			return
		}
	}

	if strings.TrimSpace(msg.Text) == "" && len(images) == 0 && len(documents) == 0 && len(unsupported) == 0 {
		g.logIgnoredMessage("no_supported_content", "new-message", msg, config.F("request_id", requestID))
		return
	}

	normalizedSenderID := principal.ExternalID
	displayName := normalizedSenderID
	if resolvedName, err := g.lookupContactDisplayName(normalizedSenderID, log); err != nil {
		log.Debug("gateway.contact_lookup.failed", "imessage contact lookup failed", config.F("request_id", requestID), config.F("status", "degraded"), config.ErrorField(err))
	} else if resolvedName != "" {
		displayName = resolvedName
	}

	canonicalUserID := principal.CanonicalUserID

	sessionKey := g.sessionKey(chat, normalizedSenderID)
	rememberCronChat(sessionKey, chat.GUID)
	log = log.With(config.F("profile", canonicalUserID))
	// A parked clarify ask owns this conversation until answered: free-text
	// replies fulfill it instead of starting a new turn. Commands flow
	// through so /stop and /new still cancel the wait.
	if !currentIsCommandAttempt && strings.TrimSpace(textWithoutMention) != "" && toolclarify.Fulfill(canonicalUserID, sessionKey, textWithoutMention) {
		log.Debug("gateway.clarify.fulfilled", "delivered user reply to parked clarify ask", config.F("request_id", requestID), config.F("message_chars", len(textWithoutMention)))
		return
	}
	var reply *routing.ReplyContext
	if replyGUID != "" {
		if replyFound {
			replyCtx := resolvedReply
			// Contact enrichment is deliberately after admission, never a lookup
			// side effect of an ignored group thread message.
			if !replyCtx.IsFromBot && replyCtx.SenderID != "" && replyCtx.SenderID != "imessage:self" {
				if name, err := g.lookupContactDisplayName(replyCtx.SenderID, log); err == nil && name != "" {
					replyCtx.DisplayName = name
				}
			}
			replyName := strings.TrimSpace(replyCtx.DisplayName)
			if replyName == "" && replyCtx.IsFromBot {
				replyName = "Oswald"
			}
			reply = &routing.ReplyContext{
				SenderName: replyName,
				Text:       strings.TrimSpace(replyCtx.Text),
				IsFromBot:  replyCtx.IsFromBot,
			}
			if len(replyCtx.Attachments) > 0 {
				realAttachments := filterRealAttachments(replyCtx.Attachments, "")
				if len(realAttachments) == 0 {
					reply.Unsupported = nil
				} else if remainingImageSlots := media.MaxImagesPerRequest - len(images); remainingImageSlots > 0 {
					reply.Images, reply.Unsupported = g.loadImagesLimit(realAttachments, remainingImageSlots, log)
				} else {
					reply.Unsupported = attachmentLabels(realAttachments)
				}
			}
			log.Debug("gateway.reply_context.applied", "applied imessage reply context", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("is_bot_reply", replyCtx.IsFromBot), config.F("reply_image_count", len(reply.Images)))
		} else {
			log.Debug("gateway.reply_context.applied", "imessage reply target missing from cache", config.F("request_id", requestID), config.F("status", "degraded"))
			reply = &routing.ReplyContext{IsUnavailable: true}
		}
	}
	g.rememberInboundMessage(msg, sessionKey, normalizedSenderID, displayName)

	// The plain conversation name rides alongside the formatted chat label
	// for the session origin record: peer name in DMs, chat name in groups.
	chatDisplayName := displayName
	if isGroup {
		if name := g.chatName(chat.GUID); name != "" {
			chatDisplayName = name
		}
	}
	responder := &runtimeResponder{
		gateway:             g,
		requestID:           requestID,
		chatGUID:            chat.GUID,
		selectedMessageGUID: selectedMessageGUID,
		sessionKey:          sessionKey,
		senderID:            normalizedSenderID,
		typingGate:          newTypingGate(),
		typingAllowed:       make(chan struct{}),
	}
	// iMessage has no live stream: a parked approval ask goes out as a real
	// message so the user learns a decision is needed while the tool call
	// blocks. Streaming transports leave this unset and render the agent's
	// status chunk instead.
	askNotify := g.approvalAskNotifier(responder, requestID)
	clarifyAsk := g.clarifyAskNotifier(responder, requestID)
	gatewayruntime.Execute(gatewayruntime.Request{
		ReceivedAt: receivedAt,
		RequestID:  requestID,
		ChatID:     chat.GUID,
		Principal: identity.Principal{
			CanonicalUserID: canonicalUserID,
			Gateway:         "imessage",
			ExternalID:      normalizedSenderID,
			Assurance:       identity.AssuranceBlueBubblesWebhook,
		},
		DisplayName:       displayName,
		SessionKey:        sessionKey,
		ChatLabel:         g.chatLabel(chat, displayName, isGroup, log),
		IsDirect:          !isGroup,
		IsGroup:           isGroup,
		IsMention:         mentionsBot,
		IsReplyToBot:      currentIsReplyToBot,
		Text:              textWithoutMention,
		EscalationNotify:  askNotify,
		ClarifyAsk:        clarifyAsk,
		PublicUserText:    publicUserText,
		PlatformMessageID: msg.GUID,
		ChatDisplayName:   chatDisplayName,
		// Adapters are process-global: the receiving credential always
		// belongs to the reserved default profile.
		TransportProfile: "default",
		Images:           images,
		Documents:        documents,
		Unsupported:      unsupported,
		Reply:            reply,
		OnAllowed: func() {
			close(responder.typingAllowed)
			g.startProcessingIndicators(chat.GUID, requestID, responder.typingGate)
		},
	}, g.runtimeDependencies(), responder)
}

// typingGate orders the delayed typing start before the stop that follows a
// send: the stop waits for the start attempt to finish so a fast
// command/fallback cannot DELETE before the pending POST fires.
type typingGate struct {
	done chan struct{}
	once sync.Once
}

func newTypingGate() *typingGate {
	return &typingGate{done: make(chan struct{})}
}

func (t *typingGate) finish() {
	if t == nil {
		return
	}
	t.once.Do(func() { close(t.done) })
}

func (t *typingGate) wait() {
	if t == nil {
		return
	}
	<-t.done
}

func (g *Gateway) startProcessingIndicators(chatGUID, requestID string, gate *typingGate) {
	log := g.log().With(config.F("request_id", requestID))
	g.trackTypingActive(chatGUID)
	go func() {
		defer gate.finish()
		g.markRead(chatGUID, log)
		// Fire immediately on admission: the indicator must cover the whole
		// pipeline, not just the agent turn. A new POST also supersedes the
		// previous turn's pending backup stop for this chat.
		if err := g.startTyping(chatGUID, log); err != nil {
			log.Debug("gateway.typing.failed", "failed to start BlueBubbles typing indicator", config.F("status", "degraded"), config.ErrorField(err))
			return
		}
		g.cancelPendingBackup(chatGUID)
	}()
}

// sessionKey returns the session identifier for a direct or group iMessage chat.
func (g *Gateway) sessionKey(chat messageChat, normalizedSenderID string) string {
	if chat.Style == chatStyleDirect {
		return "imessage:dm:" + normalizedSenderID
	}
	return "imessage:" + chat.GUID + ":" + normalizedSenderID
}

// primaryChat returns the first chat attached to the webhook payload.
func (m webhookMessage) primaryChat() messageChat {
	if len(m.Chats) == 0 {
		return messageChat{}
	}
	return m.Chats[0]
}

// defaultMentionPatterns preserves the built-in mention forms. A mention must
// be the first thing the sender says, so the patterns are anchored.
var defaultMentionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(?:<@Oswald>|@?Oswald\b)`),
}

// leadingMention reports whether the trimmed message begins with a mention and,
// if so, the byte offset just past the longest matching prefix.
func (g *Gateway) leadingMention(trimmed string) int {
	patterns := g.MentionPatterns
	if len(patterns) == 0 {
		patterns = defaultMentionPatterns
	}
	end := -1
	for _, pattern := range patterns {
		if loc := pattern.FindStringIndex(trimmed); loc != nil && loc[0] == 0 && loc[1] > end {
			end = loc[1]
		}
	}
	return end
}
