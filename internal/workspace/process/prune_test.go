package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
)

func TestPruneOldRemovesCreationAgedCompleted(t *testing.T) {
	prune.SetBirthtimeHookForTest(func(string) (time.Time, bool) { return time.Time{}, false })
	t.Cleanup(func() { prune.SetBirthtimeHookForTest(nil) })
	root := t.TempDir()
	m := NewManager(root)
	info, err := m.Spawn("req-1", "echo hi", t.TempDir(), t.TempDir(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(info.ID, 10); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "cache", "processes", info.ID)
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"meta.json", "output.log"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	counts, err := m.PruneOld(context.Background(), time.Now())
	if err != nil || counts.RemovedDirs != 1 {
		t.Fatalf("prune = %+v %v", counts, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("aged record survived: %v", err)
	}
}

func TestPruneOldKeepsFreshAndRunning(t *testing.T) {
	prune.SetBirthtimeHookForTest(func(string) (time.Time, bool) { return time.Time{}, false })
	t.Cleanup(func() { prune.SetBirthtimeHookForTest(nil) })
	root := t.TempDir()
	m := NewManager(root)
	done, err := m.Spawn("req-d", "echo done", t.TempDir(), t.TempDir(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(done.ID, 10); err != nil {
		t.Fatal(err)
	}
	live, err := m.Spawn("req-l", "sleep 30", t.TempDir(), t.TempDir(), 60)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Kill(live.ID) })
	// Age everything on disk; only the completed record may go.
	old := time.Now().Add(-25 * time.Hour)
	base := filepath.Join(root, "cache", "processes")
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(base, e.Name())
		_ = os.Chtimes(p, old, old)
	}
	counts, err := m.PruneOld(context.Background(), time.Now())
	if err != nil || counts.RemovedDirs != 1 {
		t.Fatalf("prune = %+v %v", counts, err)
	}
	if _, err := os.Stat(filepath.Join(base, live.ID)); err != nil {
		t.Fatalf("running record pruned: %v", err)
	}
}

func TestPruneOldMissingBaseIsNoOp(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "missing"))
	if _, err := m.PruneOld(context.Background(), time.Now()); err != nil {
		t.Fatalf("missing base must be a no-op: %v", err)
	}
}
