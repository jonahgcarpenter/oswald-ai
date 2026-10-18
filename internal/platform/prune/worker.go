// Worker is the single per-profile cleanup scheduler. Tools register hooks
// with their own TTLs and clocks; the worker runs everything immediately on
// start and then hourly until stopped. One hook's failure never skips the
// rest. Per-hook logging keeps developer-owned event names with their
// owners: the worker only schedules and reports.
package prune

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// Hook is one registered cleanup sweep with owner-defined TTL semantics.
// Sweep judges its own ages (usually via AgeOf or a Spec); the worker
// supplies the tick and emits the owner's fixed log events around it.
type Hook struct {
	// Name identifies the hook in code only; never logged raw.
	Name string
	// FailedEvent/FailedMessage and CompleteEvent/CompleteMessage are the
	// owner's fixed diagnostics, preserved from the pre-unification
	// per-package workers.
	FailedEvent     string
	FailedMessage   string
	CompleteEvent   string
	CompleteMessage string
	Log             *config.Logger
	Sweep           func(ctx context.Context, now time.Time) (Counts, error)
}

// Worker runs registered hooks immediately and then hourly until stopped.
type Worker struct {
	mu     sync.Mutex
	hooks  []Hook
	cancel context.CancelFunc
	done   chan struct{}
}

// Add registers a hook. Hooks added after Start do not run until the next
// Start on a fresh worker; startup registers everything before Start.
func (w *Worker) Add(h Hook) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hooks = append(w.hooks, h)
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
		w.sweepAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) sweepAll(ctx context.Context) {
	w.mu.Lock()
	hooks := make([]Hook, len(w.hooks))
	copy(hooks, w.hooks)
	w.mu.Unlock()
	for _, h := range hooks {
		if h.Sweep == nil || h.Log == nil {
			continue
		}
		started := time.Now()
		counts, err := h.Sweep(ctx, started)
		status := "ok"
		outcome := "complete"
		if err != nil {
			status = "error"
			if errors.Is(err, context.Canceled) {
				status = "ok"
				outcome = "canceled"
			} else {
				h.Log.Warn(h.FailedEvent, h.FailedMessage, config.ErrorField(err), config.F("status", "error"))
			}
		}
		h.Log.Debug(h.CompleteEvent, h.CompleteMessage,
			config.F("status", status), config.F("outcome", outcome),
			config.F("removed_file_count", counts.RemovedFiles), config.F("removed_dir_count", counts.RemovedDirs),
			config.F("removed_bytes", counts.RemovedBytes),
			config.F("duration_ms", time.Since(started).Milliseconds()))
		if ctx.Err() != nil {
			return
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
