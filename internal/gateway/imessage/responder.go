package imessage

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

type runtimeResponder struct {
	gateway             *Gateway
	requestID           string
	chatGUID            string
	selectedMessageGUID string
	sessionKey          string
	senderID            string
	typingGate          *typingGate
	// typingAllowed is closed synchronously by OnAllowed. A stop only waits
	// for the delayed start when admission actually ran; paths that return
	// before OnAllowed never started typing, so they stop immediately.
	typingAllowed chan struct{}
}

func (r *runtimeResponder) StartProcessing() (func(), error) {
	return nil, nil
}

func (r *runtimeResponder) SendFallback(text string) error {
	defer r.stopProcessingIndicators()
	return r.sendAndRemember(text)
}

func (r *runtimeResponder) SendCommandResponse(result commands.Result) error {
	defer r.stopProcessingIndicators()
	if err := result.ValidateAttachments(); err != nil {
		return err
	}
	attachments := result.Attachments
	if len(attachments) == 0 {
		return r.sendAndRemember(result.Text)
	}
	for _, attachment := range attachments {
		if err := r.gateway.sendCommandAttachment(r.chatGUID, attachment); err != nil {
			return err
		}
	}
	if strings.TrimSpace(result.Text) != "" {
		return r.sendAndRemember(result.Text)
	}
	return nil
}

func (r *runtimeResponder) SendAgentError(text string) error {
	defer r.stopProcessingIndicators()
	return r.sendAndRemember(text)
}

func (r *runtimeResponder) CancelAgentResponse() error {
	r.stopProcessingIndicators()
	return nil
}

func (r *runtimeResponder) SendAgentResponse(response *agent.Response) error {
	defer r.stopProcessingIndicators()
	if response == nil {
		return nil
	}
	if err := media.ValidateOutputAttachments(response.Attachments); err != nil {
		return err
	}
	for _, attachment := range response.Attachments {
		if err := r.gateway.sendCommandAttachment(r.chatGUID, attachment); err != nil {
			return err
		}
	}
	responseText := strings.TrimSpace(response.Response)
	if responseText == "" {
		r.gateway.log().Debug("gateway.response.empty", "imessage agent returned empty response", config.F("request_id", r.requestID), config.F("chat_id", r.chatGUID), config.F("status", "degraded"))
		return nil
	}
	return r.sendAndRemember(responseText)
}

// stopProcessingIndicators clears the typing indicator after a send so a
// missed server-side clear cannot leave it stuck. It waits for the delayed
// start attempt first, runs off the send path, and never fails delivery.
func (r *runtimeResponder) stopProcessingIndicators() {
	gate, allowed := r.typingGate, r.typingAllowed
	go func() {
		select {
		case <-allowed:
			gate.wait()
		default:
		}
		log := r.gateway.log().With(config.F("request_id", r.requestID))
		if err := r.gateway.stopTyping(r.chatGUID, log); err != nil {
			log.Debug("gateway.typing.stop_failed", "failed to stop BlueBubbles typing indicator", config.F("status", "degraded"), config.ErrorField(err))
		}
	}()
}

func (r *runtimeResponder) sendAndRemember(text string) error {
	messageGUID, err := r.gateway.sendTextReply(r.chatGUID, text, r.selectedMessageGUID, 0, r.gateway.log().With(config.F("request_id", r.requestID)))
	if err != nil {
		return err
	}
	r.gateway.log().Debug("gateway.response.sent", "sent imessage response", config.F("request_id", r.requestID), config.F("chat_id", r.chatGUID), config.F("response_chars", len(text)))
	r.gateway.rememberBotMessage(messageGUID, r.sessionKey, r.chatGUID, r.senderID, text)
	return nil
}
