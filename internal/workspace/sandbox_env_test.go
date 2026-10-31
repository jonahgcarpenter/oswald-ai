package workspace

import (
	"context"
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

func TestTerminalSetsOswaldHome(t *testing.T) {
	_, alice, _ := profileTree(t)
	ws := NewWorkspace(alice, "alice")
	term := NewTerminalHandler(ws, nil)
	res, err := term(testContext("req-home"), map[string]interface{}{
		"command": "printf %s \"$OSWALD_HOME\"",
	})
	if err != nil {
		t.Fatalf("terminal failed: %v", err)
	}
	if !strings.Contains(res.Content, alice) {
		t.Fatalf("OSWALD_HOME missing profile root: %q", res.Content)
	}
}

func TestExecuteCodeSetsOswaldHome(t *testing.T) {
	_, alice, _ := profileTree(t)
	ws := NewWorkspace(alice, "alice")
	exec := NewExecuteCodeHandler(ws)
	res, err := exec(testContext("req-home"), map[string]interface{}{
		"code": "import os; print(os.environ.get('OSWALD_HOME', ''))",
	})
	if err != nil {
		t.Fatalf("execute_code failed: %v", err)
	}
	if !strings.Contains(res.Content, alice) {
		t.Fatalf("OSWALD_HOME missing profile root: %q", res.Content)
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
