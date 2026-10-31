package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
		{"run is service only", []string{"run"}, exitUsage},
		{"setup with arguments", []string{"setup", "extra"}, exitUsage},
		{"global help flag", []string{"--help"}, exitOK},
		{"help command", []string{"help"}, exitOK},
		{"profile without subcommand", []string{"profile"}, exitUsage},
		{"gateway without subcommand", []string{"gateway"}, exitUsage},
		{"runtime without subcommand", []string{"runtime"}, exitUsage},
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

func TestRootScopeMismatch(t *testing.T) {
	global := t.TempDir()
	bob := filepath.Join(global, "profiles", "bob")
	if err := os.MkdirAll(bob, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_HOME", bob)

	var stdout, stderr bytes.Buffer
	got := Run(context.Background(), []string{"--root", t.TempDir(), "config", "show"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("mismatched --root = %d, want %d (stderr=%q)", got, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not match") {
		t.Fatalf("mismatch diagnostic = %q", stderr.String())
	}
}
