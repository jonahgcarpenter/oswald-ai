package docache

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMotionCacheSaveResolveSweep(t *testing.T) {
	root := t.TempDir()
	c := NewMotion(root)
	ctx := context.Background()

	path, err := c.Save(ctx, "alice", "clip.mp4", []byte("synthetic video"))
	if err != nil {
		t.Fatalf("save motion: %v", err)
	}
	if !strings.Contains(path, string(os.PathSeparator)+"videos"+string(os.PathSeparator)) {
		t.Fatalf("motion path = %q", path)
	}
	data, _, err := c.Resolve(ctx, "alice", path)
	if err != nil || string(data) != "synthetic video" {
		t.Fatalf("resolve motion = %q %v", data, err)
	}

	// The document and motion caches stay separate.
	if _, _, err := New(root).Resolve(ctx, "alice", path); err == nil {
		t.Fatal("documents cache resolved a motion path")
	}
	if _, err := c.Save(ctx, "alice", "notes.txt", []byte("nope")); err == nil {
		t.Fatal("motion cache accepted a text file")
	}

	counts, err := c.Sweep(ctx, time.Now().Add(lifetime+time.Minute))
	if err != nil || counts.RemovedFiles != 1 {
		t.Fatalf("sweep = %+v %v", counts, err)
	}
}

func TestMotionCacheProfileRepair(t *testing.T) {
	root := t.TempDir()
	dir := root + "/cache/videos"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	RepairMotionProfile(root, "alice")
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("motion dir perms = %v %v", info.Mode().Perm(), err)
	}
}
