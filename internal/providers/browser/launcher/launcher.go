package launcher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// LaunchedBrowser owns a Chromium child process and its DevTools endpoint.
type LaunchedBrowser struct {
	Cmd *exec.Cmd
	Dir string
	URL string
}

// sandboxBypassArgs are required when Chromium's own sandbox cannot work:
// root, containers, or hosts denying unprivileged user namespaces.
var sandboxBypassArgs = []string{"--no-sandbox", "--disable-dev-shm-usage"}

// NeedsSandboxBypass reports whether Chromium needs its sandbox disabled to
// start reliably. It performs no network I/O.
func NeedsSandboxBypass() bool {
	if os.Geteuid() == 0 {
		return true
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	if data, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil {
		return strings.TrimSpace(string(data)) == "1"
	}
	return false
}

// withSandboxBypass appends the bypass flags unless the operator already set
// --no-sandbox explicitly. Operator flags are never overridden.
func withSandboxBypass(extra []string) []string {
	for _, arg := range extra {
		if arg == "--no-sandbox" {
			return extra
		}
	}
	if !NeedsSandboxBypass() {
		return extra
	}
	return append(append([]string{}, extra...), sandboxBypassArgs...)
}

// findChromium resolves the browser binary: explicit launch command first,
// then PATH lookup. Missing installs surface install guidance, never a
// retry-download loop.
func FindChromium(launchCommand string) (string, []string, error) {
	if strings.TrimSpace(launchCommand) != "" {
		fields := strings.Fields(launchCommand)
		if len(fields) == 0 {
			return "", nil, fmt.Errorf("invalid browser launch command")
		}
		if _, err := exec.LookPath(fields[0]); err != nil {
			if _, statErr := os.Stat(fields[0]); statErr != nil {
				return "", nil, fmt.Errorf("browser unavailable: launch command %q not found", fields[0])
			}
		}
		return fields[0], fields[1:], nil
	}
	for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil, nil
		}
	}
	return "", nil, fmt.Errorf("browser unavailable: no Chromium found (tried chromium, chromium-browser, google-chrome); install Chromium or set tools.browser.launch_command")
}

// LaunchChromium starts headless Chromium with a private user-data dir and a
// scrubbed environment, then waits for the DevTools endpoint.
func LaunchChromium(ctx context.Context, binary string, extraArgs []string, profileRoot string) (*LaunchedBrowser, error) {
	dir, err := sessionDir(profileRoot)
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-dev-shm-usage",
		// Bound on-machine disk use: this browser is a stateless page
		// renderer, not a daily driver; the profile dir is recycled.
		"--disk-cache-size=67108864",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		fmt.Sprintf("--user-data-dir=%s", dir),
		"about:blank",
	}
	args = append(args, withSandboxBypass(extraArgs)...)
	cmd := exec.Command(binary, args...)
	cmd.Env = ScrubBrowserEnv()
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("start browser: %w", err)
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitForEndpoint(ctx, endpoint, cmd); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		os.RemoveAll(dir)
		return nil, err
	}
	return &LaunchedBrowser{Cmd: cmd, Dir: dir, URL: endpoint}, nil
}

func waitForEndpoint(ctx context.Context, endpoint string, cmd *exec.Cmd) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return fmt.Errorf("browser exited during startup (missing system libraries? install the Chromium headless-shell dependencies)")
		}
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		targets, err := ListTargets(reqCtx, endpoint)
		cancel()
		if err == nil && len(targets) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("browser debugger did not respond at %s", endpoint)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate browser port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// SessionBaseDir owns per-session Chromium user-data dirs and snapshot
// spills below the profile cache, so the prune worker (not the profile
// root) owns their lifecycle.
func SessionBaseDir(profileRoot string) string {
	return filepath.Join(profileRoot, "cache", "browser")
}

// sessionDir provisions one owner-only session directory as
// cache/browser/profile-<id>.
func sessionDir(profileRoot string) (string, error) {
	base := SessionBaseDir(profileRoot)
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", fmt.Errorf("create browser profile base: %w", err)
	}
	for range 5 {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "", fmt.Errorf("create browser profile id: %w", err)
		}
		dir := filepath.Join(base, "profile-"+hex.EncodeToString(id[:]))
		if err := os.Mkdir(dir, 0700); err == nil {
			return dir, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create browser profile dir: %w", err)
		}
	}
	return "", fmt.Errorf("create browser profile dir: too many collisions")
}

// SweepStaleProfiles removes orphaned Chromium user-data dirs under
// cache/browser. Call at process start, when no live session can
// exist in this process. Best-effort; missing roots are a no-op. It returns
// the number of removed directories.
func SweepStaleProfiles(profileRoot string) int {
	reaped := 0
	if strings.TrimSpace(profileRoot) != "" {
		base := SessionBaseDir(profileRoot)
		entries, err := os.ReadDir(base)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() || !strings.HasPrefix(e.Name(), "profile-") {
					continue
				}
				if err := os.RemoveAll(filepath.Join(base, e.Name())); err == nil {
					reaped++
				}
			}
		}
	}
	return reaped
}

// ScrubBrowserEnv keeps a minimal environment: sane PATH/HOME/TMPDIR plus
// loopback proxy bypass. Only browser backend configuration flows through
// options, never ambient secrets.
func ScrubBrowserEnv() []string {
	keep := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true}
	out := make([]string, 0, len(keep)+2)
	for _, kv := range os.Environ() {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			if keep[kv[:idx]] {
				out = append(out, kv)
			}
		}
	}
	out = append(out, "NO_PROXY=localhost,127.0.0.1,::1")
	out = append(out, "no_proxy=localhost,127.0.0.1,::1")
	if !hasPath(out) {
		out = append(out, "PATH=/usr/bin:/bin:/usr/local/bin")
	}
	return out
}

func hasPath(env []string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			return true
		}
	}
	return false
}

// ListTargets returns page WebSocket debugger URLs for a DevTools endpoint.
func ListTargets(ctx context.Context, endpointURL string) ([]string, error) {
	base := endpointURL
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list browser targets: %w", err)
	}
	defer resp.Body.Close()
	var targets []struct {
		Type                 string `json:"type"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, fmt.Errorf("decode browser targets: %w", err)
	}
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			out = append(out, t.WebSocketDebuggerURL)
		}
	}
	return out, nil
}
