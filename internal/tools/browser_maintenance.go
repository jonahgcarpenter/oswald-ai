package tools

import (
	"context"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// BrowserSweepHook reaps idle browser sessions on the hourly profile prune
// tick. It is a no-op when the browser backend is unconfigured.
func BrowserSweepHook(reg *registry.Registry, log *config.Logger) prune.Hook {
	return prune.Hook{
		Name:            "browser-sessions",
		FailedEvent:     "tool.browser.sweep.failed",
		FailedMessage:   "browser session sweep failed",
		CompleteEvent:   "tool.browser.sweep.complete",
		CompleteMessage: "browser session sweep completed",
		Log:             log.Server("tool.browser"),
		Sweep: func(ctx context.Context, now time.Time) (prune.Counts, error) {
			return reg.Sweep(ctx, now)
		},
	}
}
