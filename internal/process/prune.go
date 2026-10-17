package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/prune"
)

// processTTL matches the cache contract: every cache subdirectory is
// pruned 24h after creation. Live and running records are always excluded.
const processTTL = 24 * time.Hour

// maxCompleted caps retained completed records so a spawn burst stays
// bounded even before the TTL expires.
const maxCompleted = 64

// PruneOld removes completed process records created at least ttl ago and
// trims retained completed records to maxCompleted, newest first. Live and
// running records are never touched; orphaned running records (no live
// owner) keep the existing killed-on-observation behavior via Load, not
// here. A missing base directory is a no-op so fresh profiles start clean.
func (m *Manager) PruneOld(ctx context.Context, now time.Time) (prune.Counts, error) {
	var counts prune.Counts
	base, err := m.baseDir()
	if err != nil {
		return counts, err
	}
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return counts, nil
	}
	if err != nil {
		return counts, err
	}
	type candidate struct {
		id  string
		age time.Time
	}
	var completed []candidate
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		dir := filepath.Join(base, e.Name())
		linfo, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return counts, err
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		m.mu.Lock()
		live, ok := m.running[e.Name()]
		running := ok && live.info.Status == StatusRunning
		m.mu.Unlock()
		if running {
			continue
		}
		info, err := readMeta(dir)
		if err != nil {
			continue
		}
		if info.Status == StatusRunning {
			continue
		}
		age := prune.AgeOf(dir, linfo, now, prune.ClockCreation)
		if now.Sub(age) >= processTTL {
			m.mu.Lock()
			delete(m.running, e.Name())
			m.mu.Unlock()
			if err := os.RemoveAll(dir); err != nil {
				return counts, err
			}
			counts.RemovedDirs++
			continue
		}
		completed = append(completed, candidate{id: e.Name(), age: age})
	}
	if len(completed) > maxCompleted {
		sort.Slice(completed, func(i, j int) bool { return completed[i].age.After(completed[j].age) })
		for _, c := range completed[:len(completed)-maxCompleted] {
			if err := ctx.Err(); err != nil {
				return counts, err
			}
			m.mu.Lock()
			delete(m.running, c.id)
			m.mu.Unlock()
			if err := os.RemoveAll(filepath.Join(base, c.id)); err != nil {
				return counts, err
			}
			counts.RemovedDirs++
		}
	}
	return counts, nil
}

// PruneHook registers process-record pruning on the shared per-profile
// cleanup worker, giving completed records their hourly periodic trigger
// (previously prune ran only opportunistically on Spawn/List).
func (m *Manager) PruneHook(log *config.Logger) prune.Hook {
	return prune.Hook{
		Name:            "processes",
		FailedEvent:     "process.sweep.failed",
		FailedMessage:   "process sweep failed",
		CompleteEvent:   "process.sweep.complete",
		CompleteMessage: "process sweep completed",
		Log:             log.Server("process"),
		Sweep: func(ctx context.Context, now time.Time) (prune.Counts, error) {
			return m.PruneOld(ctx, now)
		},
	}
}
