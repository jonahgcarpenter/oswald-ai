// Package restart implements `oswald restart`: signal the local
// oswald-server process to exit gracefully so the supervisor (container
// runtime or service manager) restarts it in place. Load-bearing config and
// plugin changes print "run `oswald restart` to apply" — the model batches
// edits, tells the user, then runs this as the final step so its own turn is
// never killed by an automatic rebuild.
package restart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/help"
)

// serverProcessName is the exe basename matched when discovering the running
// server. Matching uses /proc/<pid>/exe (falling back to /proc/<pid>/comm),
// never a pid file on disk.
const serverProcessName = "oswald-server"

// killFunc sends a signal to pid. It is a variable for tests so no real
// process is signaled.
var killFunc = func(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// resolveServerPID locates the server to signal. It is a variable for tests
// so process discovery can be stubbed without touching /proc.
var resolveServerPID = func() (int, error) {
	if pid, ok := ancestorServerPID(os.Getpid()); ok {
		return pid, nil
	}
	if isServerPID(1) {
		return 1, nil
	}
	return 0, errors.New("server not found in local PID namespace")
}

// Run signals the local server. Exit codes mirror the parent cli convention.
func Run(_ context.Context, _ string, rest []string, stdout, stderr io.Writer) int {
	const usage = "usage: oswald restart [--delay SECS|--now] [--force]"
	if help.WantsHelp(rest) {
		fmt.Fprintln(stdout, usage)
		fmt.Fprintln(stdout, "  Signal the local oswald-server to exit gracefully so the")
		fmt.Fprintln(stdout, "  supervisor restarts it in place (batch edits first).")
		return 0
	}
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
	if delay > 0 {
		time.Sleep(delay)
	}
	pid, err := resolveServerPID()
	if err != nil || pid <= 0 {
		fmt.Fprintln(stderr, "oswald: server is not running")
		return 1
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if err := killFunc(pid, sig); err != nil {
		fmt.Fprintln(stderr, "oswald: server is not running")
		return 1
	}
	fmt.Fprintf(stdout, "restarting server (pid %d)\n", pid)
	return 0
}

// ancestorServerPID walks the parent chain from self and returns the nearest
// oswald-server ancestor. Starting at the parent skips the `oswald` CLI
// itself; the loop is bounded and cycle-guarded.
func ancestorServerPID(self int) (int, bool) {
	seen := map[int]bool{self: true}
	for pid := parentPID(self); pid > 1 && !seen[pid]; {
		seen[pid] = true
		if isServerPID(pid) {
			return pid, true
		}
		next := parentPID(pid)
		if next == pid {
			return 0, false
		}
		pid = next
	}
	return 0, false
}

// parentPID reports the parent of pid via /proc/<pid>/stat. It returns 0
// when the process or its stat is unreadable.
func parentPID(pid int) int {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// stat is "pid (comm) state ppid ..."; comm may contain spaces and
	// parens, so split after the last ")".
	rest := string(raw[strings.LastIndexByte(string(raw), ')')+1:])
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return 0
	}
	return ppid
}

// isServerPID reports whether pid's executable basename is the server. The
// /proc/<pid>/exe symlink is authoritative; /proc/<pid>/comm covers kernels
// or permissions where the link is unreadable.
func isServerPID(pid int) bool {
	if pid <= 0 {
		return false
	}
	if target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return filepath.Base(target) == serverProcessName
	}
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		return strings.TrimSpace(string(raw)) == serverProcessName
	}
	return false
}
