package prune

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func writeFile(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestAgeOfClocks(t *testing.T) {
	SetBirthtimeHookForTest(nil)
	path := filepath.Join(t.TempDir(), "f")
	mod := time.Now().Add(-time.Hour)
	writeFile(t, path, mod)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := AgeOf(path, info, time.Now(), ClockMtime); !got.Equal(mod) {
		t.Fatalf("mtime clock = %v, want %v", got, mod)
	}
	born := mod.Add(-time.Hour)
	SetBirthtimeHookForTest(func(string) (time.Time, bool) { return born, true })
	t.Cleanup(func() { SetBirthtimeHookForTest(nil) })
	if got := AgeOf(path, info, time.Now(), ClockCreation); !got.Equal(born) {
		t.Fatalf("creation clock = %v, want %v", got, born)
	}
	// Future birthtime falls back to mtime.
	SetBirthtimeHookForTest(func(string) (time.Time, bool) { return time.Now().Add(time.Hour), true })
	if got := AgeOf(path, info, time.Now(), ClockCreation); !got.Equal(mod) {
		t.Fatalf("future birthtime fallback = %v, want %v", got, mod)
	}
	// Missing birthtime falls back to mtime.
	SetBirthtimeHookForTest(func(string) (time.Time, bool) { return time.Time{}, false })
	if got := AgeOf(path, info, time.Now(), ClockCreation); !got.Equal(mod) {
		t.Fatalf("missing birthtime fallback = %v, want %v", got, mod)
	}
}

func TestSweepSpecMatrix(t *testing.T) {
	SetBirthtimeHookForTest(func(string) (time.Time, bool) { return time.Time{}, false })
	t.Cleanup(func() { SetBirthtimeHookForTest(nil) })
	now := time.Now()
	old := now.Add(-25 * time.Hour)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "old.txt"), old)
	writeFile(t, filepath.Join(root, "fresh.txt"), now)
	writeFile(t, filepath.Join(root, "sub", "old-nested.txt"), old)
	writeFile(t, filepath.Join(root, "sub", "fresh-nested.txt"), now)
	// Old parent kept alive by a fresh child.
	writeFile(t, filepath.Join(root, "mixed", "fresh.txt"), now)
	if err := os.Chtimes(filepath.Join(root, "mixed"), old, old); err != nil {
		t.Fatal(err)
	}
	// Symlinks are skipped, never followed.
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	counts, err := Sweep(context.Background(), Spec{Name: "t", Dir: root, TTL: 24 * time.Hour, Clock: ClockMtime}, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts.RemovedFiles != 2 {
		t.Fatalf("removed files = %d, want 2", counts.RemovedFiles)
	}
	for _, want := range []string{"fresh.txt", "sub/fresh-nested.txt", "mixed/fresh.txt", "link"} {
		if _, err := os.Lstat(filepath.Join(root, want)); err != nil {
			t.Fatalf("expected survivor %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "mixed")); err != nil {
		t.Fatalf("old parent with fresh child removed: %v", err)
	}
}

func TestSweepMatchAndValidation(t *testing.T) {
	SetBirthtimeHookForTest(func(string) (time.Time, bool) { return time.Time{}, false })
	t.Cleanup(func() { SetBirthtimeHookForTest(nil) })
	now := time.Now()
	old := now.Add(-25 * time.Hour)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep.log"), old)
	writeFile(t, filepath.Join(root, "drop.tmp"), old)
	counts, err := Sweep(context.Background(), Spec{
		Name: "t", Dir: root, TTL: 24 * time.Hour, Clock: ClockMtime,
		Match: func(base string) bool { return filepath.Ext(base) == ".tmp" },
	}, now)
	if err != nil || counts.RemovedFiles != 1 {
		t.Fatalf("match sweep = %+v %v", counts, err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep.log")); err != nil {
		t.Fatalf("non-matching file removed: %v", err)
	}
	if _, err := Sweep(context.Background(), Spec{Name: "t", Dir: filepath.Join(root, "missing"), TTL: time.Hour, Clock: ClockMtime}, now); err != nil {
		t.Fatalf("missing dir must be a no-op: %v", err)
	}
	if _, err := Sweep(context.Background(), Spec{Name: "t", Dir: root, TTL: 0, Clock: ClockMtime}, now); err == nil {
		t.Fatal("non-positive TTL accepted")
	}
}

func TestWorkerFailureIsolationAndStop(t *testing.T) {
	var logs bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&logs)
	ranFail := &atomic.Bool{}
	ranOK := &atomic.Bool{}
	fail := Hook{
		Name: "fail", FailedEvent: "test.sweep.failed", FailedMessage: "failed",
		CompleteEvent: "test.sweep.complete", CompleteMessage: "complete", Log: log,
		Sweep: func(context.Context, time.Time) (Counts, error) {
			ranFail.Store(true)
			return Counts{}, errors.New("boom")
		},
	}
	ok := Hook{
		Name: "ok", FailedEvent: "test.sweep.failed", FailedMessage: "failed",
		CompleteEvent: "test2.sweep.complete", CompleteMessage: "complete", Log: log,
		Sweep: func(context.Context, time.Time) (Counts, error) {
			ranOK.Store(true)
			return Counts{RemovedFiles: 1}, nil
		},
	}
	w := &Worker{}
	w.Add(fail)
	w.Add(ok)
	w.Add(Hook{Name: "nil-sweep", Log: log})
	w.Start()
	deadline := time.After(5 * time.Second)
	for {
		if ranFail.Load() && ranOK.Load() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("hooks did not run")
		case <-time.After(10 * time.Millisecond):
		}
	}
	w.Stop()
	w.Stop()
	out := logs.String()
	if !bytes.Contains([]byte(out), []byte(`"event":"test.sweep.failed"`)) {
		t.Fatalf("missing WARN diagnostic: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte(`"event":"test2.sweep.complete"`)) {
		t.Fatalf("failing hook skipped the next hook: %s", out)
	}
	// A canceled sweep reports ok/canceled without a WARN diagnostic.
	var logs2 bytes.Buffer
	log2 := config.NewLogger(config.LevelDebug)
	log2.SetOutput(&logs2)
	w2 := &Worker{}
	w2.Add(Hook{
		Name: "cancel", FailedEvent: "cancel.sweep.failed", FailedMessage: "failed",
		CompleteEvent: "cancel.sweep.complete", CompleteMessage: "complete", Log: log2,
		Sweep: func(ctx context.Context, _ time.Time) (Counts, error) {
			return Counts{}, ctx.Err()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w2.sweepAll(ctx)
	out2 := logs2.String()
	if !bytes.Contains([]byte(out2), []byte(`"outcome":"canceled"`)) {
		t.Fatalf("missing canceled outcome: %s", out2)
	}
	if bytes.Contains([]byte(out2), []byte(`"event":"cancel.sweep.failed"`)) {
		t.Fatalf("cancellation must not warn: %s", out2)
	}
}
