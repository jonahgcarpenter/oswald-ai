package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
)

// cacheChildrenTTL matches the image-cache TTL: every cache subdirectory
// is pruned 24h after creation. Scratch is the exception by system-prompt
// contract (idle/mtime) and keeps its own hook below.
const cacheChildrenTTL = 24 * time.Hour

// SweepHook registers the scratch sweep on the shared per-profile cleanup
// worker. Scratch keeps its idle/mtime contract; the worker only schedules.
// Log events are unchanged from the former dedicated worker.
func SweepHook(root string, log *config.Logger) prune.Hook {
	return prune.Hook{
		Name:            "scratch",
		FailedEvent:     "workspace.scratch.sweep.failed",
		FailedMessage:   "scratch sweep failed",
		CompleteEvent:   "workspace.scratch.sweep.complete",
		CompleteMessage: "scratch sweep completed",
		Log:             log.Server("workspace"),
		Sweep: func(ctx context.Context, now time.Time) (prune.Counts, error) {
			counts, err := Sweep(ctx, root, now)
			return prune.Counts{RemovedFiles: counts.RemovedFiles, RemovedDirs: counts.RemovedDirs, RemovedBytes: counts.RemovedBytes}, err
		},
	}
}

// CacheChildrenHook prunes every cache subdirectory not owned by a
// dedicated hook (scratch, images, processes, ...). Each child gets a
// plain 24h creation-time sweep; unknown future subdirectories are covered
// without new wiring. Managed-cache denials elsewhere already ensure only
// tools create these entries.
func CacheChildrenHook(root string, log *config.Logger, skip map[string]struct{}) prune.Hook {
	return prune.Hook{
		Name:            "cache-children",
		FailedEvent:     "workspace.cache.sweep.failed",
		FailedMessage:   "cache sweep failed",
		CompleteEvent:   "workspace.cache.sweep.complete",
		CompleteMessage: "cache sweep completed",
		Log:             log.Server("workspace"),
		Sweep: func(ctx context.Context, now time.Time) (prune.Counts, error) {
			var total prune.Counts
			if err := validateRoot(root); err != nil {
				return total, err
			}
			entries, err := os.ReadDir(filepath.Join(root, "cache"))
			if errors.Is(err, os.ErrNotExist) {
				return total, nil
			}
			if err != nil {
				return total, err
			}
			for _, e := range entries {
				if err := ctx.Err(); err != nil {
					return total, err
				}
				full := filepath.Join(root, "cache", e.Name())
				linfo, err := os.Lstat(full)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return total, err
				}
				if !linfo.IsDir() || linfo.Mode()&os.ModeSymlink != 0 {
					continue
				}
				if _, owned := skip[e.Name()]; owned {
					continue
				}
				counts, err := prune.Sweep(ctx, prune.Spec{
					Name:  "cache-children",
					Dir:   full,
					TTL:   cacheChildrenTTL,
					Clock: prune.ClockCreation,
				}, now)
				total.RemovedFiles += counts.RemovedFiles
				total.RemovedDirs += counts.RemovedDirs
				total.RemovedBytes += counts.RemovedBytes
				if err != nil {
					return total, err
				}
			}
			return total, nil
		},
	}
}
