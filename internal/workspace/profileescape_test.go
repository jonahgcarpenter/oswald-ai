package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if err := os.WriteFile(filepath.Join(bob, "SOUL.md"), []byte("bob soul"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "config.yaml"), []byte("x: 1"), 0600); err != nil {
		t.Fatal(err)
	}
	return tree, alice, bob
}

func TestScanProfileEscapeShell(t *testing.T) {
	tree, alice, bob := profileTree(t)
	bobSoul := filepath.Join(bob, "SOUL.md")

	deny := []string{
		"ls -la " + bob,
		"cat " + bobSoul,
		"cat " + bobSoul + " " + filepath.Join(tree, "config.yaml"),
		"cd " + bob + " && python3 -c \"import sqlite3\"",
		"cat ../bob/SOUL.md",
		"cat ../../bob/SOUL.md",
		"python3 -c \"import sqlite3; c = sqlite3.connect('file:" + filepath.Join(bob, "state.db") + "?mode=ro', uri=True)\"",
		"cat " + filepath.Join(tree, "config.yaml"),
		"DB=" + bobSoul + " python3 -c \"print(1)\"",
	}
	for _, cmd := range deny {
		if !ScanProfileEscapeShell(cmd, alice) {
			t.Errorf("shell allowed cross-profile: %q", cmd)
		}
	}

	allow := []string{
		"echo hi",
		"ls",
		"cat notes.md",
		"ls /tmp",
		"cd /tmp && ls",
		"cat " + filepath.Join(alice, "notes.md"),
		"cat ../alice/notes.md",
		`echo "cat ` + bobSoul + `"`,
		"curl -sL https://example.com/x | head -5",
	}
	for _, cmd := range allow {
		if ScanProfileEscapeShell(cmd, alice) {
			t.Errorf("shell denied own-profile command: %q", cmd)
		}
	}
}

func TestScanProfileEscapeShellHomeFold(t *testing.T) {
	home := t.TempDir()
	tree := filepath.Join(home, ".oswald")
	bob := filepath.Join(tree, "profiles", "bob")
	alice := filepath.Join(tree, "profiles", "alice")
	for _, d := range []string{alice, bob} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	abs := filepath.Join(bob, "SOUL.md")
	if !ScanProfileEscapeShell("cat "+abs, alice) {
		t.Fatalf("shell allowed home-folded cross-profile path: %q", abs)
	}
}

func TestScanProfileEscapeShellDefault(t *testing.T) {
	tree, _, bob := profileTree(t)
	if !ScanProfileEscapeShell("cat profiles/bob/SOUL.md", tree) {
		t.Error("default shell allowed profiles/ read")
	}
	if !ScanProfileEscapeShell("cat "+filepath.Join(bob, "SOUL.md"), tree) {
		t.Error("default shell allowed absolute profiles/ read")
	}
	if ScanProfileEscapeShell("cat config.yaml", tree) {
		t.Error("default shell denied own config read")
	}
}

func TestScanProfileEscapeCode(t *testing.T) {
	_, alice, bob := profileTree(t)
	bobSoul := filepath.Join(bob, "SOUL.md")

	deny := []string{
		"open('" + bobSoul + "').read()",
		"import sqlite3; c = sqlite3.connect('file:" + filepath.Join(bob, "state.db") + "?mode=ro', uri=True)",
		"import os; os.chdir('" + bob + "')",
		"open('../bob/SOUL.md').read()",
	}
	for _, code := range deny {
		if !ScanProfileEscapeCode(code, alice) {
			t.Errorf("code allowed cross-profile: %q", code)
		}
	}
	allow := []string{
		"print(40+2)",
		"open('notes.md').read()",
		"import urllib.request; print(urllib.request.urlopen('https://example.com').status)",
	}
	for _, code := range allow {
		if ScanProfileEscapeCode(code, alice) {
			t.Errorf("code denied own-profile snippet: %q", code)
		}
	}
}

func TestTerminalDeniesCrossProfile(t *testing.T) {
	_, alice, bob := profileTree(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(alice, "alice")
	term := NewTerminalHandler(ws, process.NewManager(alice))
	res, err := term(testContext("req-xprofile"), map[string]interface{}{
		"command": "cat " + filepath.Join(bob, "SOUL.md"),
	})
	if err != nil {
		t.Fatalf("deny returned error: %v", err)
	}
	if !strings.Contains(res.Content, `"status":"blocked"`) || !strings.Contains(res.Content, "cross-profile") {
		t.Fatalf("missing blocked envelope: %q", res.Content)
	}
}

func TestExecuteCodeDeniesCrossProfile(t *testing.T) {
	_, alice, bob := profileTree(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(alice, "alice")
	exec := NewExecuteCodeHandler(ws)
	_, err := exec(testContext("req-xcode"), map[string]interface{}{
		"code": "open('" + filepath.Join(bob, "SOUL.md") + "').read()",
	})
	if err == nil || !strings.Contains(err.Error(), "cross-profile") {
		t.Fatalf("code allowed cross-profile read: %v", err)
	}
}

func TestProcessManageWriteDeniesCrossProfile(t *testing.T) {
	_, alice, bob := profileTree(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(alice, "alice")
	manage := NewProcessManageHandler(ws, process.NewManager(alice))
	_, err := manage(testContext("req-xwrite"), map[string]interface{}{
		"action":     "write",
		"session_id": "nosuch",
		"data":       "cat " + filepath.Join(bob, "SOUL.md"),
	})
	if err == nil || !strings.Contains(err.Error(), "cross-profile") {
		t.Fatalf("write allowed cross-profile data: %v", err)
	}
}
