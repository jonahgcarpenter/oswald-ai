package workspace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func chtimes(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestSweepRemovesIdleFilesKeepsFresh(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "cache", "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	fresh := time.Now()

	oldFile := filepath.Join(scratch, "old.txt")
	freshFile := filepath.Join(scratch, "fresh.txt")
	if err := os.WriteFile(oldFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(freshFile, []byte("fresh"), 0600); err != nil {
		t.Fatal(err)
	}
	chtimes(t, oldFile, old)
	chtimes(t, freshFile, fresh)
	// Stabilize the scratch dir mtime so the dir itself is not a factor.
	chtimes(t, scratch, fresh)

	counts, err := Sweep(context.Background(), root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if counts.RemovedFiles != 1 {
		t.Fatalf("removed files = %d, want 1", counts.RemovedFiles)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatalf("old file survives: %v", err)
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Fatalf("fresh file missing: %v", err)
	}
	// Outside scratch is untouched.
	sibling := filepath.Join(root, "cache", "scratch-gopath")
	if err := os.MkdirAll(sibling, 0700); err != nil {
		t.Fatal(err)
	}
	siblingFile := filepath.Join(sibling, "keep.txt")
	if err := os.WriteFile(siblingFile, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	chtimes(t, siblingFile, old)
	if _, err := Sweep(context.Background(), root, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(siblingFile); err != nil {
		t.Fatalf("sibling outside scratch was swept: %v", err)
	}
}

func TestSweepNestedFreshChildProtectsAncestors(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "cache", "scratch")
	nested := filepath.Join(scratch, "exp1", "sub")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	fresh := time.Now()
	freshFile := filepath.Join(nested, "fresh.txt")
	if err := os.WriteFile(freshFile, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	chtimes(t, freshFile, fresh)
	// Old ancestor mtimes must not doom a fresh subtree.
	chtimes(t, nested, old)
	chtimes(t, filepath.Join(scratch, "exp1"), old)

	if _, err := Sweep(context.Background(), root, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Fatalf("fresh nested file swept: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("fresh subtree dir swept: %v", err)
	}
}

func TestSweepRemovesEmptyOldDirsKeepsRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "cache", "scratch")
	empty := filepath.Join(scratch, "empty-old")
	if err := os.MkdirAll(empty, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	chtimes(t, empty, old)

	counts, err := Sweep(context.Background(), root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if counts.RemovedDirs != 1 {
		t.Fatalf("removed dirs = %d, want 1", counts.RemovedDirs)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatalf("empty old dir survives: %v", err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch root removed: %v", err)
	}
}

func TestSweepSkipsSymlinksAndMissing(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	// Missing scratch is a no-op.
	if _, err := Sweep(context.Background(), root, time.Now()); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "cache", "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	link := filepath.Join(scratch, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Sweep(context.Background(), root, time.Now()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink not preserved: %v %v", info, err)
	}
}

func TestScratchWorkerImmediateSweep(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "cache", "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scratch, "old.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	chtimes(t, path, old)

	var logs bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&logs)
	w := NewWorker(root, log)
	w.Start()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		select {
		case <-deadline:
			t.Fatal("immediate sweep did not remove expired scratch file")
		case <-time.After(10 * time.Millisecond):
		}
	}
	w.Stop()
	w.Stop()
	if !bytes.Contains(logs.Bytes(), []byte(`"removed_file_count":1`)) {
		t.Fatalf("missing successful sweep measurement: %s", logs.String())
	}
}
