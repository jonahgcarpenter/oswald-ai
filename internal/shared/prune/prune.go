// Package prune owns shared cache-eviction mechanics: creation-time age,
// declarative directory/TTL sweeps, and the single per-profile cleanup
// worker that owner hooks register with.
//
// Owners keep their predicates: the worker only schedules. Each hook
// defines its own TTL and clock; generic subdirectories get a plain
// Spec{Dir, TTL, ClockCreation} sweep.
package prune

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// Clock selects which timestamp judges age.
type Clock int

const (
	// ClockMtime judges idle time since last modification. Scratch uses
	// this to honor the system-prompt "idle for 24h" contract.
	ClockMtime Clock = iota
	// ClockCreation judges absolute age since file creation (birthtime via
	// Statx, mtime fallback when the filesystem reports none).
	ClockCreation
)

// Spec points the shared logic at one directory with one TTL.
type Spec struct {
	// Name identifies the sweep in tests and diagnostics.
	Name string
	// Dir is swept but never removed itself.
	Dir string
	// TTL expiries entries at or beyond this age.
	TTL time.Duration
	// Clock selects creation or modification time.
	Clock Clock
	// Match filters regular files by basename; nil keeps all.
	Match func(base string) bool
}

// Counts describes successful sweep removals.
type Counts struct {
	RemovedFiles int
	RemovedDirs  int
	RemovedBytes int64
}

// birthtimeHook overrides Statx lookup in tests (birthtime is not settable
// via Chtimes). Nil means use the live filesystem.
var birthtimeHook func(path string) (time.Time, bool)

// SetBirthtimeHookForTest overrides birthtime lookup; pass nil to restore
// live Statx behavior. Tests only.
func SetBirthtimeHookForTest(fn func(path string) (time.Time, bool)) {
	birthtimeHook = fn
}

// birthtimeOf returns filesystem birthtime when valid: masked, non-zero,
// and not in the future.
func birthtimeOf(path string) (time.Time, bool) {
	if birthtimeHook != nil {
		return birthtimeHook(path)
	}
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME, &st); err != nil {
		return time.Time{}, false
	}
	if st.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false
	}
	t := time.Unix(st.Btime.Sec, int64(st.Btime.Nsec))
	return t, true
}

// AgeOf resolves the age timestamp for path: mtime directly, or birthtime
// when valid, falling back to mtime so the TTL stays strict instead of
// retaining entries the filesystem cannot date.
func AgeOf(path string, info os.FileInfo, now time.Time, clock Clock) time.Time {
	if clock != ClockCreation || info == nil {
		if info == nil {
			return now
		}
		return info.ModTime()
	}
	if t, ok := birthtimeOf(path); ok && !t.IsZero() && !t.After(now) {
		return t
	}
	return info.ModTime()
}

// CreationTimeAt resolves the creation timestamp for dirfd/name without a
// pathname lookup: birthtime via dirfd-relative Statx when valid, else the
// supplied fallback (typically fstat mtime).
func CreationTimeAt(dirfd int, name string, fallback, now time.Time) time.Time {
	if birthtimeHook != nil {
		if t, ok := birthtimeHook(name); ok && !t.IsZero() && !t.After(now) {
			return t
		}
		return fallback
	}
	var st unix.Statx_t
	if err := unix.Statx(dirfd, name, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &st); err != nil {
		return fallback
	}
	if st.Mask&unix.STATX_BTIME == 0 {
		return fallback
	}
	t := time.Unix(st.Btime.Sec, int64(st.Btime.Nsec))
	if t.IsZero() || t.After(now) {
		return fallback
	}
	return t
}

// Sweep removes entries under spec.Dir at or beyond spec.TTL. Files are
// judged by their own age; directories by the newest age in their subtree
// so an old parent with a fresh child survives. Only regular files are
// removed; symlinks and special files are skipped. A missing directory is
// a no-op. The spec root itself is never removed.
func Sweep(ctx context.Context, spec Spec, now time.Time) (Counts, error) {
	var counts Counts
	if spec.TTL <= 0 {
		return counts, errors.New("prune TTL must be positive")
	}
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	dir := filepath.Clean(spec.Dir)
	if dir == "" || dir == "." || dir == "/" {
		return counts, errors.New("prune directory is invalid")
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return counts, nil
	}
	if err != nil {
		return counts, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return counts, errors.New("prune directory is unsafe")
	}

	type fileEntry struct {
		path string
		size int64
		age  time.Time
	}
	type dirEntry struct {
		path   string
		depth  int
		newest time.Time
	}
	var files []fileEntry
	var dirs []dirEntry
	dirIndex := make(map[string]int)

	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
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
		rel, err := filepath.Rel(dir, path)
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
			dirs = append(dirs, dirEntry{path: path, depth: depth, newest: AgeOf(path, linfo, now, spec.Clock)})
			return nil
		}
		if !linfo.Mode().IsRegular() {
			return nil
		}
		if path == dir {
			return nil
		}
		if spec.Match != nil && !spec.Match(linfo.Name()) {
			return nil
		}
		files = append(files, fileEntry{path: path, size: linfo.Size(), age: AgeOf(path, linfo, now, spec.Clock)})
		// Propagate file age up to ancestor dirs for subtree-newest.
		ancestor := filepath.Dir(path)
		for {
			if idx, ok := dirIndex[ancestor]; ok {
				if files[len(files)-1].age.After(dirs[idx].newest) {
					dirs[idx].newest = files[len(files)-1].age
				}
			}
			if ancestor == dir {
				break
			}
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				break
			}
			ancestor = parent
		}
		return nil
	})
	if err != nil {
		return counts, err
	}
	// Bubble child-dir newness up so a nested fresh child protects ancestors.
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].depth > dirs[j].depth })
	for _, d := range dirs {
		if d.path == dir {
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
		if now.Sub(f.age) < spec.TTL {
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
		if d.path == dir {
			continue
		}
		if now.Sub(d.newest) < spec.TTL {
			continue
		}
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
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return counts, nil
}
