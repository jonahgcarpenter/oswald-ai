package workspace

import (
	"os"
	"strings"
	"testing"
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
