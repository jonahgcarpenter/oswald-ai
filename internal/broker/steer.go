package broker

import (
	"context"
	"errors"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"time"
)

// CancelReport describes foreground agent work affected by a cancellation request.

type steerEntry struct {
	req       *Request
	queuedAt  time.Time
	stopWatch func() bool
}

func (b *Broker) removeSteer(key LaneKey, head *work, requestID string, cause error) {
	if head == nil || head.steerInbox == nil {
		return
	}
	if _, ok := head.steerInbox.Remove(requestID); !ok {
		return
	}
	b.mu.Lock()
	entry, ok := head.steerEntries[requestID]
	if ok {
		delete(head.steerEntries, requestID)
		if b.steerCount > 0 {
			b.steerCount--
		}
	}
	b.mu.Unlock()
	if !ok {
		return
	}
	if entry.stopWatch != nil {
		entry.stopWatch()
	}
	if cause == nil {
		cause = context.Canceled
	}
	deliverResult(entry.req.ResponseChan, Result{Principal: entry.req.Principal, Err: cause, ExecutionComplete: true})
}

// submitEnqueue builds agent lane work and enqueues it as a FIFO follower.

func (b *Broker) settleSteers(host *work, resp *agent.Response, hostErr error) {
	if host == nil || host.steerInbox == nil {
		return
	}
	drained := host.steerInbox.Drained()
	pending := host.steerInbox.Pending()
	if len(drained) == 0 && len(pending) == 0 {
		return
	}
	drainedSet := make(map[string]struct{}, len(drained))
	for _, st := range drained {
		drainedSet[st.RequestID] = struct{}{}
	}
	b.mu.Lock()
	order := append([]string(nil), host.steerOrder...)
	entries := make(map[string]*steerEntry, len(host.steerEntries))
	for id, entry := range host.steerEntries {
		entries[id] = entry
	}
	host.steerEntries = make(map[string]*steerEntry)
	host.steerOrder = nil
	// Clear the inbox so late arrivals after settlement become normal followers.
	host.steerInbox.TakeAll()
	failed := hostErr != nil
	b.mu.Unlock()
	consume := func(id string) {
		b.mu.Lock()
		entry, ok := entries[id]
		if ok {
			delete(entries, id)
			if b.steerCount > 0 {
				b.steerCount--
			}
		}
		b.mu.Unlock()
		if !ok || entry == nil {
			return
		}
		if entry.stopWatch != nil {
			entry.stopWatch()
		}
		if failed {
			b.requeueSteer(host.key, entry)
			return
		}
		duration := time.Since(entry.queuedAt).Milliseconds()
		b.log.Debug("broker.request.steered", "completed steered follow-up with host turn",
			config.F("request_id", entry.req.RequestID),
			config.F("gateway", entry.req.Principal.Gateway),
			config.F("chat_id", entry.req.ChatID))
		deliverResult(entry.req.ResponseChan, Result{Response: resp, Principal: entry.req.Principal, QueueWaitMS: 0, AgentDurationMS: duration, ExecutionComplete: true, SteeredShared: true})
	}
	requeue := func(id string) {
		b.mu.Lock()
		entry, ok := entries[id]
		if ok {
			delete(entries, id)
			if b.steerCount > 0 {
				b.steerCount--
			}
		}
		b.mu.Unlock()
		if !ok || entry == nil {
			return
		}
		if entry.stopWatch != nil {
			entry.stopWatch()
		}
		b.requeueSteer(host.key, entry)
	}
	if failed {
		for _, id := range order {
			if _, ok := entries[id]; !ok {
				continue
			}
			requeue(id)
		}
		return
	}
	for _, st := range drained {
		consume(st.RequestID)
	}
	for _, st := range pending {
		if _, ok := drainedSet[st.RequestID]; ok {
			continue
		}
		requeue(st.RequestID)
	}
	// Entries removed by transport cancellation are already gone.
	for id := range entries {
		requeue(id)
	}
}

// requeueSteer converts one unconsumed steer into a FIFO lane follower that
// preserves the original arrival time for queue-wait telemetry.

func (b *Broker) requeueSteer(key LaneKey, entry *steerEntry) {
	if entry == nil || entry.req == nil {
		return
	}
	req := entry.req
	req.ForceQueue = true
	w := b.buildAgentWork(req)
	w.queuedAt = entry.queuedAt
	if w.queuedAt.IsZero() {
		w.queuedAt = time.Now()
	}
	b.log.Debug("broker.request.queued", "queued unconsumed steer as next turn",
		config.F("request_id", req.RequestID), config.F("gateway", req.Principal.Gateway),
		config.F("chat_id", req.ChatID), config.F("session_id", req.SessionKey))
	if err := b.enqueue(w); err != nil {
		w.cancel(err)
		reason := "queue_full"
		text := "request rejected: broker queue full"
		if errors.Is(err, ErrShuttingDown) {
			reason = "shutting_down"
			text = DrainNoticeText
		}
		b.log.Warn("broker.request.rejected", "rejected requeued steer",
			config.F("request_id", req.RequestID), config.F("gateway", req.Principal.Gateway),
			config.F("profile", req.Principal.CanonicalUserID),
			config.F("chat_id", req.ChatID), config.F("status", "rejected"), config.F("reason", reason))
		deliverResult(req.ResponseChan, Result{Response: &agent.Response{Response: config.SafeText(text)}, ExecutionComplete: true})
	}
}

// CancelActiveAgentWork cancels the currently running foreground request in one conversation.
// Steered follow-ups survive as queued next turns via host settlement.
