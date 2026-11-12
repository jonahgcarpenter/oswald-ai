// Package restart implements `oswald restart`: signal the running
// oswald-server to exit gracefully so the container runtime restarts it in
// place (same pod, same volume). Load-bearing config and plugin changes print
// "run `oswald restart` to apply" — the model batches edits, tells the user,
// then runs this as the final step so its own turn is never killed by an
// automatic rebuild.
package restart

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PIDFileName is the server PID marker inside the configuration root.
const PIDFileName = "server.pid"

// killFunc sends a signal to pid. It is a variable for tests so no real
// process is signaled.
var killFunc = func(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// Run signals the running server. Exit codes mirror the parent cli convention.
func Run(_ context.Context, root string, rest []string, stdout, stderr io.Writer) int {
	const usage = "usage: oswald restart [--delay SECS|--now] [--force]"
	delay := time.Duration(0)
	force := false
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		switch {
		case token == "--now":
			delay = 0
		case token == "--force":
			force = true
		case token == "--delay" && i+1 < len(rest):
			i++
			n, err := strconv.Atoi(strings.TrimSpace(rest[i]))
			if err != nil || n < 0 || n > 300 {
				fmt.Fprintln(stderr, usage)
				return 2
			}
			delay = time.Duration(n) * time.Second
		case strings.HasPrefix(token, "--delay="):
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(token, "--delay=")))
			if err != nil || n < 0 || n > 300 {
				fmt.Fprintln(stderr, usage)
				return 2
			}
			delay = time.Duration(n) * time.Second
		case strings.HasPrefix(token, "-"):
			fmt.Fprintln(stderr, usage)
			return 2
		default:
			fmt.Fprintln(stderr, usage)
			return 2
		}
	}
	pidFile := filepath.Join(root, PIDFileName)
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		fmt.Fprintln(stderr, "oswald: server is not running (no pid file)")
		return 1
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		fmt.Fprintln(stderr, "oswald: server pid file is invalid")
		return 1
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if err := killFunc(pid, sig); err != nil {
		// Stale pid file: process is gone. Clean up so the next boot
		// writes fresh, and report not-running instead of an error string
		// that could confuse the model.
		_ = os.Remove(pidFile)
		fmt.Fprintln(stderr, "oswald: server is not running")
		return 1
	}
	fmt.Fprintf(stdout, "restarting server (pid %d)\n", pid)
	return 0
}
