package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/approvals"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

// profileTree builds a fake .oswald data tree: <tmp>/.oswald with global
// operator files and two named profiles. It returns the tree root and the
// two profile roots.
func profileTree(t *testing.T) (tree, alice, bob string) {
	t.Helper()
	tree = filepath.Join(t.TempDir(), ".oswald")
	alice = filepath.Join(tree, "profiles", "alice")
	bob = filepath.Join(tree, "profiles", "bob")
	for _, d := range []string{tree, alice, bob} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bob, "SOUL.md"), []byte("bob soul\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "config.yaml"), []byte("x: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return tree, alice, bob
}

func TestScanConfinedWriteShell(t *testing.T) {
	_, alice, bob := profileTree(t)
	bobSoul := filepath.Join(bob, "SOUL.md")
	root := alice

	deny := map[string]string{
		"echo x > " + bobSoul:                    "system-managed",
		"echo x > " + filepath.Join(bob, ".env"): "protected",
		"echo x > /tmp/outside.txt":              "system-managed",
		"echo x > config.yaml":                   "oswald providers",
		"echo x > skills/review/SKILL.md":        "skill_manage tool",
		"sed -i s/a/b/ config.yaml":              "oswald providers",
		"cp new.yaml config.yaml":                "oswald providers",
		"echo x | tee cache/foo":                 "cache/scratch",
		"rm -rf cache/images":                    "cache/scratch",
		"mkdir cache/scratch_go":                 "cache/scratch",
		"cat config.yaml > /tmp/out.txt":         "system-managed",
		"echo x > .env":                          "protected",
	}
	for cmd, want := range deny {
		if reason, blocked := ScanConfinedWrite(cmd, alice, root, true); !blocked {
			t.Errorf("shell allowed confined write: %q", cmd)
		} else if !strings.Contains(reason, want) {
			t.Errorf("shell deny %q = %q, want %q", cmd, reason, want)
		}
	}

	allow := []string{
		"echo hi",
		"ls /tmp",
		"cat " + bobSoul,
		"cat config.yaml",
		"cat /etc/hostname",
		"cat /proc/cpuinfo",
		"cat ../alice/notes.md",
		`echo "echo x > /tmp/outside.txt"`,
		"echo hi > /dev/null",
		"mkdir cache/scratch/exp1",
		"touch cache/scratch/out.txt",
		"go test ./...",
		"curl -sSL https://example.com/f | head -5",
	}
	for _, cmd := range allow {
		if reason, blocked := ScanConfinedWrite(cmd, alice, root, true); blocked {
			t.Errorf("shell denied safe command %q: %q", cmd, reason)
		}
	}
}

func TestScanConfinedWriteDefault(t *testing.T) {
	tree, _, bob := profileTree(t)
	// Reads roam, including cross-profile and outside.
	for _, cmd := range []string{"cat profiles/bob/SOUL.md", "cat " + filepath.Join(bob, "SOUL.md"), "cat config.yaml"} {
		if reason, blocked := ScanConfinedWrite(cmd, tree, tree, true); blocked {
			t.Errorf("default shell denied read %q: %q", cmd, reason)
		}
	}
	// Writes stay confined.
	for _, cmd := range []string{"echo x > profiles/bob/notes.md", "cat x > /tmp/out.txt", "sed -i s/a/b/ config.yaml"} {
		if _, blocked := ScanConfinedWrite(cmd, tree, tree, true); !blocked {
			t.Errorf("default shell allowed write: %q", cmd)
		}
	}
}

func TestScanConfinedWriteCode(t *testing.T) {
	tree, alice, bob := profileTree(t)
	bobSoul := filepath.Join(bob, "SOUL.md")

	deny := map[string]string{
		"open('" + filepath.Join(tree, "cache", "foo") + "', 'w').write('x')":   "managed cache",
		"import os; os.makedirs('" + filepath.Join(tree, "cache", "foo") + "')": "managed cache",
		"open('" + bobSoul + "', 'w').write('x')":                               "system-managed",
		"open('" + filepath.Join(bob, "state.db") + "').read()":                 "protected",
		"open('" + filepath.Join(tree, ".env") + "').read()":                    "protected",
		"open('/tmp/outside.txt', 'w').write('x')":                              "system-managed",
	}
	for code, want := range deny {
		if reason, blocked := ScanConfinedWrite(code, alice, tree, false); !blocked {
			t.Errorf("code allowed confined write: %q", code)
		} else if !strings.Contains(reason, want) {
			t.Errorf("code deny %q = %q, want %q", code, reason, want)
		}
	}

	allow := []string{
		"print(40+2)",
		"open('" + bobSoul + "').read()",
		"open('notes.md').read()",
		"open('" + filepath.Join(tree, "cache", "scratch", "exp", "out.txt") + "').read()",
		"import urllib.request; print(urllib.request.urlopen('https://example.com').status)",
	}
	for _, code := range allow {
		if reason, blocked := ScanConfinedWrite(code, alice, tree, false); blocked {
			t.Errorf("code denied safe snippet %q: %q", code, reason)
		}
	}
}

