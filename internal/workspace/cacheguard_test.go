package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

func TestCacheReadsAllowedWritesDenied(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	allowWrite := []string{
		"cache/scratch/a.txt",
		"cache/scratch/nested/b.txt",
	}
	for _, raw := range allowWrite {
		if _, _, err := ws.ResolveWrite(raw); err != nil {
			t.Errorf("scratch write rejected %q: %v", raw, err)
		}
	}
	readOnly := []string{
		"cache",
		"cache/scratch_go/x",
		"cache/scratch-gopath/x",
		"cache/processes/x",
		"cache/images/x",
		"cache/foo",
		"cache/browser/scratch-abc.txt",
		"cache/browser/profile-abc",
	}
	for _, raw := range readOnly {
		if _, _, err := ws.Resolve(raw); err != nil {
			t.Errorf("managed cache read denied %q: %v", raw, err)
			continue
		}
		_, _, err := ws.ResolveWrite(raw)
		if err == nil {
			t.Errorf("managed cache write allowed: %q", raw)
			continue
		}
		rel, group, ok := AsDeny(err)
		if !ok || group != GroupSystem {
			t.Errorf("wrong deny for %q: %v (rel=%q group=%d ok=%v)", raw, err, rel, group, ok)
		}
		if msg := DenyMessage(rel, group, true); !strings.Contains(msg, "cache/scratch") && !strings.Contains(msg, "system-managed") {
			t.Errorf("deny message missing for %q: %q", raw, msg)
		}
	}
}

func TestScanManagedCacheShell(t *testing.T) {
	root := t.TempDir()
	deny := []string{
		"mkdir cache/scratch_go",
		"mkdir cache/foo && touch cache/foo/a",
		"GOPATH=cache/scratch-gopath go build ./...",
		"ls cache/processes",
		"cat cache/images/x",
		"cp out.bin cache/foo.bin",
		"mkdir " + filepath.Join(root, "cache", "scratch_go"),
	}
	for _, cmd := range deny {
		if !ScanManagedCacheShell(cmd, root) {
			t.Errorf("shell allowed managed cache: %q", cmd)
		}
	}
	allow := []string{
		"echo hi",
		"ls",
		"mkdir cache/scratch/exp1",
		"ls cache/scratch",
		"touch cache/scratch/out.txt",
		`echo "mkdir cache/foo"`,
		"go test ./...",
	}
	for _, cmd := range allow {
		if ScanManagedCacheShell(cmd, root) {
			t.Errorf("shell denied safe command: %q", cmd)
		}
	}
}

func TestScanManagedCacheCode(t *testing.T) {
	root := t.TempDir()
	deny := []string{
		`open('cache/foo', 'w')`,
		`open("cache/scratch_go/x").read()`,
		`import os; os.makedirs("cache/foo")`,
		`from pathlib import Path; Path("cache/foo").mkdir()`,
		`open('cache/images/x', 'rb').read()`,
		`open(os.path.join("cache", "x"), "w")`,
		`open('` + filepath.Join(root, "cache", "foo") + `')`,
	}
	for _, code := range deny {
		if !ScanManagedCacheCode(code, root) {
			t.Errorf("code allowed managed cache: %q", code)
		}
	}
	allow := []string{
		`print(40+2)`,
		`open('cache/scratch/exp/out.txt').read()`,
		`open('notes.md').read()`,
		`print("cache")`,
	}
	for _, code := range allow {
		if ScanManagedCacheCode(code, root) {
			t.Errorf("code denied safe snippet: %q", code)
		}
	}
}

func TestTerminalDeniesManagedCache(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	res, err := term(testContext("req-cache"), map[string]interface{}{
		"command": "mkdir cache/scratch_go",
	})
	if err != nil {
		t.Fatalf("deny returned error: %v", err)
	}
	if !strings.Contains(res.Content, `"status":"blocked"`) || !strings.Contains(res.Content, "cache/scratch") {
		t.Fatalf("missing cache blocked envelope: %q", res.Content)
	}
}

func TestExecuteCodeDeniesManagedCache(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	exec := NewExecuteCodeHandler(ws)
	_, err := exec(testContext("req-ccode"), map[string]interface{}{
		"code": `open('cache/foo', 'w')`,
	})
	if err == nil || !strings.Contains(err.Error(), "cache/scratch") {
		t.Fatalf("code allowed managed cache write: %v", err)
	}
}
