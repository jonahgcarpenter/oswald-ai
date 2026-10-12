package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/process"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	outputpkg "github.com/jonahgcarpenter/oswald-ai/internal/tools/output"
)

const (
	defaultTerminalTimeout = 180
	maxTerminalTimeout     = 600
	maxTerminalOutput      = 100 << 10
)

// NewTerminalHandler returns the sandboxed shell runner with tracked
// background support. Background spawns via the profile process manager and
// returns a session_id for process_manage polling.
func NewTerminalHandler(ws *Workspace, pm *process.Manager) func(context.Context, map[string]interface{}) (governance.Result, error) {
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
		if background {
			if pm == nil {
				return governance.Result{}, errors.New("process manager is unavailable")
			}
			if timeout > maxTerminalTimeout {
				// Over-cap background keeps the requested bound; note it.
				notifyNote = appendNote(notifyNote, fmt.Sprintf("note: timeout %ds exceeds foreground max; running tracked", timeout))
			}
			info, serr := pm.Spawn(requestID(ctx), command, workdir, scratch, timeout)
			if serr != nil {
				return governance.Result{}, serr
			}
			if persist {
				_ = pm.SetPersist(info.ID, true)
			}
			content := fmt.Sprintf("session_id: %s\npid: %d\nexit_code: 0\nstatus: running\nPoll with process_manage (poll/log/wait).", info.ID, info.PID)
			if heartbeat > 0 {
				content += "\nnote: heartbeat recorded; poll with process_manage (no live push in this deployment)"
			}
			if notifyNote != "" {
				content += "\n" + notifyNote
			}
			return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
		}
		if timeout > maxTerminalTimeout {
			// Over-cap foreground promotes to tracked background.
			if pm == nil {
				return governance.Result{}, errors.New("process manager is unavailable")
			}
			info, serr := pm.Spawn(requestID(ctx), command, workdir, scratch, timeout)
			if serr != nil {
				return governance.Result{}, serr
			}
			return governance.Result{Content: fmt.Sprintf("session_id: %s\npid: %d\nexit_code: 0\nstatus: running\nnote: foreground timeout over cap; promoted to tracked background. Poll with process_manage.", info.ID, info.PID), Outcome: governance.OutcomeProductive}, nil
		}
		output, code, runErr := runShell(ctx, command, workdir, scratch, timeout)
		content := fmt.Sprintf("exit_code: %d\n%s", code, output)
		if runErr != nil && code < 0 {
			return governance.Result{}, runErr
		}
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func runShell(ctx context.Context, command, dir, scratch string, timeoutSecs int) (string, int, error) {
	shell, err := shellPath()
	if err != nil {
		return "", -1, errors.New("shell is unavailable")
	}
	// Timeout applies to the command only; the caller context still cancels.
	runCtx := ctx
	cancel := func() {}
	if timeoutSecs > 0 {
		runCtx, cancel = context.WithTimeout(ctx, secondsToDuration(timeoutSecs))
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, shell, "-c", command)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"HOME=" + userHome(),
		"LANG=C.UTF-8",
		"TMPDIR=" + scratch,
		"OSWALD_WORKSPACE=" + dir,
		"OSWALD_SCRATCH=" + scratch,
	}
	var buf bytes.Buffer
	cmd.Stdout = &limitedWriter{buf: &buf, limit: maxTerminalOutput}
	cmd.Stderr = &limitedWriter{buf: &buf, limit: maxTerminalOutput}
	err = cmd.Run()
	_ = cancel
	output := outputpkg.Clean(buf.String())
	truncated := false
	if len(output) > maxTerminalOutput {
		output = output[:maxTerminalOutput]
		truncated = true
	}
	output = strings.TrimRight(output, "\n")
	if truncated {
		output += "\n[output truncated at 100KB]"
	}
	if output == "" {
		output = "(no output)"
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return output, exitErr.ExitCode(), nil
		}
		if runCtx.Err() == context.DeadlineExceeded {
			return output, 124, nil
		}
		return output, -1, errors.New("command failed to start")
	}
	return output, 0, nil
}

func shellPath() (string, error) {
	for _, candidate := range []string{"/bin/sh", "/usr/bin/sh"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("no shell")
}

type limitedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return w.buf.Write(p)
}
