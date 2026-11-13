package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"

	outputpkg "github.com/jonahgcarpenter/oswald-ai/internal/tools/output"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

// runShell executes one foreground shell command with the sandboxed child
// environment and a bounded output capture. Callers own timeout selection
// and retry policy; a negative code with an error means the command never
// started.
func runShell(ctx context.Context, command, dir, scratch, home string, timeoutSecs int) (string, int, error) {
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
	base := []string{
		"PATH=/opt/oswald:/usr/bin:/bin:/usr/local/bin",
		"HOME=" + UserHome(),
		"LANG=C.UTF-8",
		"TMPDIR=" + scratch,
		"OSWALD_WORKSPACE=" + dir,
		"OSWALD_SCRATCH=" + scratch,
		"OSWALD_HOME=" + home,
	}
	cmd.Env = append(process.SandboxEnvForProfile(process.WithPluginShimsOnPath(process.WithNodeOnPath(base), home), home), process.GitHubTokenEnv()...)
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
