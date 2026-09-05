package websearch

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRollingLimiterWaitsForWindow(t *testing.T) {
	limiter := newRollingLimiter(1, 20*time.Millisecond)
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 15*time.Millisecond {
		t.Fatal("second attempt was not rate limited")
	}
}

func TestRollingLimiterHonorsCancellation(t *testing.T) {
	limiter := newRollingLimiter(1, time.Second)
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := limiter.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v", err)
	}
}
