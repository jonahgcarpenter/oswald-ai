package docache

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMediaCacheSaveResolveSweep(t *testing.T) {
	root := t.TempDir()
	c := NewMedia(root)
	ctx := context.Background()

	path, err := c.Save(ctx, "alice", "clip.mp4", []byte("synthetic video"))
	if err != nil {
		t.Fatalf("save media: %v", err)
	}
	if !strings.Contains(path, string(os.PathSeparator)+"media"+string(os.PathSeparator)) {
		t.Fatalf("media path = %q", path)
	}
	data, _, err := c.Resolve(ctx, "alice", path)
	if err != nil || string(data) != "synthetic video" {
		t.Fatalf("resolve media = %q %v", data, err)
	}

	// Audio and catch-all extensions are both accepted.
	if _, err := c.Save(ctx, "alice", "voice.m4a", []byte("audio")); err != nil {
		t.Fatalf("media cache rejected audio: %v", err)
	}
	if _, err := c.Save(ctx, "alice", "bundle.zip", []byte("zip")); err != nil {
		t.Fatalf("media cache rejected catch-all attachment: %v", err)
	}
	if _, err := c.Save(ctx, "alice", "file.verylongextension", []byte("nope")); err == nil {
		t.Fatal("media cache accepted an unsafe extension")
	}

	// The document and media caches stay separate.
	if _, _, err := New(root).Resolve(ctx, "alice", path); err == nil {
		t.Fatal("documents cache resolved a media path")
	}

	counts, err := c.Sweep(ctx, time.Now().Add(lifetime+time.Minute))
	if err != nil || counts.RemovedFiles != 3 {
		t.Fatalf("sweep = %+v %v", counts, err)
	}
}

func TestMediaCacheProfileRepair(t *testing.T) {
	root := t.TempDir()
	dir := root + "/cache/media"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	RepairMediaProfile(root, "alice")
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("media dir perms = %v %v", info.Mode().Perm(), err)
	}
}
