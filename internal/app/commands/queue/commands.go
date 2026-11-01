package queue

import (
	"context"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// Submitter enqueues one agent turn as a FIFO follower.
type Submitter interface {
	Submit(*broker.Request) error
}

type handler struct{ broker Submitter }

// New constructs the queue command. Plain messages steer into the running
// turn by default; /queue forces the prompt to wait for the next turn.
func New(submitter Submitter) commands.Handler { return handler{broker: submitter} }

func (handler) Definition() commands.Definition {
	return commands.Definition{Name: "queue", Summary: "Queue a prompt for the next turn instead of steering the running turn.", Usage: "/queue <prompt>"}
}

func (h handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	def := h.Definition()
	prompt := strings.TrimSpace(req.ArgsText)
	if prompt == "" {
		return commands.Result{Text: commands.UsageText(def), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	if !req.Principal.Authenticated() {
		return commands.Result{Text: "Queueing requires an authenticated identity.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "authentication_required"}}, nil
	}
	if h.broker == nil {
		return commands.Result{}, errUnavailable()
	}
	breq := &broker.Request{
		RequestID:    req.RequestID,
		Principal:    req.Principal,
		ChatID:       req.ChatID,
		SessionKey:   req.SessionKey,
		DisplayName:  req.DisplayName,
		Prompt:       prompt,
		ForceQueue:   true,
		Ctx:          ctx,
		ResponseChan: make(chan broker.Result, 1),
	}
	if strings.TrimSpace(breq.RequestID) == "" {
		breq.RequestID = config.NewRequestID()
	}
	if err := h.broker.Submit(breq); err != nil {
		return commands.Result{Text: config.SafeErrorText(err), Outcome: commands.Outcome{Status: "rejected", ReasonCode: queueReason(err)}}, nil
	}
	var result broker.Result
	select {
	case result = <-breq.ResponseChan:
	case <-ctx.Done():
		return commands.Result{Text: "Queued prompt canceled before it ran.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "request_canceled"}}, nil
	}
	if result.Err != nil {
		return commands.Result{Text: config.SafeErrorText(result.Err), Outcome: commands.Outcome{Status: "error", ReasonCode: queueReason(result.Err)}}, nil
	}
	text := ""
	if result.Response != nil {
		if result.Response.Error != "" {
			return commands.Result{Text: result.Response.Response, Outcome: commands.Outcome{Status: "error", ReasonCode: "model_failure", Operation: "queue.prompt", AffectedCount: 1}}, nil
		}
		text = result.Response.Response
	}
	if strings.TrimSpace(text) == "" {
		text = "Queued prompt completed."
	}
	return commands.Result{Text: text, Outcome: commands.Outcome{Status: "ok", Operation: "queue.prompt", IsChanged: true, AffectedCount: 1}}, nil
}
