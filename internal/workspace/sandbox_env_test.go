package workspace

import (
	"context"
	"strings"
	"testing"
)

func TestRunShellSeesGitHubTokenOnly(t *testing.T) {
	t.Setenv("GH_TOKEN", "synthetic-gh-token")
	t.Setenv("DISCORD_TOKEN", "synthetic-discord-token")

	output, code, err := runShell(context.Background(), "echo canary-auth=$GH_TOKEN; echo other=$DISCORD_TOKEN", t.TempDir(), t.TempDir(), 30)
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
