package cron

import (
	"context"
	"sync"
	"time"
)

// Ticker advances one profile store on an interval and dispatches due jobs.
// It owns no goroutines beyond its loop and joins before shutdown. A Wake
// re-arms the scan without waiting out the interval (for creates/updates);
// ticks are idempotent, so a redundant wake is harmless.
type Ticker struct {
	store      *Store
	runner     *Runner
	dispatch   *Dispatcher
	interval   time.Duration
	wake       chan struct{}
	stop       chan struct{}
	wg         sync.WaitGroup
	once       sync.Once
	wakeMu     sync.Mutex
	wakeClosed bool
}

// NewTicker creates a sequential per-profile ticker.
func NewTicker(store *Store, runner *Runner, interval time.Duration) *Ticker {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Ticker{store: store, runner: runner, interval: interval, wake: make(chan struct{}, 1), stop: make(chan struct{})}
}

// SetDispatcher installs the live dispatcher used for due jobs.
func (t *Ticker) SetDispatcher(dispatch *Dispatcher) {
	t.dispatch = dispatch
}

// Wake triggers an out-of-band scan. It never blocks.
func (t *Ticker) Wake() {
	t.wakeMu.Lock()
	defer t.wakeMu.Unlock()
	if t.wakeClosed {
		return
	}
	select {
	case t.wake <- struct{}{}:
	default:
	}
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
				t.scan(now)
				timer.Reset(t.interval)
			case <-t.wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				t.scan(time.Now())
				timer.Reset(t.interval)
			}
		}
	}()
}

func (t *Ticker) scan(now time.Time) {
	if t.store == nil || t.dispatch == nil {
		return
	}
	due := t.store.ClaimDue(now)
	if len(due) == 0 {
		return
	}
	t.dispatch.RunDue(context.Background(), due)
}

// Stop joins the loop.
func (t *Ticker) Stop() {
	t.once.Do(func() { close(t.stop) })
	t.wg.Wait()
	t.wakeMu.Lock()
	t.wakeClosed = true
	t.wakeMu.Unlock()
}
