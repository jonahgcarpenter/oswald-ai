package imessage

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	toolclarify "github.com/jonahgcarpenter/oswald-ai/internal/tools/clarify"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
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
	defer r.scheduleBackupStop()
	return r.sendAndRemember(text)
}

func (r *runtimeResponder) SendCommandResponse(result commands.Result) error {
	defer r.scheduleBackupStop()
	if err := result.ValidateAttachments(); err != nil {
		return err
	}
	if strings.TrimSpace(result.Text) == "" && len(result.Attachments) == 0 {
		return nil
	}
	attachments := result.Attachments
	if len(attachments) == 0 {
		return r.sendAndRemember(result.Text)
	}
	if strings.TrimSpace(result.Text) != "" {
		if err := r.sendAndRemember(result.Text); err != nil {
			return err
		}
	}
	for _, attachment := range attachments {
		if err := r.gateway.sendCommandAttachment(r.chatGUID, r.selectedMessageGUID, attachment); err != nil {
			return err
		}
	}
	return nil
}

func (r *runtimeResponder) SendAgentError(text string) error {
	defer r.scheduleBackupStop()
	return r.sendAndRemember(text)
}

func (r *runtimeResponder) CancelAgentResponse() error {
	// A cancel sends no reply, so the server cannot auto-clear: delete
	// deliberately now, with the usual backup unless a new turn supersedes it.
	r.cancelTyping()
	return nil
}

func (r *runtimeResponder) SendAgentResponse(response *agent.Response) error {
	defer r.scheduleBackupStop()
	if response == nil {
		return nil
	}
	if err := media.ValidateOutputAttachments(response.Attachments); err != nil {
		return err
	}
	responseText := strings.TrimSpace(response.Response)
	if responseText != "" {
		if err := r.sendAndRemember(responseText); err != nil {
			return err
		}
	}
	for _, attachment := range response.Attachments {
		if err := r.gateway.sendCommandAttachment(r.chatGUID, r.selectedMessageGUID, attachment); err != nil {
			return err
		}
	}
	if responseText == "" {
		r.gateway.log().Debug("gateway.response.empty", "imessage agent returned empty response", config.F("request_id", r.requestID), config.F("chat_id", r.chatGUID), config.F("status", "degraded"))
		return nil
	}
	return nil
}

// scheduleBackupStop arms the single precautionary typing DELETE after a
// terminal send. The server is assumed to clear the indicator when the reply
// lands; the backup only catches the stuck case. It waits for the delayed
// start attempt first so a fast command or fallback cannot DELETE before the
// pending POST fires, then runs off the send path and never fails delivery.
// Steer and queue follow-ups never call this — only the active turn's send.
func (r *runtimeResponder) scheduleBackupStop() {
	gate, allowed := r.typingGate, r.typingAllowed
	if allowed == nil {
		// Never admitted, so nothing started.
		return
	}
	go func() {
		select {
		case <-allowed:
			gate.wait()
		}
		r.gateway.scheduleBackupTypingStop(r.chatGUID, r.requestID)
	}()
}

// cancelTyping sends one deliberate typing DELETE for a cancel. No reply is
// sent on this path, so the server cannot auto-clear.
func (r *runtimeResponder) cancelTyping() {
	gate, allowed := r.typingGate, r.typingAllowed
	if allowed != nil {
		select {
		case <-allowed:
			gate.wait()
		}
	}
	log := r.gateway.log().With(config.F("request_id", r.requestID))
	if err := r.gateway.stopTypingNow(r.chatGUID, log); err != nil {
		log.Debug("gateway.typing.stop_failed", "failed to stop BlueBubbles typing indicator on cancel", config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	log.Debug("gateway.typing.stop", "deliberately stopped typing indicator on cancel", config.F("status", "ok"))
	r.gateway.scheduleBackupTypingStop(r.chatGUID, r.requestID)
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

// approvalAskNotifier reports a parked approval ask as a standalone reply
// while the tool call blocks. It fires once per park (single-flight) from a
// lane worker; delivery is best-effort and never unblocks the waiter.
func (g *Gateway) approvalAskNotifier(r *runtimeResponder, requestID string) workspace.EscalationNotifier {
	return func(notice workspace.EscalationNotice) {
		text := workspace.EscalationStatusText(notice.Reason, notice.Command)
		if notice.Expired {
			text = workspace.EscalationExpiredText(notice.Reason)
		}
		if err := r.sendAndRemember(text); err != nil {
			g.log().With(config.F("request_id", requestID)).Debug("gateway.approval_ask.failed", "failed to send approval ask", config.ErrorField(err))
		}
	}
}

// clarifyAskNotifier reports a parked clarify ask as a standalone reply
// while the tool call blocks. Delivery failures are returned so the waiter
// can finalize as undelivered; they never unblock the waiter otherwise.
func (g *Gateway) clarifyAskNotifier(r *runtimeResponder, requestID string) toolclarify.Notifier {
	return func(notice toolclarify.Notice) error {
		text := toolclarify.RenderAskOne(notice.Question, notice.MinutesLeft)
		if notice.Expired {
			text = toolclarify.RenderExpired()
		}
		if err := r.sendAndRemember(text); err != nil {
			g.log().With(config.F("request_id", requestID)).Debug("gateway.clarify_ask.failed", "failed to send clarify ask", config.ErrorField(err))
			return err
		}
		return nil
	}
}
