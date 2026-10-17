package imagecache

import (
	"context"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/prune"
)

// SweepHook registers the image cache sweep on the shared per-profile
// cleanup worker. TTL and creation clock stay owned here; the worker only
// schedules. Log events are unchanged from the former dedicated worker.
func SweepHook(c *Cache, log *config.Logger) prune.Hook {
	return prune.Hook{
		Name:            "images",
		FailedEvent:     "imagecache.sweep.failed",
		FailedMessage:   "image cache sweep failed",
		CompleteEvent:   "imagecache.sweep.complete",
		CompleteMessage: "image cache sweep completed",
		Log:             log.Server("imagecache"),
		Sweep: func(ctx context.Context, now time.Time) (prune.Counts, error) {
			counts, err := c.Sweep(ctx, now)
			return prune.Counts{RemovedFiles: counts.RemovedFiles, RemovedBytes: counts.RemovedBytes}, err
		},
	}
}
