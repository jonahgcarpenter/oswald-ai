package imagecache

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Worker sweeps expired image files immediately and then hourly until stopped.
type Worker struct {
	cache  *Cache
	log    *config.Logger
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewWorker constructs a cache maintenance worker.
func NewWorker(cache *Cache, log *config.Logger) *Worker {
	return &Worker{cache: cache, log: log.Server("imagecache")}
}

// Start begins an independent worker lifetime; Stop must join it before shutdown.
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
		counts, err := w.cache.Sweep(ctx, started)
		status := "ok"
		outcome := "complete"
		if err != nil {
			status = "error"
			if errors.Is(err, context.Canceled) {
				status = "ok"
				outcome = "canceled"
			} else {
				w.log.Warn("imagecache.sweep.failed", "image cache sweep failed", config.ErrorField(err), config.F("status", "error"))
			}
		}
		w.log.Info("imagecache.sweep.complete", "image cache sweep completed",
			config.F("record_kind", "measurement"), config.F("status", status), config.F("outcome", outcome),
			config.F("removed_file_count", counts.RemovedFiles), config.F("removed_bytes", counts.RemovedBytes),
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
