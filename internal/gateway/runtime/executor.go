package runtime

import (
	"context"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/usermanagement"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// Execute applies shared routing policy, command handling, and broker submission.
func Execute(req Request, deps Dependencies, responder Responder) Outcome {
	log := deps.Log.Server("gateway.runtime", config.F("gateway", req.Principal.Gateway))
	decision := routing.Decide(routing.Input{
		IsDirect:           req.IsDirect,
		IsGroup:            req.IsGroup,
		IsMention:          req.IsMention,
		IsReplyToBot:       req.IsReplyToBot,
		IsCommandAttempt:   commands.IsAttempt(req.Text),
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
	if !req.Principal.Valid() {
		err := responder.SendAgentError("Failed to resolve account identity")
		log.Error("gateway.account.invalid_principal", "request has no valid principal", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID))
		return Outcome{Action: decision.Action, Reason: "invalid_principal", Err: err}
	}
	userID := req.Principal.CanonicalUserID

	if deps.Access != nil {
		isBanned, banReason, err := deps.Access.BanStatus(userID)
		if err != nil {
			log.Error("gateway.access_check.failed", "failed to check user access", config.F("request_id", req.RequestID), config.F("user_id", userID), config.ErrorField(err))
			sendErr := responder.SendAgentError(config.SafeErrorText(err))
			if sendErr != nil {
				log.Error("gateway.send.failed", "failed to send access error response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(sendErr))
			}
			return Outcome{Action: decision.Action, Reason: "access_check_failed", Err: err}
		}
		if isBanned {
			err := responder.SendFallback(usermanagement.BannedMessage(banReason))
			if err != nil {
				log.Error("gateway.response.failed", "failed to send banned response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(err))
			}
			return Outcome{Action: decision.Action, Reason: "user_banned", Err: err}
		}
	}

	if decision.Action == routing.ActionCommand {
		startedAt := time.Now()
		parsed, _ := commands.Parse(decision.Prompt)
		commandName := parsed.Name
		if commandName == "" {
			commandName = "unknown"
		}
		response := "Unknown command: /"
		var err error
		if deps.Commands != nil {
			executeCommand := func() error {
				result, commandErr := deps.Commands.Execute(context.Background(), commands.Request{
					RequestID: req.RequestID, Principal: req.Principal, ChatID: req.ChatID,
					SessionKey: req.SessionKey, DisplayName: req.DisplayName,
					IsDirect: req.IsDirect, IsGroup: req.IsGroup, Raw: decision.Prompt,
				})
				response = result.Text
				return commandErr
			}
			if deps.Broker != nil {
				err = deps.Broker.RunInLane(context.Background(), req.Principal, req.SessionKey, executeCommand)
			} else {
				err = executeCommand()
			}
		}
		if err != nil {
			log.Error("gateway.command.failed", "command failed", config.F("request_id", req.RequestID), config.F("user_id", userID), config.ErrorField(err))
			response = config.SafeErrorText(err)
		}
		sendErr := responder.SendCommandResponse(response)
		if sendErr != nil {
			log.Error("gateway.response.failed", "failed to send command response", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.ErrorField(sendErr))
		}
		status := "ok"
		if err != nil || sendErr != nil {
			status = "error"
		}
		log.Info("gateway.command.completed", "completed gateway command",
			config.F("request_id", req.RequestID),
			config.F("chat_id", req.ChatID),
			config.F("session_id", req.SessionKey),
			config.F("user_id", userID),
			config.F("command", commandName),
			config.F("response_chars", len(response)),
			config.F("duration_ms", time.Since(startedAt).Milliseconds()),
			config.F("status", status),
		)
		return Outcome{Action: decision.Action, Reason: decision.Reason, Err: sendErr}
	}

	cleanup, err := responder.StartProcessing()
	if err != nil {
		log.Debug("gateway.processing.start_failed", "failed to start gateway processing indicator", config.F("request_id", req.RequestID), config.F("chat_id", req.ChatID), config.F("status", "degraded"), config.ErrorField(err))
	}
	if cleanup != nil {
		defer cleanup()
	}

	log.Info("gateway.request.received", "received gateway request",
		config.F("request_id", req.RequestID),
		config.F("chat_id", req.ChatID),
		config.F("session_id", req.SessionKey),
		config.F("user_id", userID),
		config.F("identity_assurance", req.Principal.Assurance),
		config.F("image_count", len(decision.Images)),
		config.F("is_group", req.IsGroup),
		config.F("is_mention", req.IsMention),
		config.F("is_reply", req.Reply != nil),
		config.F("prompt_chars", len(decision.Prompt)),
	)

	brokerReq := &broker.Request{
		RequestID:    req.RequestID,
		ChatID:       req.ChatID,
		Principal:    req.Principal,
		DisplayName:  req.DisplayName,
		SessionKey:   req.SessionKey,
		IsDirect:     req.IsDirect,
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
	} else if result.Response != nil {
		log.Info("gateway.response.sent", "sent gateway response",
			config.F("request_id", req.RequestID),
			config.F("chat_id", req.ChatID),
			config.F("session_id", req.SessionKey),
			config.F("user_id", userID),
			config.F("response_chars", len(result.Response.Response)),
			config.F("status", "ok"),
		)
		if deps.Formation != nil && result.Response.PendingConfirmationCandidateID > 0 {
			if presentErr := deps.Formation.MarkConfirmationPresented(context.Background(), userID, req.SessionKey, result.Response.SessionGeneration, req.RequestID, result.Response.PendingConfirmationCandidateID); presentErr != nil {
				log.Warn("memory.confirmation.present_failed", "failed to mark delivered memory confirmation", config.F("request_id", req.RequestID), config.F("user_id", userID), config.F("candidate_id", result.Response.PendingConfirmationCandidateID), config.F("status", "degraded"), config.ErrorField(presentErr))
			}
		}
		if deps.Formation != nil && result.Response.SourceTurnID > 0 {
			source := usermemory.FormationSource{
				RequestID: req.RequestID, SessionID: req.SessionKey,
				SessionGeneration: result.Response.SessionGeneration,
				TurnID:            result.Response.SourceTurnID, Model: result.Response.Model,
				ExtractorVersion: usermemory.FormationExtractorVersion,
			}
			if enqueueErr := deps.Formation.Enqueue(context.Background(), userID, source); enqueueErr != nil {
				log.Warn("memory.formation.job.enqueue_failed", "failed to enqueue post-turn memory formation", config.F("request_id", req.RequestID), config.F("user_id", userID), config.F("turn_id", result.Response.SourceTurnID), config.F("status", "degraded"), config.ErrorField(enqueueErr))
			}
		}
	}
	return Outcome{Action: decision.Action, Reason: decision.Reason, Err: err}
}
