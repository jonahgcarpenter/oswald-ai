package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
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

func TestTerminalPassthroughResolvesProfileScope(t *testing.T) {
	tree, alice, _ := profileTree(t)
	if err := os.WriteFile(filepath.Join(tree, ".env"), []byte("SYNTH_SHARED_VAR=synthetic-shared\nSYNTH_LAUNCH_ONLY=synthetic-launch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alice, ".env"), []byte("SYNTH_PROFILE_VAR=synthetic-profile\n"), 0600); err != nil {
		t.Fatal(err)
	}
	process.ClearEnvPassthrough()
	defer process.ClearEnvPassthrough()
	if refused := process.RegisterEnvPassthrough([]string{"SYNTH_SHARED_VAR", "SYNTH_PROFILE_VAR", "OPENAI_API_KEY"}); len(refused) != 1 {
		t.Fatalf("refused = %v", refused)
	}
	ws := NewWorkspace(alice, "alice")
	term := NewTerminalHandler(ws, nil)
	res, err := term(testContext("req-passthrough"), map[string]interface{}{
		"command": "printf '%s|%s|%s' \"$SYNTH_SHARED_VAR\" \"$SYNTH_PROFILE_VAR\" \"$SYNTH_LAUNCH_ONLY\"",
	})
	if err != nil {
		t.Fatalf("terminal failed: %v", err)
	}
	if !strings.Contains(res.Content, "synthetic-shared|synthetic-profile|") {
		t.Fatalf("passthrough values missing: %q", res.Content)
	}
	if strings.Contains(res.Content, "synthetic-launch") {
		t.Fatalf("unregistered name leaked: %q", res.Content)
	}
}
