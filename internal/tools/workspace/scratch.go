// Scratch sweeping owns the 24h ephemeral semantics advertised in the system
// prompt: entries under <profile-root>/cache/scratch idle for 24h are
// pruned. The sweep never leaves the scratch subtree and never follows
// symlinks.
package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// scratchLifetime matches the system-prompt contract and the image-cache TTL.
const scratchLifetime = 24 * time.Hour

// Counts describes successful scratch sweep removals.
type Counts struct {
	RemovedFiles int
	RemovedDirs  int
	RemovedBytes int64
}

// Sweep removes scratch entries idle for 24h. Files are judged by their own
// mtime; directories by the newest mtime in their subtree so an old top
// directory with a fresh child survives. A missing scratch directory is a
// no-op. Symlinks and non-regular, non-directory files are skipped. The
// scratch root itself is never removed.
func Sweep(ctx context.Context, profileRoot string, now time.Time) (Counts, error) {
	var counts Counts
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	if err := validateRoot(profileRoot); err != nil {
		return counts, err
	}
	scratch := filepath.Join(profileRoot, "cache", "scratch")
	info, err := os.Lstat(scratch)
	if errors.Is(err, os.ErrNotExist) {
		return counts, nil
	}
	if err != nil {
		return counts, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return counts, errors.New("scratch directory is unsafe")
	}

	type fileEntry struct {
		path string
		size int64
		mod  time.Time
	}
	type dirEntry struct {
		path   string
		depth  int
		newest time.Time
	}
	var files []fileEntry
	var dirs []dirEntry
	dirIndex := make(map[string]int)

	err = filepath.WalkDir(scratch, func(path string, d os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		// Never follow symlinks: Lstat every path and skip links.
		linfo, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(scratch, path)
		if err != nil {
			return err
		}
		depth := 0
		if rel != "." {
			depth = 1
			for _, r := range rel {
				if r == filepath.Separator {
					depth++
				}
			}
		}
		if linfo.IsDir() {
			dirIndex[path] = len(dirs)
			dirs = append(dirs, dirEntry{path: path, depth: depth, newest: linfo.ModTime()})
			return nil
		}
		if !linfo.Mode().IsRegular() {
			return nil
		}
		if path == scratch {
			return nil
		}
		files = append(files, fileEntry{path: path, size: linfo.Size(), mod: linfo.ModTime()})
		// Propagate file mtime up to ancestor dirs for subtree-newest.
		dir := filepath.Dir(path)
		for {
			if idx, ok := dirIndex[dir]; ok {
				if files[len(files)-1].mod.After(dirs[idx].newest) {
					dirs[idx].newest = files[len(files)-1].mod
				}
			}
			if dir == scratch {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		return nil
	})
	if err != nil {
		return counts, err
	}
	// Bubble child-dir newness up so a nested fresh file protects its ancestors.
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].depth > dirs[j].depth })
	for _, d := range dirs {
		if d.path == scratch {
			continue
		}
		parent := filepath.Dir(d.path)
		if idx, ok := dirIndex[parent]; ok {
			if d.newest.After(dirs[idx].newest) {
				dirs[idx].newest = d.newest
			}
		}
	}

	changed := false
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		if now.Sub(f.mod) < scratchLifetime {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return counts, err
		}
		counts.RemovedFiles++
		counts.RemovedBytes += f.size
		changed = true
	}
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		if d.path == scratch {
			continue
		}
		if now.Sub(d.newest) < scratchLifetime {
			continue
		}
		// Remove only when empty; fresh survivors keep their parent alive
		// because Remove fails on non-empty directories.
		if err := os.Remove(d.path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// Non-empty (fresh child, skipped special file) is not an error.
			continue
		}
		counts.RemovedDirs++
		changed = true
	}
	if changed {
		if d, err := os.Open(scratch); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return counts, nil
}

// Worker sweeps one profile scratch directory immediately and then hourly
// until stopped. It mirrors the imagecache worker lifecycle.
type Worker struct {
	root   string
	log    *config.Logger
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewWorker binds a sweeper to a profile root.
func NewWorker(root string, log *config.Logger) *Worker {
	return &Worker{root: root, log: log.Server("workspace")}
}

// Start begins an independent worker lifetime; Stop must join it.
func (w *Worker) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})
	go w.run(ctx, time.Hour)
}

func (w *Worker) run(ctx context.Context, interval time.Duration) {
	defer close(w.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		started := time.Now()
		counts, err := Sweep(ctx, w.root, started)
		status := "ok"
		outcome := "complete"
		if err != nil {
			status = "error"
			if errors.Is(err, context.Canceled) {
				status = "ok"
				outcome = "canceled"
			} else {
				w.log.Warn("workspace.scratch.sweep.failed", "scratch sweep failed", config.ErrorField(err), config.F("status", "error"))
			}
		}
		w.log.Debug("workspace.scratch.sweep.complete", "scratch sweep completed",
			config.F("status", status), config.F("outcome", outcome),
			config.F("removed_file_count", counts.RemovedFiles), config.F("removed_dir_count", counts.RemovedDirs),
			config.F("removed_bytes", counts.RemovedBytes),
			config.F("duration_ms", time.Since(started).Milliseconds()))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Stop cancels and joins any running sweep.
func (w *Worker) Stop() {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
