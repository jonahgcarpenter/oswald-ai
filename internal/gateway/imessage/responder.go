package imessage

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type runtimeResponder struct {
	gateway             *Gateway
	requestID           string
	chatGUID            string
	selectedMessageGUID string
	sessionKey          string
	senderID            string
}

func (r *runtimeResponder) StartProcessing() (func(), error) {
	return nil, nil
}

func (r *runtimeResponder) SendFallback(text string) error {
	return r.sendAndRemember(text)
}

func (r *runtimeResponder) SendCommandResponse(text string) error {
	return r.sendAndRemember(text)
}

func (r *runtimeResponder) SendAgentError(text string) error {
	return r.sendAndRemember(text)
}

func (r *runtimeResponder) SendAgentResponse(response *agent.AgentResponse) error {
	if response == nil {
		return nil
	}
	responseText := strings.TrimSpace(response.Response)
	if responseText == "" {
		r.gateway.Log.Server("gateway.imessage", config.F("gateway", "imessage")).Debug("gateway.response.empty", "imessage agent returned empty response", config.F("request_id", r.requestID), config.F("chat_id", r.chatGUID), config.F("status", "degraded"))
		return nil
	}
	return r.sendAndRemember(responseText)
}

func (r *runtimeResponder) sendAndRemember(text string) error {
	messageGUID, err := r.gateway.sendTextReply(r.chatGUID, text, r.selectedMessageGUID, 0)
	if err != nil {
		return err
	}
	r.gateway.Log.Server("gateway.imessage", config.F("gateway", "imessage")).Debug("gateway.response.sent", "sent imessage response", config.F("request_id", r.requestID), config.F("chat_id", r.chatGUID), config.F("response_chars", len(text)), config.F("status", "ok"))
	r.gateway.rememberBotMessage(messageGUID, r.sessionKey, r.chatGUID, r.senderID, text)
	return nil
}
