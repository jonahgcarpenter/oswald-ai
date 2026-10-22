package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionDirLivesUnderCache(t *testing.T) {
	root := t.TempDir()
	dir, err := sessionDir(root)
	if err != nil {
		t.Fatal(err)
	}
	wantBase := filepath.Join(root, "cache", "browser-profiles") + string(filepath.Separator)
	if !strings.HasPrefix(dir, wantBase) {
		t.Fatalf("session dir = %q, want under %q", dir, wantBase)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("session dir = %+v %v", info, err)
	}
	again, err := sessionDir(root)
	if err != nil || again == dir {
		t.Fatalf("session ids collide: %q %q %v", dir, again, err)
	}
}

func TestSweepStaleProfiles(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "oswald-browser-12345")
	if err := os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	stale, err := sessionDir(root)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh session dir proves the sweeper only runs at startup: a live
	// backend session must never be reaped by age here.
	if got := SweepStaleProfiles(root); got != 2 {
		t.Fatalf("reaped %d, want 2", got)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy dir survived")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale session dir survived")
	}
	if got := SweepStaleProfiles(root); got != 0 {
		t.Fatalf("empty sweep reaped %d", got)
	}
	if got := SweepStaleProfiles(""); got != 0 {
		t.Fatalf("empty root sweep reaped %d", got)
	}
}

func TestSweepReapsOrphanProfileDirs(t *testing.T) {
	root := t.TempDir()
	base := sessionBaseDir(root)
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(base, "deadbeef")
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(base, "freshbeef")
	if err := os.Mkdir(fresh, 0700); err != nil {
		t.Fatal(err)
	}
	b := NewLocalBackend((Options{}).Effective(), root)
	if got := b.Sweep(time.Now()); got < 1 {
		t.Fatalf("orphan sweep reaped %d", got)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("old orphan survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh dir was reaped")
	}
}
