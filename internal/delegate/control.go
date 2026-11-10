package delegate

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// Control actions operate on the caller's own live subagents. Anything else —
// unknown ids, finished work, another session's children — reports the same
// unknown-subagent error so callers never learn about work that isn't theirs.
func (m *Manager) findLive(profile, session, id string) (*liveChild, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	child, ok := m.live[id]
	if !ok || child.done || child.origin.profile != profile || child.origin.sessionKey != session {
		return nil, false
	}
	return child, true
}

const unknownSubagent = "unknown subagent (it may have finished, or belong to another session)"

// unitActivity is the shared, atomically-updated progress record for one
// live child: the transcript writer advances it, the stall monitor and
// steer honesty checks read it.
type unitActivity struct {
	toolCalls    int64
	toolResults  int64
	lastEvent    int64
	inTool       int64
	startedAt    int64
	firstEventAt int64
}

func (m *Manager) controlList(_ context.Context, principalProfile, sessionKey string) (governance.Result, error) {
	children := m.liveForSession(principalProfile, sessionKey)
	if len(children) == 0 {
		return governance.Result{Content: "No live subagents in this session.", Outcome: governance.OutcomeUnproductive}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d live subagent(s):\n", len(children))
	now := time.Now()
	for _, child := range children {
		goal := child.goal
		if r := []rune(goal); len(r) > 80 {
			goal = string(r[:80]) + "…"
		}
		fmt.Fprintf(&sb, "- %s: %s (running %ds)\n", child.id, goal, int64(now.Sub(child.started).Seconds()))
	}
	return governance.Result{Content: strings.TrimSpace(sb.String()), Outcome: governance.OutcomeProductive}, nil
}

// controlSteer queues guidance into a running child at its next iteration
// boundary. Acceptance only means the text was queued before completion —
// the completion itself reports whether it landed.
func (m *Manager) controlSteer(_ context.Context, profile, session, id, message string) (governance.Result, error) {
	child, ok := m.findLive(profile, session, id)
	if !ok {
		return governance.Result{}, fmt.Errorf("delegate: %s", unknownSubagent)
	}
	id = strings.TrimSpace(id)
	child.steer.Add(message, id, nil)
	child.mu.Lock()
	child.steers = append(child.steers, acceptedSteer{text: message, at: time.Now(), toolCalls: atomic.LoadInt64(&child.activity.toolResults)})
	child.mu.Unlock()
	m.log.Debug("delegate.steer.queued", "queued subagent steer", config.F("delegation_id", id))
	return governance.Result{Content: "Steer queued for subagent " + id + ". It applies at the child's next iteration boundary.", Outcome: governance.OutcomeProductive}, nil
}

// controlStop ends a child at its next iteration boundary. The partial
// result still returns through the normal delivery path.
func (m *Manager) controlStop(_ context.Context, profile, session, id string) (governance.Result, error) {
	child, ok := m.findLive(profile, session, id)
	if !ok {
		return governance.Result{}, fmt.Errorf("delegate: %s", unknownSubagent)
	}
	child.cancel()
	m.log.Debug("delegate.stop.requested", "subagent stop requested", config.F("delegation_id", child.id))
	return governance.Result{Content: "Stopping subagent " + child.id + ". Its partial result will arrive as a normal completion.", Outcome: governance.OutcomeProductive}, nil
}

// CancelSession interrupts every live child owned by a session whose
// conversation is ending (/new) or stopped (/stop). Their partial results
// are dropped rather than delivered: the owning turn is gone, so there is
// nowhere to deliver to.
func (m *Manager) CancelSession(profile, sessionKey string) int {
	if m == nil || strings.TrimSpace(sessionKey) == "" {
		return 0
	}
	var stopped []*liveChild
	m.mu.Lock()
	for _, child := range m.live {
		if child.done || child.origin.profile != profile || child.origin.sessionKey != sessionKey {
			continue
		}
		stopped = append(stopped, child)
	}
	m.mu.Unlock()
	for _, child := range stopped {
		child.mu.Lock()
		child.drop = true
		child.mu.Unlock()
		child.cancel()
	}
	if n := len(stopped); n > 0 {
		m.log.Debug("delegate.session.cancelled", "cancelled session subagents on new session", config.F("profile", profile), config.F("count", int64(n)))
	}
	return len(stopped)
}
