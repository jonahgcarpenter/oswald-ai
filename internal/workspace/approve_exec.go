package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/approvals"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
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
	if err := policy.ScanLifecycle(pending.Command, pending.Workdir); err != nil {
		return governance.Result{}, err
	}
	if err := policy.BlockSelfRepo(pending.Command, pending.Workdir); err != nil {
		return governance.Result{}, err
	}
	// Replay re-checks every deny-class rule: approval overrides the
	// recoverable reviewer path only, never unrecoverable, fetch,
	// confined-write, or protected-read denials.
	live := policy.StripInert(pending.Command)
	if reason, blocked := policy.ScanUnrecoverable(live); blocked {
		return governance.Result{}, errors.New("approved command refused on re-scan: " + reason)
	}
	if verdict, reason := policy.ClassifyFetch(pending.Command, live, pending.Workdir); verdict == policy.FetchDeny {
		return governance.Result{}, errors.New("approved command refused on re-scan: " + reason)
	}
	if reason, blocked := policy.ScanConfinedWrite(pending.Command, pending.Workdir, ws.Root(), true); blocked {
		return governance.Result{}, errors.New("approved command refused on re-scan: " + reason)
	}
	if reason, blocked := policy.ScanProtectedRead(pending.Command, pending.Workdir, ws.Root(), true); blocked {
		return governance.Result{}, errors.New("approved command refused on re-scan: " + reason)
	}
	if reason, blocked := policy.ScanEnvRetarget(pending.Command, true); blocked {
		return governance.Result{}, errors.New("approved command refused on re-scan: " + reason)
	}
	return executeTerminalCommand(ctx, pm, pending.RequestID, pending.Command, pending.Workdir, pending.Scratch, ws.Root(), pending.Timeout, pending.Background, pending.Persist, 0, "")
}
