package runtime

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	admincmd "github.com/jonahgcarpenter/oswald-ai/internal/commands/admin"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
)

// Execute applies shared routing policy, command handling, and broker submission.
func Execute(req Request, deps Dependencies, responder Responder) Outcome {
	log := deps.Log.Server("gateway.runtime", config.F("gateway", req.Gateway))
	decision := routing.Decide(routing.Input{
		Gateway:            req.Gateway,
		ChatID:             req.ChatID,
		SenderID:           req.SenderID,
		DisplayName:        req.DisplayName,
		SessionKey:         req.SessionKey,
		IsDirect:           req.IsDirect,
		IsGroup:            req.IsGroup,
		IsMention:          req.IsMention,
		IsReplyToBot:       req.IsReplyToBot,
		IsCommand:          req.IsCommand,
		Text:               req.Text,
		CurrentImages:      req.Images,
		CurrentUnsupported: req.Unsupported,
		Reply:              req.Reply,
	})

	switch decision.Action {
	case routing.ActionIgnore:
		return Outcome{Action: decision.Action, Reason: decision.Reason}
	case routing.ActionGatewayFallback:
		err := responder.SendFallback(decision.ResponseText)
		if err != nil {
			log.Error("gateway.response.failed", "failed to send gateway fallback", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(err))
		}
		return Outcome{Action: decision.Action, Reason: decision.Reason, Err: err}
	}

	if deps.Access != nil && req.SenderID != "" {
		isBanned, banReason, err := deps.Access.BanStatus(req.SenderID)
		if err != nil {
			log.Error("gateway.access_check.failed", "failed to check user access", config.F("request_id", req.RequestID), config.F("user_id", req.SenderID), config.ErrorField(err))
			sendErr := responder.SendAgentError(config.SafeErrorText(err))
			if sendErr != nil {
				log.Error("gateway.send.failed", "failed to send access error response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(sendErr))
			}
			return Outcome{Action: decision.Action, Reason: "access_check_failed", Err: err}
		}
		if isBanned {
			err := responder.SendFallback(admincmd.BannedMessage(banReason))
			if err != nil {
				log.Error("gateway.response.failed", "failed to send banned response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(err))
			}
			return Outcome{Action: decision.Action, Reason: "user_banned", Err: err}
		}
	}

	if decision.Action == routing.ActionCommand {
		response, handled, err := deps.Commands.Handle(req.SenderID, decision.Prompt)
		if !handled {
			return Outcome{Action: decision.Action, Reason: decision.Reason}
		}
		if err != nil {
			log.Error("gateway.command.failed", "command failed", config.F("request_id", req.RequestID), config.F("user_id", req.SenderID), config.ErrorField(err))
			response = config.SafeErrorText(err)
		}
		sendErr := responder.SendCommandResponse(response)
		if sendErr != nil {
			log.Error("gateway.response.failed", "failed to send command response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(sendErr))
		}
		return Outcome{Action: decision.Action, Reason: decision.Reason, Err: sendErr}
	}

	cleanup, err := responder.StartProcessing()
	if err != nil {
		log.Debug("gateway.processing.start_failed", "failed to start gateway processing indicator", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.F("status", "degraded"), config.ErrorField(err))
	}
	if cleanup != nil {
		defer cleanup()
	}

	log.Debug("gateway.request.received", "received gateway request",
		config.F("request_id", req.RequestID),
		config.F("chat_id", req.ChatID),
		config.F("session_id", req.SessionKey),
		config.F("user_id", req.SenderID),
		config.F("image_count", len(decision.Images)),
		config.F("is_group", req.IsGroup),
		config.F("is_mention", req.IsMention),
		config.F("is_reply", req.Reply != nil),
		config.F("prompt_chars", len(decision.Prompt)),
	)

	brokerReq := &broker.Request{
		RequestID:    req.RequestID,
		Channel:      req.Gateway,
		ChatID:       req.ChatID,
		SenderID:     req.SenderID,
		DisplayName:  req.DisplayName,
		SessionKey:   req.SessionKey,
		Prompt:       decision.Prompt,
		Images:       decision.Images,
		StreamFunc:   req.StreamFunc,
		ResponseChan: make(chan broker.Result, 1),
	}
	deps.Broker.Submit(brokerReq)
	result := <-brokerReq.ResponseChan

	if result.Err != nil {
		log.Error("gateway.response.failed", "agent processing failed", config.F("request_id", req.RequestID), config.ErrorField(result.Err))
		err := responder.SendAgentError(config.SafeErrorText(result.Err))
		if err != nil {
			log.Error("gateway.send.failed", "failed to send agent error response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(err))
		}
		return Outcome{Action: decision.Action, Reason: decision.Reason, Err: result.Err}
	}

	err = responder.SendAgentResponse(result.Response)
	if err != nil {
		log.Error("gateway.send.failed", "failed to send agent response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(err))
	}
	return Outcome{Action: decision.Action, Reason: decision.Reason, Err: err}
}
