package restart

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRestartMissingPIDFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), t.TempDir(), nil, &stdout, &stderr); code != 1 {
		t.Fatalf("missing pid = %d, want 1", code)
	}
}

func TestRestartInvalidPIDFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, PIDFileName), []byte("not-a-pid"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), root, nil, &stdout, &stderr); code != 1 {
		t.Fatalf("invalid pid = %d, want 1", code)
	}
}

func TestRestartSignalsPID(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, PIDFileName), []byte("12345\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := killFunc
	defer func() { killFunc = old }()
	var gotPID int
	var gotSig syscall.Signal
	killFunc = func(pid int, sig syscall.Signal) error {
		gotPID, gotSig = pid, sig
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), root, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("restart = %d, want 0: %s", code, stderr.String())
	}
	if gotPID != 12345 || gotSig != syscall.SIGTERM {
		t.Fatalf("signal = pid %d sig %v, want pid 12345 SIGTERM", gotPID, gotSig)
	}
}

func TestRestartForceSendsSIGKILL(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, PIDFileName), []byte("99"), 0600); err != nil {
		t.Fatal(err)
	}
	old := killFunc
	defer func() { killFunc = old }()
	var gotSig syscall.Signal
	killFunc = func(pid int, sig syscall.Signal) error {
		gotSig = sig
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), root, []string{"--force"}, &stdout, &stderr); code != 0 {
		t.Fatalf("force restart = %d, want 0", code)
	}
	if gotSig != syscall.SIGKILL {
		t.Fatalf("signal = %v, want SIGKILL", gotSig)
	}
}

func TestRestartBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), t.TempDir(), []string{"--delay", "abc"}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad delay = %d, want 2", code)
	}
}
