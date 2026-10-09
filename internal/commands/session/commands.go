package session

import (
	"context"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
)

// Starter closes one canonical user's current session so the next turn opens a
// fresh one. It never deletes prior transcripts.
type Starter interface {
	NewSessionContext(context.Context, string, string) error
}

type handler struct{ sessions Starter }

// New creates the new-session command.
func New(sessions Starter) commands.Handler { return &handler{sessions: sessions} }

func (h *handler) Definition() commands.Definition {
	return commands.Definition{Name: "new", Summary: "Start a new session; past sessions stay searchable.", Usage: "/new"}
}

func (h *handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if len(req.Args) != 0 {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	if !req.Principal.Authenticated() {
		return commands.Result{Text: "Starting a new session requires an authenticated identity.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "authentication_required"}}, nil
	}
	if h.sessions == nil {
		return commands.Result{}, fmt.Errorf("new session is not configured")
	}
	if err := h.sessions.NewSessionContext(ctx, req.Principal.CanonicalUserID, req.SessionKey); err != nil {
		return commands.Result{}, err
	}
	return commands.Result{Text: "Started a new session. Past conversations remain searchable.", Outcome: commands.Outcome{Status: "ok", Operation: "session.new", IsChanged: true, AffectedCount: 1}}, nil
}
