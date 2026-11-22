package docache

import (
	"context"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
)

// SweepHook registers the cache sweep on the shared per-profile cleanup
// worker. Documents and motion media register separate hooks.
func SweepHook(c *Cache, log *config.Logger) prune.Hook {
	name := "documents"
	if c != nil && c.kind == kindMotion {
		name = "motion"
	}
	return prune.Hook{
		Name:            name,
		FailedEvent:     "doccache.sweep.failed",
		FailedMessage:   "document cache sweep failed",
		CompleteEvent:   "doccache.sweep.complete",
		CompleteMessage: "document cache sweep completed",
		Log:             log.Server("docache"),
		Sweep: func(ctx context.Context, now time.Time) (prune.Counts, error) {
			counts, err := c.Sweep(ctx, now)
			return prune.Counts{RemovedFiles: counts.RemovedFiles, RemovedBytes: counts.RemovedBytes}, err
		},
	}
}
