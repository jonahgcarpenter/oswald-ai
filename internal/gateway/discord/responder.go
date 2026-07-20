package discord

import (
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type runtimeResponder struct {
	gateway    *Gateway
	requestID  string
	channelID  string
	replyToID  string
	sessionKey string
	authorID   string
}

func (r *runtimeResponder) StartProcessing() (func(), error) {
	stopTyping := make(chan struct{})
	go func() {
		_ = r.gateway.sendTyping(r.channelID)
		ticker := time.NewTicker(9 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				_ = r.gateway.sendTyping(r.channelID)
			case <-stopTyping:
				return
			}
		}
	}()
	return func() { close(stopTyping) }, nil
}

func (r *runtimeResponder) SendFallback(text string) error {
	_, err := r.gateway.sendMessage(r.channelID, text, r.replyToID)
	return err
}

func (r *runtimeResponder) SendCommandResponse(result commands.Result) error {
	if err := result.ValidateAttachments(); err != nil {
		return err
	}
	attachments := result.OrderedAttachments()
	if len(attachments) == 0 {
		_, err := r.gateway.sendMessage(r.channelID, result.Text, r.replyToID)
		return err
	}
	for i := range attachments {
		replyToID := ""
		if i == 0 {
			replyToID = r.replyToID
		}
		attachmentResult := commands.Result{Attachment: &attachments[i]}
		if _, err := r.gateway.sendCommandAttachment(r.channelID, attachmentResult, replyToID); err != nil {
			return err
		}
	}
	if result.Text != "" {
		_, err := r.gateway.sendMessage(r.channelID, result.Text, "")
		return err
	}
	return nil
}

func (r *runtimeResponder) SendAgentError(text string) error {
	_, err := r.gateway.sendMessage(r.channelID, text, r.replyToID)
	return err
}

func (r *runtimeResponder) SendAgentResponse(response *agent.AgentResponse) error {
	if response == nil {
		return nil
	}

	responseText := response.Response
	chunks := splitMessage(responseText, 2000)
	originCtx := replyContext{
		SessionKey:  r.sessionKey,
		ChannelID:   r.channelID,
		SenderID:    r.authorID,
		DisplayName: "Oswald",
		Text:        responseText,
		IsFromBot:   true,
		CreatedAt:   time.Now(),
	}

	log := r.gateway.log()
	log.Debug("gateway.response.prepared", "prepared discord response", config.F("request_id", r.requestID), config.F("chunk_count", len(chunks)), config.F("response_chars", len(responseText)), config.F("model", response.Model))

	sentCount := 0
	for i, chunk := range chunks {
		currentReplyID := ""
		if i == 0 {
			currentReplyID = r.replyToID
		}

		sentMessageID, err := r.gateway.sendMessage(r.channelID, chunk, currentReplyID)
		if err != nil {
			log.Error("gateway.send.failed", "failed to send discord response chunk", config.F("request_id", r.requestID), config.F("chunk_index", i+1), config.ErrorField(err))
			return err
		}
		sentCount++

		chunkCtx := originCtx
		chunkCtx.Text = chunk
		r.gateway.rememberReply(sentMessageID, chunkCtx)
	}
	if sentCount == len(chunks) {
		log.Debug("gateway.response.sent", "sent discord response", config.F("request_id", r.requestID), config.F("chunk_count", sentCount), config.F("status", "ok"))
	}
	return nil
}
