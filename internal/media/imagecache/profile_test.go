package imagecache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProfileCacheUsesDirectRootAndSweepsOnlyOwnImages(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	cache := NewProfileCache(root, "alice")
	ctx := context.Background()
	path, err := cache.Save(ctx, "alice", samplePNG(t), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, filepath.Join(root, ".cache", "images")+string(os.PathSeparator)) {
		t.Fatal("incorrect profile image path")
	}
	if _, _, err := cache.Resolve(ctx, "bob", path); err == nil {
		t.Fatal("cross-profile cache access accepted")
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	counts, err := cache.Sweep(ctx, time.Now())
	if err != nil || counts.RemovedFiles != 1 {
		t.Fatal("profile sweep failed", err)
	}
	missing := NewProfileCache(filepath.Join(root, "missing"), "alice")
	if _, err := missing.Save(ctx, "alice", samplePNG(t), "image/png"); err == nil {
		t.Fatal("missing operator profile created")
	}
}
