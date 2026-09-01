package websearch

import (
	"context"
	"sync"
	"time"
)

type attemptLimiter interface {
	Wait(context.Context) error
}

type rollingLimiter struct {
	mu         sync.Mutex
	limit      int
	window     time.Duration
	timestamps []time.Time
	now        func() time.Time
}

func newRollingLimiter(limit int, window time.Duration) *rollingLimiter {
	return &rollingLimiter{limit: limit, window: window, now: time.Now}
}

func (l *rollingLimiter) Wait(ctx context.Context) error {
	for {
		now := l.now()
		l.mu.Lock()
		cutoff := now.Add(-l.window)
		first := 0
		for first < len(l.timestamps) && !l.timestamps[first].After(cutoff) {
			first++
		}
		if first > 0 {
			l.timestamps = append(l.timestamps[:0], l.timestamps[first:]...)
		}
		if len(l.timestamps) < l.limit {
			l.timestamps = append(l.timestamps, now)
			l.mu.Unlock()
			return nil
		}
		wait := l.timestamps[0].Add(l.window).Sub(now)
		l.mu.Unlock()
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type noopLimiter struct{}

func (noopLimiter) Wait(context.Context) error { return nil }
