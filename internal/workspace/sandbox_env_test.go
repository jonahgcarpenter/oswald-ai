package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunShellSeesGitHubTokenOnly(t *testing.T) {
	t.Setenv("GH_TOKEN", "synthetic-gh-token")
	t.Setenv("DISCORD_TOKEN", "synthetic-discord-token")

	output, code, err := runShell(context.Background(), "echo canary-auth=$GH_TOKEN; echo other=$DISCORD_TOKEN", t.TempDir(), t.TempDir(), t.TempDir(), 30)
	if err != nil || code != 0 {
		t.Fatalf("runShell = code %d err %v output %q", code, err, output)
	}
	if !strings.Contains(output, "canary-auth=synthetic-gh-token") {
		t.Fatalf("GH_TOKEN not visible to foreground shell: %q", output)
	}
	if strings.Contains(output, "synthetic-discord-token") {
		t.Fatalf("non-allowlisted secret visible to foreground shell: %q", output)
	}
}

func TestRunShellIncludesOswaldBinInPath(t *testing.T) {
	output, code, err := runShell(context.Background(), `printf %s "$PATH"`, t.TempDir(), t.TempDir(), t.TempDir(), 30)
	if err != nil || code != 0 {
		t.Fatalf("runShell = code %d err %v output %q", code, err, output)
	}
	if !strings.Contains(output, "/opt/oswald") {
		t.Fatalf("terminal PATH missing /opt/oswald: %q", output)
	}
}

func TestRunShellHomeIsProfileRoot(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	output, code, err := runShell(context.Background(), `printf '%s|%s|%s|%s' "$HOME" "$OSWALD_HOME" "$OSWALD_WORKSPACE" "$OSWALD_SCRATCH"`, home, filepath.Join(home, "cache", "scratch"), home, 30)
	if err != nil || code != 0 {
		t.Fatalf("runShell = code %d err %v output %q", code, err, output)
	}
	if want := home + "|" + home + "||"; output != want {
		t.Fatalf("sandbox home vars = %q, want %q", output, want)
	}
}
