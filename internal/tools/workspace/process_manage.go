package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/process"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	outputpkg "github.com/jonahgcarpenter/oswald-ai/internal/tools/output"
)

// ProcessManageDefinition returns the background-process companion contract.
func ProcessManageDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        ProcessManageName,
		Description: "Manage tracked background terminal processes: list sessions, poll status, read logs, wait for completion, kill, write/submit stdin, close stdin, or hand off ownership to the parent scope. Session IDs accept unique prefixes.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"action":     {Type: "string", Description: "list|poll|log|wait|kill|write|submit|close|handoff"},
			"session_id": {Type: "string", Description: "Process session ID (unique prefix accepted)"},
			"data":       {Type: "string", Description: "Stdin bytes for write/submit, purpose sentence for handoff"},
			"timeout":    {Type: "integer", Description: "Wait timeout in seconds", Minimum: float64Ptr(1)},
			"offset":     {Type: "integer", Description: "Log start line (default 0)", Minimum: float64Ptr(0)},
			"limit":      {Type: "integer", Description: "Log max lines (default 200)", Minimum: float64Ptr(1)},
		}, Required: []string{"action"}, AdditionalProperties: boolPtr(false)},
	}
}

// NewProcessManageHandler returns the process companion handler.
func NewProcessManageHandler(ws *Workspace, pm *process.Manager) func(context.Context, map[string]interface{}) (governance.Result, error) {
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
			if ScanProfileEscapeCode(data, ws.Root()) {
				return governance.Result{}, errors.New("cross-profile access is not allowed")
			}
			if err := pm.Write(sessionID, data); err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: "stdin written", Outcome: governance.OutcomeProductive}, nil
		case "submit":
			if ScanProfileEscapeCode(data, ws.Root()) {
				return governance.Result{}, errors.New("cross-profile access is not allowed")
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
