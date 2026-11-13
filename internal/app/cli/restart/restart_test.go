package restart

import (
	"bytes"
	"errors"
	"strings"
	"syscall"
	"testing"
)

func stubResolve(pid int, err error) func() (int, error) {
	return func() (int, error) {
		return pid, err
	}
}

func TestRestartServerNotFound(t *testing.T) {
	old := resolveServerPID
	defer func() { resolveServerPID = old }()
	resolveServerPID = stubResolve(0, errors.New("server not found in local PID namespace"))
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), t.TempDir(), nil, &stdout, &stderr); code != 1 {
		t.Fatalf("missing server = %d, want 1", code)
	}
	if out := stderr.String(); !strings.Contains(out, "oswald: server is not running") {
		t.Fatalf("stderr = %q, want not-running", out)
	}
}

func TestRestartSignalsServer(t *testing.T) {
	oldResolve := resolveServerPID
	defer func() { resolveServerPID = oldResolve }()
	resolveServerPID = stubResolve(4321, nil)
	old := killFunc
	defer func() { killFunc = old }()
	var gotPID int
	var gotSig syscall.Signal
	killFunc = func(pid int, sig syscall.Signal) error {
		gotPID, gotSig = pid, sig
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), t.TempDir(), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("restart = %d, want 0: %s", code, stderr.String())
	}
	if gotPID != 4321 || gotSig != syscall.SIGTERM {
		t.Fatalf("signal = pid %d sig %v, want pid 4321 SIGTERM", gotPID, gotSig)
	}
}

func TestRestartForceSendsSIGKILL(t *testing.T) {
	oldResolve := resolveServerPID
	defer func() { resolveServerPID = oldResolve }()
	resolveServerPID = stubResolve(99, nil)
	old := killFunc
	defer func() { killFunc = old }()
	var gotSig syscall.Signal
	killFunc = func(pid int, sig syscall.Signal) error {
		gotSig = sig
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), t.TempDir(), []string{"--force"}, &stdout, &stderr); code != 0 {
		t.Fatalf("force restart = %d, want 0", code)
	}
	if gotSig != syscall.SIGKILL {
		t.Fatalf("signal = %v, want SIGKILL", gotSig)
	}
}

func TestRestartKillFailureReportsNotRunning(t *testing.T) {
	oldResolve := resolveServerPID
	defer func() { resolveServerPID = oldResolve }()
	resolveServerPID = stubResolve(4321, nil)
	old := killFunc
	defer func() { killFunc = old }()
	killFunc = func(pid int, sig syscall.Signal) error {
		return errors.New("no such process")
	}
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), t.TempDir(), nil, &stdout, &stderr); code != 1 {
		t.Fatalf("dead server = %d, want 1", code)
	}
	if out := stderr.String(); !strings.Contains(out, "oswald: server is not running") {
		t.Fatalf("stderr = %q, want not-running", out)
	}
}

func TestRestartBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), t.TempDir(), []string{"--delay", "abc"}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad delay = %d, want 2", code)
	}
}

func TestHelpPages(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(t.Context(), root, args, &stdout, &stderr); code != 0 {
			t.Fatalf("restart %v = %d, want exit 0", args, code)
		} else if out := stdout.String(); out == "" || !strings.Contains(out, "usage: oswald restart") {
			t.Fatalf("restart %v missing usage: %q", args, out)
		}
	}
}