func TestTerminalConfinedWriteEnvelope(t *testing.T) {
	_, alice, bob := profileTree(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(alice, "alice")
	term := NewTerminalHandler(ws, process.NewManager(alice))
	// Reads roam, including cross-profile.
	res, err := term(testContext("req-xread"), map[string]interface{}{
		"command": "cat " + filepath.Join(bob, "SOUL.md"),
	})
	if err != nil || !strings.Contains(res.Content, "bob soul") {
		t.Fatalf("cross-profile read failed: %+v %v", res, err)
	}
	// Writes stay confined with the group message.
	res, err = term(testContext("req-xwrite"), map[string]interface{}{
		"command": "echo x > " + filepath.Join(bob, "notes.md"),
	})
	if err != nil {
		t.Fatalf("deny returned error: %v", err)
	}
	if !strings.Contains(res.Content, `"status":"blocked"`) || !strings.Contains(res.Content, "system-managed") {
		t.Fatalf("missing blocked envelope: %q", res.Content)
	}
}

func TestExecuteCodeConfinedWrite(t *testing.T) {
	_, alice, bob := profileTree(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(alice, "alice")
	exec := NewExecuteCodeHandler(ws)
	res, err := exec(testContext("req-xcode"), map[string]interface{}{
		"code": "print(open('" + filepath.Join(bob, "SOUL.md") + "').read())",
	})
	if err != nil || !strings.Contains(res.Content, "bob soul") {
		t.Fatalf("cross-profile code read failed: %+v %v", res, err)
	}
	_, err = exec(testContext("req-xcode-w"), map[string]interface{}{
		"code": "open('" + filepath.Join(bob, "notes.md") + "', 'w').write('x')",
	})
	if err == nil || !strings.Contains(err.Error(), "system-managed") {
		t.Fatalf("code allowed cross-profile write: %v", err)
	}
}

func TestProcessManageWriteConfined(t *testing.T) {
	_, alice, bob := profileTree(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(alice, "alice")
	manage := NewProcessManageHandler(ws, process.NewManager(alice))
	_, err := manage(testContext("req-xwrite"), map[string]interface{}{
		"action":     "write",
		"session_id": "nosuch",
		"data":       "echo x > " + filepath.Join(bob, "notes.md"),
	})
	if err == nil || !strings.Contains(err.Error(), "system-managed") {
		t.Fatalf("write allowed cross-profile data: %v", err)
	}
}

func TestScanProtectedReadShell(t *testing.T) {
	_, alice, bob := profileTree(t)
	bobState := filepath.Join(bob, "state.db")

	deny := map[string]string{
		"cd " + bob + " && sqlite3 state.db \"SELECT id FROM sessions;\"": "session_search",
		"sqlite3 " + bobState + " \"SELECT id FROM sessions;\"":           "session_search",
		"cat " + bobState:                   "session_search",
		"cat .env":                          "protected",
		"cat " + filepath.Join(bob, ".env"): "protected",
		"cat config.yaml":                   "oswald config",
		"cat /proc/self/environ":            "protected",
	}
	for cmd, want := range deny {
		if reason, blocked := ScanProtectedRead(cmd, alice, alice, true); !blocked {
			t.Errorf("shell allowed protected read: %q", cmd)
		} else if !strings.Contains(reason, want) {
			t.Errorf("shell deny %q = %q, want %q", cmd, reason, want)
		}
	}

	allow := []string{
		"cat " + filepath.Join(bob, "SOUL.md"),
		"cat /etc/hostname",
		"sqlite3 state.db \"SELECT id FROM sessions;\"",
		`echo "cat .env"`,
		"grep -r credentials docs/",
		"env | head -5",
		"cmd < /dev/null",
		"ls",
		"go test ./...",
	}
	for _, cmd := range allow {
		if reason, blocked := ScanProtectedRead(cmd, alice, alice, true); blocked {
			t.Errorf("shell denied safe command %q: %q", cmd, reason)
		}
	}
}

func TestScanProtectedReadCode(t *testing.T) {
	_, alice, bob := profileTree(t)

	deny := map[string]string{
		"open('.env').read()":                                   "protected",
		"open('config.yaml').read()":                            "oswald config",
		"open('" + filepath.Join(bob, "state.db") + "').read()": "session_search",
		"open('" + filepath.Join(alice, ".env") + "').read()":   "protected",
		"open('credentials.json').read()":                       "protected",
	}
	for code, want := range deny {
		if reason, blocked := ScanProtectedRead(code, alice, alice, false); !blocked {
			t.Errorf("code allowed protected read: %q", code)
		} else if !strings.Contains(reason, want) {
			t.Errorf("code deny %q = %q, want %q", code, reason, want)
		}
	}

	allow := []string{
		"open('notes.md').read()",
		"open('" + filepath.Join(bob, "SOUL.md") + "').read()",
		"open('/etc/passwd').read()",
		"print('v1.2 done')",
		"open('state.db').read()",
	}
	for _, code := range allow {
		if reason, blocked := ScanProtectedRead(code, alice, alice, false); blocked {
			t.Errorf("code denied safe snippet %q: %q", code, reason)
		}
	}
}

func TestTerminalDeniesCrossProfileStateRead(t *testing.T) {
	_, alice, bob := profileTree(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(alice, "alice")
	term := NewTerminalHandler(ws, process.NewManager(alice))
	res, err := term(testContext("req-xstate"), map[string]interface{}{
		"command": "cd " + bob + " && sqlite3 state.db \"SELECT id FROM sessions;\"",
	})
	if err != nil {
		t.Fatalf("deny returned error: %v", err)
	}
	if !strings.Contains(res.Content, `"status":"blocked"`) || !strings.Contains(res.Content, "session_search") {
		t.Fatalf("missing blocked envelope: %q", res.Content)
	}
}

func TestExecuteApprovedRefusesProtectedRead(t *testing.T) {
	_, alice, bob := profileTree(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(alice, "alice")
	pending := approvals.Pending{Profile: "alice", SessionKey: "s", Command: "cat " + filepath.Join(bob, "state.db"), Workdir: alice, Scratch: alice, Timeout: 30}
	if _, err := ExecuteApproved(context.Background(), ws, process.NewManager(alice), pending); err == nil || !strings.Contains(err.Error(), "refused on re-scan") {
		t.Fatalf("replay allowed protected read: %v", err)
	}
}
