package workspace

import (
	"os"
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
