package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/approvals"
	"github.com/jonahgcarpenter/oswald-ai/internal/process"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// ExecuteApproved replays a user-approved pending command. The destructive
// detection that triggered review is skipped (the user overrode it), but
// the lifecycle and self-repo checks still fail closed, and the sandbox
// containment is re-validated before anything runs.
func ExecuteApproved(ctx context.Context, ws *Workspace, pm *process.Manager, pending approvals.Pending) (governance.Result, error) {
	if err := ctx.Err(); err != nil {
		return governance.Result{}, err
	}
	if ws == nil || ws.Root() == "" {
		return governance.Result{}, errors.New("workspace is unavailable")
	}
	if strings.TrimSpace(pending.Command) == "" {
		return governance.Result{}, errors.New("approved command is empty")
	}
	rel, err := filepath.Rel(ws.Root(), pending.Workdir)
	if err != nil || pending.Workdir == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return governance.Result{}, errors.New("approved working directory escapes the workspace")
	}
	if err := ScanLifecycle(pending.Command, pending.Workdir); err != nil {
		return governance.Result{}, err
	}
	if err := BlockSelfRepo(pending.Command, pending.Workdir); err != nil {
		return governance.Result{}, err
	}
	return executeTerminalCommand(ctx, pm, pending.RequestID, pending.Command, pending.Workdir, pending.Scratch, pending.Timeout, pending.Background, pending.Persist, 0, "")
}
