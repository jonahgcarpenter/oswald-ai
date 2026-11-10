package delegate

import (
	"context"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// alivePID reports whether a process id still runs. EPERM means alive but
// uninspectable; anything else (including ESRCH and invalid pids) is dead.
func alivePID(pid int64) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(int(pid), 0)
	return err == nil || err == syscall.EPERM
}

func nowUnix() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// Start recovers orphaned rows, then runs the delivery, stall, and prune
// loops until Stop. Calling Start twice without an intervening Stop is a
// no-op guard: the second call returns immediately.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	if m.pool == nil {
		m.pool = make(chan struct{}, m.maxConcurrent())
	}
	m.mu.Unlock()
	m.recover(runCtx)
	m.wg.Add(3)
	go m.deliverySweep(runCtx)
	go m.stallSweep(runCtx)
	go m.pruneLoop(runCtx)
}

// Stop cancels the loops and abandons live children in place. Their rows
// stay running and unclaimed, so the next Start reaps them as unknown:
// work that died with the process is never silently retried.
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.stopped = true
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

// deliverySweep periodically offers terminal-but-undelivered rows to the
// broker. A missed tick only delays handoff; rows persist.
func (m *Manager) deliverySweep(ctx context.Context) {
	defer m.wg.Done()
	m.deliverDue(ctx)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.deliverDue(ctx)
		}
	}
}

// stallSweep watches live children for wedged work and force-finalizes them
// after the grace window, so a stuck subagent can never hold its session
// hostage silently.
func (m *Manager) stallSweep(ctx context.Context) {
	defer m.wg.Done()
	ticker := time.NewTicker(stallCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkStalls(ctx)
		}
	}
}

// checkStalls interrupts children with no progress and force-finalizes
// those that outlive the grace window.
func (m *Manager) checkStalls(ctx context.Context) {
	now := time.Now()
	m.mu.Lock()
	var idle, toolStalled []*liveChild
	for _, child := range m.live {
		if child.done || child.forceFinal {
			continue
		}
		last := time.Unix(0, atomic.LoadInt64(&child.activity.lastEvent))
		inTool := atomic.LoadInt64(&child.activity.inTool) != 0
		quietFor := now.Sub(last)
		threshold := stalledIdleAfter
		if inTool {
			threshold = stalledInToolAfter
		}
		if quietFor < threshold {
			continue
		}
		if child.graceUntil.IsZero() {
			idle = append(idle, child)
			continue
		}
		if now.After(child.graceUntil) {
			toolStalled = append(toolStalled, child)
		}
	}
	m.mu.Unlock()
	for _, child := range idle {
		child.cancel()
		child.mu.Lock()
		if child.graceUntil.IsZero() {
			child.graceUntil = now.Add(stalledGrace)
		}
		child.mu.Unlock()
		m.log.Warn("delegate.stall.interrupted", "interrupting stalled subagent", config.F("delegation_id", child.id), config.F("status", "degraded"))
	}
	for _, child := range toolStalled {
		m.forceFinalize(ctx, child, now)
	}
}

// forceFinalize records a stalled child as terminal through the same
// single-winner path as normal completion: if the worker already recorded
// its outcome, this is a no-op. The delivery loop carries the verdict home.
func (m *Manager) forceFinalize(ctx context.Context, child *liveChild, now time.Time) {
	child.mu.Lock()
	if child.done || child.forceFinal {
		child.mu.Unlock()
		return
	}
	child.forceFinal = true
	child.mu.Unlock()
	last := time.Unix(0, atomic.LoadInt64(&child.activity.lastEvent))
	inTool := atomic.LoadInt64(&child.activity.inTool) != 0
	phase := "idle"
	if inTool {
		phase = "in_tool"
	}
	threshold := stalledIdleAfter
	if inTool {
		threshold = stalledInToolAfter
	}
	m.log.Warn("delegate.stall.force_finalized", "force-finalized stalled subagent", config.F("delegation_id", child.id), config.F("status", "degraded"))
	m.finishUnit(ctx, child.id, childResult{
		Status:  "stalled",
		Summary: "Subagent stopped making progress and was abandoned.",
		Timeout: &timeoutInfo{
			Seconds:      int64(threshold.Seconds()),
			Elapsed:      int64(now.Sub(child.started).Seconds()),
			LastEventAge: int64(now.Sub(last).Seconds()),
			Phase:        phase,
		},
		RouteModel:    child.routeModel,
		RouteProvider: child.routeProvider,
	})
}
