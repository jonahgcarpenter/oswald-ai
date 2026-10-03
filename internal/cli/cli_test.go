package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunUsageAndDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no arguments", nil, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"global help flag", []string{"--help"}, exitOK},
		{"help command", []string{"help"}, exitOK},
		{"profile without subcommand", []string{"profile"}, exitUsage},
		{"unknown profile command", []string{"profile", "frobnicate"}, exitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := Run(context.Background(), tc.args, strings.NewReader(""), &stdout, &stderr)
			if got != tc.want {
				t.Fatalf("Run(%v) = %d, want %d (stderr=%q)", tc.args, got, tc.want, stderr.String())
			}
		})
	}
}

func TestRunConfigFailureReturnsErrorWithoutBanner(t *testing.T) {
	var stdout, stderr bytes.Buffer
	got := Run(context.Background(), []string{"--root", t.TempDir(), "run"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run with invalid configuration = %d, want %d", got, exitError)
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected banner on non-terminal stdout: %q", stdout.String())
	}
}
