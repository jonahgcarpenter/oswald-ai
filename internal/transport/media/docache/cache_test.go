package docache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
)

func TestSaveResolveRoundTrip(t *testing.T) {
	root := t.TempDir()
	c := New(filepath.Join(root, "home"))
	ctx := context.Background()
	data := []byte("hello document")
	path, err := c.Save(ctx, "alice", "notes.txt", data)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	got, mime, err := c.Resolve(ctx, "alice", path)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("round trip mismatch: %q", got)
	}
	if mime == "" {
		t.Fatal("expected MIME type")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("private file has loose permissions: %v", info.Mode())
	}
}

func TestResolveEnforcesOwnership(t *testing.T) {
	root := t.TempDir()
	c := New(filepath.Join(root, "home"))
	ctx := context.Background()
	path, err := c.Save(ctx, "alice", "notes.txt", []byte("secret"))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, _, err := c.Resolve(ctx, "bob", path); err == nil {
		t.Fatal("cross-profile resolve should fail")
	}
}

func TestSaveRejectsUnsupported(t *testing.T) {
	root := t.TempDir()
	c := New(filepath.Join(root, "home"))
	ctx := context.Background()
	if _, err := c.Save(ctx, "alice", "tool.exe", []byte("MZ")); err == nil {
		t.Fatal("unsupported extension should be rejected")
	}
	if _, err := c.Save(ctx, "alice", "empty.txt", nil); err == nil {
		t.Fatal("empty payload should be rejected")
	}
}

func TestSweepRemovesExpired(t *testing.T) {
	prune.SetBirthtimeHookForTest(func(string) (time.Time, bool) { return time.Time{}, false })
	t.Cleanup(func() { prune.SetBirthtimeHookForTest(nil) })
	root := t.TempDir()
	c := New(filepath.Join(root, "home"))
	ctx := context.Background()
	path, err := c.Save(ctx, "alice", "old.txt", []byte("stale"))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(path, now.Add(-25*time.Hour), now.Add(-25*time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	counts, err := c.Sweep(ctx, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if counts.RemovedFiles != 1 {
		t.Fatalf("removed = %d, want 1", counts.RemovedFiles)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expired file should be gone")
	}
}

func TestDeleteUser(t *testing.T) {
	root := t.TempDir()
	c := New(filepath.Join(root, "home"))
	ctx := context.Background()
	path, err := c.Save(ctx, "alice", "notes.txt", []byte("data"))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := c.DeleteUser(ctx, "alice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("user files should be gone")
	}
}
