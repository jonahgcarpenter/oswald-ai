package toolterminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

// defaultTerminalTimeout is the default foreground wait, matching the
// workspace execution engine's bound.
const defaultTerminalTimeout = 180

// NewTerminalHandler returns the sandboxed shell runner with tracked
// background support. Background spawns via the profile process manager and
// returns a session_id for process_manage polling.
func NewTerminalHandler(ws *workspace.Workspace, pm *process.Manager) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		if ws == nil || ws.Root() == "" {
			return governance.Result{}, errors.New("workspace is unavailable")
		}
		if _, ok := requestctx.PrincipalFromContext(ctx); !ok {
			return governance.Result{}, errors.New("workspace: authenticated principal required")
		}
		principal, _ := requestctx.PrincipalFromContext(ctx)
		if !principal.Authenticated() {
			return governance.Result{}, errors.New("workspace: authenticated principal required")
		}
		if err := rejectUnknown(args, "command", "background", "timeout", "workdir", "pty", "notify", "heartbeat", "persist_on_release"); err != nil {
			return governance.Result{}, err
		}
		command, ok := stringArg(args, "command")
		if !ok || strings.TrimSpace(command) == "" {
			return governance.Result{}, errors.New("command is required")
		}
		if len(command) > 32<<10 {
			return governance.Result{}, errors.New("command exceeds size limit")
		}
		background, err := boolArg(args, "background", false)
		if err != nil {
			return governance.Result{}, err
		}
		if pty, err := boolArg(args, "pty", false); err != nil {
			return governance.Result{}, err
		} else if pty && !background {
			return governance.Result{}, errors.New("pty requires background=true")
		}
		persist, err := boolArg(args, "persist_on_release", false)
		if err != nil {
			return governance.Result{}, err
		}
		notifyNote := ""
		if raw, ok := args["notify"]; ok {
			switch v := raw.(type) {
			case bool:
				if v && !background {
					notifyNote = "note: notify applies to background processes only"
				}
			case []interface{}:
				if !background {
					notifyNote = "note: notify applies to background processes only"
				} else {
					notifyNote = "note: watch patterns recorded; poll with process_manage (no live push in this deployment)"
				}
			default:
				return governance.Result{}, errors.New("notify must be a boolean or string array")
			}
		}
		heartbeat, err := intArg(args, "heartbeat", 0)
		if err != nil || heartbeat < 0 {
			return governance.Result{}, errors.New("heartbeat must be an integer >= 0")
		}
		timeout, err := intArg(args, "timeout", defaultTerminalTimeout)
		if err != nil || timeout < 1 {
			return governance.Result{}, errors.New("timeout must be an integer >= 1")
		}
		workdir := ws.Root()
		if raw, ok := stringArg(args, "workdir"); ok && strings.TrimSpace(raw) != "" {
			if err := workspace.ValidateWorkdirString(raw); err != nil {
				return governance.Result{}, err
			}
			resolved, rerr := ws.ResolveDir(raw)
			if rerr != nil {
				return governance.Result{}, rerr
			}
			workdir = resolved
		}
		scratch, serr := ws.EnsureScratch()
		if serr != nil {
			return governance.Result{}, errors.New("workspace is unavailable")
		}
		return ws.ExecuteCommand(ctx, pm, workspace.CommandRequest{
			Principal:  principal,
			Command:    command,
			Workdir:    workdir,
			Scratch:    scratch,
			Timeout:    timeout,
			Background: background,
			Persist:    persist,
			Heartbeat:  heartbeat,
			NotifyNote: notifyNote,
		})
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

func stringArg(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key].(string)
	return v, ok
}

func boolArg(args map[string]interface{}, key string, def bool) (bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return def, nil
	}
	v, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return v, nil
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
