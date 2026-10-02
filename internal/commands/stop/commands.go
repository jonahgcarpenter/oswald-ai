package stop

import (
	"context"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
)

// Canceler stops only the current profile conversation's active agent request.
type Canceler interface {
	CancelActiveAgentWork(string, string) broker.CancelReport
}
type handler struct{ canceler Canceler }

// New constructs the out-of-band, conversation-scoped stop command.
func New(canceler Canceler) commands.Handler { return handler{canceler: canceler} }
func (handler) Definition() commands.Definition {
	return commands.Definition{Name: "stop", Summary: "Stop a running response in this conversation.", Usage: "/stop", OutOfBand: true}
}
func (h handler) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	if len(req.Args) != 0 {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	if !req.Principal.Authenticated() {
		return commands.Result{Text: "Stopping a response requires an authenticated identity.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "authentication_required"}}, nil
	}
	if h.canceler == nil {
		return commands.Result{}, fmt.Errorf("stop command is unavailable")
	}
	report := h.canceler.CancelActiveAgentWork(req.Principal.CanonicalUserID, req.SessionKey)
	if report.ActiveSignaled == 0 {
		return commands.Result{Text: "Nothing is currently running in this conversation.", Outcome: commands.Outcome{Status: "ok", ReasonCode: "no_op", Operation: "stop.session"}}, nil
	}
	return commands.Result{Text: "Stopped the current response.", Outcome: commands.Outcome{Status: "ok", Operation: "stop.session", IsChanged: true, AffectedCount: report.ActiveSignaled, ActiveCanceledCount: report.ActiveSignaled}}, nil
}
