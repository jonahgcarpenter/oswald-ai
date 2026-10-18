// Package approve implements the /approve and /deny chat commands for
// terminal-command security review. Both consume the single pending ask for
// the current profile conversation; outside its five-minute window they
// report expiry instead of acting.
package approve

import (
	"context"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/approvals"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

// Deps carries the profile root used to rebuild the sandboxed executor.
type Deps struct {
	Root string
}

type approveHandler struct{ root string }

// NewApprove constructs the approve command.
func NewApprove(root string) commands.Handler { return approveHandler{root: root} }

func (h approveHandler) Definition() commands.Definition {
	return commands.Definition{Name: "approve", Summary: "Approve the pending command in this conversation.", Usage: "/approve", OutOfBand: true}
}

func (h approveHandler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if len(req.Args) != 0 {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	if !req.Principal.Authenticated() {
		return commands.Result{Text: "Approving a command requires an authenticated identity.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "authentication_required"}}, nil
	}
	pending, ok, expired := approvals.Take(req.Principal.CanonicalUserID, req.SessionKey)
	if !ok {
		if expired {
			return commands.Result{Text: "The approval request expired; it was denied. Ask again to retry the command.", Outcome: commands.Outcome{Status: "ok", ReasonCode: "approval_expired", Operation: "approval.decide"}}, nil
		}
		return commands.Result{Text: "No command is waiting for approval in this conversation.", Outcome: commands.Outcome{Status: "ok", ReasonCode: "no_op", Operation: "approval.decide"}}, nil
	}
	ws := workspace.NewWorkspace(h.root, req.Principal.CanonicalUserID)
	result, err := workspace.ExecuteApproved(ctx, ws, process.Shared(h.root), pending)
	if err != nil {
		return commands.Result{Text: "Approved, but execution was refused.", Outcome: commands.Outcome{Status: "ok", ReasonCode: "approval_guard_refused", Operation: "approval.execute", IsChanged: true, AffectedCount: 1}}, nil
	}
	return commands.Result{Text: "Approved and executed:\n" + truncateRunes(result.Content, 4000), Outcome: commands.Outcome{Status: "ok", Operation: "approval.execute", IsChanged: true, AffectedCount: 1}}, nil
}

type denyHandler struct{}

// NewDeny constructs the deny command.
func NewDeny() commands.Handler { return denyHandler{} }

func (denyHandler) Definition() commands.Definition {
	return commands.Definition{Name: "deny", Summary: "Deny the pending command in this conversation.", Usage: "/deny", OutOfBand: true}
}

func (denyHandler) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	if len(req.Args) != 0 {
		return commands.Result{Text: commands.UsageText(denyHandler{}.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	if !req.Principal.Authenticated() {
		return commands.Result{Text: "Denying a command requires an authenticated identity.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "authentication_required"}}, nil
	}
	_, ok, expired := approvals.Take(req.Principal.CanonicalUserID, req.SessionKey)
	if !ok {
		if expired {
			return commands.Result{Text: "The approval request already expired; it was denied.", Outcome: commands.Outcome{Status: "ok", ReasonCode: "approval_expired", Operation: "approval.decide"}}, nil
		}
		return commands.Result{Text: "No command is waiting for approval in this conversation.", Outcome: commands.Outcome{Status: "ok", ReasonCode: "no_op", Operation: "approval.decide"}}, nil
	}
	return commands.Result{Text: "Denied. The command will not run.", Outcome: commands.Outcome{Status: "ok", Operation: "approval.decide", IsChanged: true, AffectedCount: 1}}, nil
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "\n[truncated]"
}
