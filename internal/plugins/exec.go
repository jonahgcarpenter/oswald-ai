package plugins

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

// Exec bounds. Plugin binaries run in the foreground with captured output;
// long jobs belong in tracked background processes instead.
const (
	execDefaultTimeout = 180
	execMaxTimeout     = 600
	maxExecOutput      = 100 << 10
)

// ExecPluginBinary runs one profile-installed plugin binary. The child
// gets a fixed base (PATH with the server-resolved node dir, locale, scratch
// TMPDIR), the profile's passthrough allowlist resolved against the profile
// dotenv scope, and HOME pointed at the per-profile plugin data home so CLIs
// that persist config files land on the persisted volume.
// Stdin is closed: interactive prompts fail closed, so callers pass flags.
// Presence means enabled: callers verify the plugin directory exists.
func ExecPluginBinary(ctx context.Context, profileRoot, key, bin string, args []string, timeoutSecs int, log *config.Logger) (string, error) {
	if !ValidPluginKey(key) {
		return "", fmt.Errorf("invalid plugin key")
	}
	if strings.TrimSpace(profileRoot) == "" {
		return "", fmt.Errorf("plugin home is unavailable")
	}
	binary, err := NodeBinPath(profileRoot, key, bin)
	if err != nil {
		return "", err
	}
	if timeoutSecs <= 0 {
		timeoutSecs = execDefaultTimeout
	}
	if timeoutSecs > execMaxTimeout {
		timeoutSecs = execMaxTimeout
	}
	home, err := StorageRootForProfile(profileRoot, key)
	if err != nil {
		return "", err
	}
	pluginHome := filepath.Join(home, "home")
	if err := os.MkdirAll(pluginHome, 0700); err != nil {
		return "", fmt.Errorf("prepare plugin home: %w", err)
	}
	_ = os.Chmod(pluginHome, 0700)
	scratch := filepath.Join(home, "cache", "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		return "", fmt.Errorf("prepare plugin scratch: %w", err)
	}
	base := []string{
		"PATH=/opt/oswald:/usr/bin:/bin:/usr/local/bin",
		"HOME=" + pluginHome,
		"LANG=C.UTF-8",
		"TMPDIR=" + scratch,
		"OSWALD_WORKSPACE=" + profileRoot,
		"OSWALD_SCRATCH=" + scratch,
		"OSWALD_HOME=" + profileRoot,
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, binary, args...)
	cmd.Dir = home
	cmd.Env = append(process.SandboxEnvForProfile(process.WithNodeOnPath(base), profileRoot), process.GitHubTokenEnv()...)
	cmd.Stdin = nil
	var buf bytes.Buffer
	cmd.Stdout = &limitedExecWriter{buf: &buf, limit: maxExecOutput}
	cmd.Stderr = &limitedExecWriter{buf: &buf, limit: maxExecOutput}
	runErr := cmd.Run()
	output := strings.TrimRight(buf.String(), "\n")
	if len(output) > maxExecOutput {
		output = output[:maxExecOutput]
	}
	if output == "" {
		output = "(no output)"
	}
	if log != nil {
		log.Server("plugin.exec").Debug("plugin.exec.completed", "executed plugin binary", config.F("plugin", key), config.F("tool_name", bin))
	}
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			return "", fmt.Errorf("plugin exited with code %d: %s", exitErr.ExitCode(), output)
		}
		if runCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("plugin execution timed out")
		}
		return "", fmt.Errorf("plugin failed to start")
	}
	return output, nil
}

// StorageRootForProfile resolves the per-profile plugin data dir without a
// Manager, creating it owner-only on first use. Code lives in the sibling
// code/ tree and is never touched here.
func StorageRootForProfile(profileRoot, plugin string) (string, error) {
	if !ValidPluginKey(plugin) {
		return "", fmt.Errorf("invalid plugin key")
	}
	if strings.TrimSpace(profileRoot) == "" {
		return "", fmt.Errorf("plugin home is unavailable")
	}
	root := PluginDataDir(profileRoot, plugin)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("create plugin storage: %w", err)
	}
	_ = os.Chmod(root, 0700)
	return root, nil
}

type limitedExecWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedExecWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return w.buf.Write(p)
}
