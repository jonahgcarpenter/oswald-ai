package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/prune"
)

func TestCacheChildrenHookPrunesUnknownSubdirs(t *testing.T) {
	prune.SetBirthtimeHookForTest(func(string) (time.Time, bool) { return time.Time{}, false })
	t.Cleanup(func() { prune.SetBirthtimeHookForTest(nil) })
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-25 * time.Hour)
	// Unknown future subdirectory is pruned by creation-fallback age.
	unknown := filepath.Join(root, "cache", "telemetry", "old.txt")
	if err := os.MkdirAll(filepath.Dir(unknown), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unknown, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Dir(unknown), old, old); err != nil {
		t.Fatal(err)
	}
	// Owned names are skipped even when old.
	owned := filepath.Join(root, "cache", "images", "old.bin")
	if err := os.MkdirAll(filepath.Dir(owned), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(owned, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	hook := CacheChildrenHook(root, config.NewLogger(config.LevelError), map[string]struct{}{
		"scratch": {}, "images": {}, "processes": {},
	})
	counts, err := hook.Sweep(context.Background(), now)
	if err != nil || counts.RemovedFiles != 1 {
		t.Fatalf("children sweep = %+v %v", counts, err)
	}
	if _, err := os.Stat(unknown); !os.IsNotExist(err) {
		t.Fatalf("unknown subdir survived: %v", err)
	}
	if _, err := os.Stat(owned); err != nil {
		t.Fatalf("owned subdir swept: %v", err)
	}
}

func TestCacheChildrenHookMissingCacheIsNoOp(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	hook := CacheChildrenHook(root, config.NewLogger(config.LevelError), map[string]struct{}{})
	if _, err := hook.Sweep(context.Background(), time.Now()); err != nil {
		t.Fatalf("missing cache must be a no-op: %v", err)
	}
}
