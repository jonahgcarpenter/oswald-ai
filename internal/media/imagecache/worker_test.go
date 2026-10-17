package imagecache

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/prune"
)

func TestSweepHookImmediateSweepAndJoinedStop(t *testing.T) {
	prune.SetBirthtimeHookForTest(func(string) (time.Time, bool) { return time.Time{}, false })
	t.Cleanup(func() { prune.SetBirthtimeHookForTest(nil) })
	root := t.TempDir()
	cache := New(root)
	path, err := cache.Save(context.Background(), "alice", samplePNG(t), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&logs)
	w := &prune.Worker{}
	w.Add(SweepHook(cache, log))
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
			t.Fatal("immediate sweep did not remove expired image")
		case <-time.After(10 * time.Millisecond):
		}
	}
	w.Stop()
	w.Stop()
	if !bytes.Contains(logs.Bytes(), []byte(`"removed_file_count":1`)) {
		t.Fatalf("missing successful immediate sweep measurement: %s", logs.String())
	}
}

func TestSweepUsesCreationTime(t *testing.T) {
	born := time.Now().Add(-25 * time.Hour)
	prune.SetBirthtimeHookForTest(func(string) (time.Time, bool) { return born, true })
	t.Cleanup(func() { prune.SetBirthtimeHookForTest(nil) })
	root := t.TempDir()
	cache := New(root)
	path, err := cache.Save(context.Background(), "alice", samplePNG(t), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	// Fresh mtime must not save an old creation: the file is expired.
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	counts, err := cache.Sweep(context.Background(), now)
	if err != nil || counts.RemovedFiles != 1 {
		t.Fatalf("creation-aged sweep = %+v %v", counts, err)
	}
}
