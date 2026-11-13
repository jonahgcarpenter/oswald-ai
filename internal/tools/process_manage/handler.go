package processmanage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	outputpkg "github.com/jonahgcarpenter/oswald-ai/internal/tools/output"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

// NewProcessManageHandler returns the process companion handler. The
// workspace provides confinement policy and the process manager owns
// session lifecycle; this package owns only the model-facing surface.
func NewProcessManageHandler(ws *workspace.Workspace, pm *process.Manager) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		if ws == nil || ws.Root() == "" || pm == nil {
			return governance.Result{}, errors.New("workspace is unavailable")
		}
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("workspace: authenticated principal required")
		}
		if err := rejectUnknown(args, "action", "session_id", "data", "timeout", "offset", "limit"); err != nil {
			return governance.Result{}, err
		}
		action, _ := stringArg(args, "action")
		action = strings.TrimSpace(action)
		sessionID, _ := stringArg(args, "session_id")
		data, _ := stringArg(args, "data")
		timeout, err := intArg(args, "timeout", 30)
		if err != nil {
			return governance.Result{}, err
		}
		offset, err := intArg(args, "offset", 0)
		if err != nil || offset < 0 {
			return governance.Result{}, errors.New("offset must be an integer >= 0")
		}
		limit, err := intArg(args, "limit", 200)
		if err != nil || limit < 1 || limit > 2000 {
			return governance.Result{}, errors.New("limit must be an integer between 1 and 2000")
		}
		switch action {
		case "list":
			infos, err := pm.List()
			if err != nil {
				return governance.Result{}, errors.New("process list failed")
			}
			if len(infos) == 0 {
				return governance.Result{Content: "(no processes)", Outcome: governance.OutcomeUnproductive}, nil
			}
			var sb strings.Builder
			for _, info := range infos {
				fmt.Fprintf(&sb, "%s %s exit=%d %s\n", info.ID, info.Status, info.ExitCode, outputpkg.Clean(info.CommandPreview))
			}
			return governance.Result{Content: strings.TrimSuffix(sb.String(), "\n"), Outcome: governance.OutcomeProductive}, nil
		case "poll":
			info, err := pm.Load(sessionID)
			if err != nil {
				return governance.Result{}, err
			}
			tail, _ := pm.Tail(info.ID, 4096)
			return governance.Result{Content: fmt.Sprintf("session_id: %s\nstatus: %s\nexit_code: %d\n%s", info.ID, info.Status, info.ExitCode, outputpkg.Clean(tail)), Outcome: governance.OutcomeProductive}, nil
		case "log":
			lines, total, err := pm.LogLines(sessionID, offset, limit)
			if err != nil {
				return governance.Result{}, err
			}
			if len(lines) == 0 {
				return governance.Result{Content: fmt.Sprintf("(no output, %d total lines)", total), Outcome: governance.OutcomeUnproductive}, nil
			}
			return governance.Result{Content: outputpkg.Clean(strings.Join(lines, "\n")), Outcome: governance.OutcomeProductive}, nil
		case "wait":
			info, err := pm.Wait(sessionID, timeout)
			if err != nil {
				return governance.Result{}, err
			}
			tail, _ := pm.Tail(info.ID, 4096)
			return governance.Result{Content: fmt.Sprintf("session_id: %s\nstatus: %s\nexit_code: %d\n%s", info.ID, info.Status, info.ExitCode, outputpkg.Clean(tail)), Outcome: governance.OutcomeProductive}, nil
		case "kill":
			info, err := pm.Kill(sessionID)
			if err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: fmt.Sprintf("session_id: %s\nstatus: %s\nexit_code: %d", info.ID, info.Status, info.ExitCode), Outcome: governance.OutcomeProductive}, nil
		case "write":
			if reason, blocked := workspace.ScanConfinedWrite(data, ws.Root(), ws.Root(), false); blocked {
				return governance.Result{}, errors.New(reason)
			}
			if reason, blocked := workspace.ScanProtectedRead(data, ws.Root(), ws.Root(), false); blocked {
				return governance.Result{}, errors.New(reason)
			}
			if err := pm.Write(sessionID, data); err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: "stdin written", Outcome: governance.OutcomeProductive}, nil
		case "submit":
			if reason, blocked := workspace.ScanConfinedWrite(data, ws.Root(), ws.Root(), false); blocked {
				return governance.Result{}, errors.New(reason)
			}
			if reason, blocked := workspace.ScanProtectedRead(data, ws.Root(), ws.Root(), false); blocked {
				return governance.Result{}, errors.New(reason)
			}
			if err := pm.Submit(sessionID, data); err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: "stdin submitted", Outcome: governance.OutcomeProductive}, nil
		case "close":
			if err := pm.Close(sessionID); err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: "stdin closed", Outcome: governance.OutcomeProductive}, nil
		case "handoff":
			info, err := pm.Handoff(sessionID, requestID(ctx), data)
			if err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: fmt.Sprintf("session_id: %s handed off", info.ID), Outcome: governance.OutcomeProductive}, nil
		default:
			return governance.Result{}, errors.New("action must be list|poll|log|wait|kill|write|submit|close|handoff")
		}
	}
}

// The arg helpers below are intentionally local: every tools/* package owns
// its own small copies instead of a shared generic helper package.

func rejectUnknown(args map[string]interface{}, allowed ...string) error {
	keep := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		keep[k] = true
	}
	for k := range args {
		if !keep[k] {
			return fmt.Errorf("unknown argument %q", k)
		}
	}
	return nil
}

func requestID(ctx context.Context) string {
	return strings.TrimSpace(requestctx.MetadataFromContext(ctx).RequestID)
}

func stringArg(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key].(string)
	return v, ok
}

func intArg(args map[string]interface{}, key string, def int) (int, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return def, nil
	}
	switch v := raw.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		return int(v), nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("%s must be an integer", key)
	}
}
