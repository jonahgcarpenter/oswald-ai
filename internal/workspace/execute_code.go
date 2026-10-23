package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	outputpkg "github.com/jonahgcarpenter/oswald-ai/internal/tools/output"
)

const (
	executeCodeTimeoutSecs = 300
	maxExecuteCodeOutput   = 50 << 10
)

// NewExecuteCodeHandler returns the stateless Python runner. Each call runs
// fresh with python3 in the profile working directory; state does not
// persist across calls unless written to workspace files.
func NewExecuteCodeHandler(ws *Workspace) func(context.Context, map[string]interface{}) (governance.Result, error) {
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
		if err := rejectUnknown(args, "code", "reset"); err != nil {
			return governance.Result{}, err
		}
		code, ok := stringArg(args, "code")
		if !ok || strings.TrimSpace(code) == "" {
			return governance.Result{}, errors.New("code is required")
		}
		if len(code) > 200<<10 {
			return governance.Result{}, errors.New("code exceeds size limit")
		}
		if reason, blocked := ScanConfinedWrite(code, ws.Root(), ws.Root(), false); blocked {
			return governance.Result{}, errors.New(reason)
		}
		reset, err := boolArg(args, "reset", false)
		if err != nil {
			return governance.Result{}, err
		}
		scratch, serr := ws.EnsureScratch()
		if serr != nil {
			return governance.Result{}, errors.New("workspace is unavailable")
		}
		if reset {
			_ = os.RemoveAll(filepath.Join(scratch, "__pycache__"))
		}
		python, err := exec.LookPath("python3")
		if err != nil {
			return governance.Result{}, errors.New("python3 is unavailable")
		}
		tmp, err := os.CreateTemp(scratch, "execute-*.py")
		if err != nil {
			return governance.Result{}, errors.New("workspace is unavailable")
		}
		name := tmp.Name()
		defer os.Remove(name)
		if _, err := tmp.WriteString(code); err != nil {
			tmp.Close()
			return governance.Result{}, errors.New("workspace is unavailable")
		}
		tmp.Close()
		if err := os.Chmod(name, 0600); err != nil {
			return governance.Result{}, errors.New("workspace is unavailable")
		}
		runCtx, cancel := context.WithTimeout(ctx, secondsToDuration(executeCodeTimeoutSecs))
		defer cancel()
		cmd := exec.CommandContext(runCtx, python, name)
		cmd.Dir = ws.Root()
		cmd.Env = []string{
			"PATH=/usr/bin:/bin:/usr/local/bin",
			"HOME=" + userHome(),
			"LANG=C.UTF-8",
			"TMPDIR=" + scratch,
			"OSWALD_WORKSPACE=" + ws.Root(),
			"OSWALD_SCRATCH=" + scratch,
			"PYTHONDONTWRITEBYTECODE=1",
		}
		var buf bytes.Buffer
		cmd.Stdout = &limitedWriter{buf: &buf, limit: maxExecuteCodeOutput}
		cmd.Stderr = &limitedWriter{buf: &buf, limit: maxExecuteCodeOutput}
		runErr := cmd.Run()
		output := outputpkg.Clean(buf.String())
		output = strings.TrimRight(output, "\n")
		if output == "" {
			output = "(no output)"
		}
		if len(output) >= maxExecuteCodeOutput {
			output += "\n[stdout truncated at 50KB]"
		}
		if runErr != nil {
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				return governance.Result{
					Content: fmt.Sprintf("exit_code: %d\n%s", exitErr.ExitCode(), output),
					Outcome: governance.OutcomeProductive,
				}, nil
			}
			if runCtx.Err() == context.DeadlineExceeded {
				return governance.Result{
					Content: fmt.Sprintf("exit_code: 124\n%s\n[timed out after 300s]", output),
					Outcome: governance.OutcomeProductive,
				}, nil
			}
			return governance.Result{}, errors.New("python execution failed to start")
		}
		return governance.Result{
			Content: fmt.Sprintf("exit_code: 0\n%s", output),
			Outcome: governance.OutcomeProductive,
		}, nil
	}
}
