package cron

import (
	"context"
	"sync"
	"time"
)

// Ticker advances one profile store on an interval. It owns no goroutines
// beyond its loop and joins before shutdown.
type Ticker struct {
	store    *Store
	runner   *Runner
	interval time.Duration
	stop     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
}

// NewTicker creates a sequential per-profile ticker.
func NewTicker(store *Store, runner *Runner, interval time.Duration) *Ticker {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Ticker{store: store, runner: runner, interval: interval, stop: make(chan struct{})}
}

// Start begins the loop; safe to call once.
func (t *Ticker) Start() {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		timer := time.NewTimer(t.interval)
		defer timer.Stop()
		for {
			select {
			case <-t.stop:
				return
			case now := <-timer.C:
				if t.store != nil {
					_ = t.store.Tick(context.Background(), t.runner, now, 4)
				}
				timer.Reset(t.interval)
			}
		}
	}()
}

// Stop joins the loop.
func (t *Ticker) Stop() {
	t.once.Do(func() { close(t.stop) })
	t.wg.Wait()
}
