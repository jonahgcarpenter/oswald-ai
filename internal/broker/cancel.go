package broker

import (
	"fmt"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"strings"
)

// CancelReport describes foreground agent work affected by a cancellation request.

type CancelReport struct {
	ActiveSignaled int
	QueuedCanceled int
}

// LaneKey identifies work that must execute serially and in acceptance order.

func (b *Broker) CancelActiveAgentWork(canonicalUserID, sessionID string) CancelReport {
	canonicalUserID = strings.TrimSpace(canonicalUserID)
	sessionID = strings.TrimSpace(sessionID)
	if canonicalUserID == "" || sessionID == "" {
		return CancelReport{}
	}
	return b.cancelAgentWork(func(w *work) bool {
		return w.state == workRunning && w.ownerUserID == canonicalUserID && w.key.SessionID == sessionID
	}, false)
}

// CancelAllAgentWork cancels all active and queued foreground agent requests,
// including steered follow-ups that have not yet been consumed.

func (b *Broker) CancelAllAgentWork() CancelReport {
	return b.cancelAgentWork(func(w *work) bool {
		return w.state == workRunning || w.state == workQueued
	}, true)
}

func (b *Broker) cancelAgentWork(matches func(*work) bool, failSteers bool) CancelReport {
	b.mu.Lock()
	var report CancelReport
	var canceled []*work
	var failedSteers []*steerEntry
	for _, lane := range b.lanes {
		for _, w := range lane {
			if w.request == nil || !matches(w) {
				continue
			}
			if w.state == workRunning {
				report.ActiveSignaled++
			} else {
				report.QueuedCanceled++
			}
			w.state = workCanceled
			w.cancel(ErrAgentWorkCanceled)
			b.untrackInflightLocked(w.request)
			canceled = append(canceled, w)
			if failSteers && w.steerInbox != nil && len(w.steerEntries) > 0 {
				for id, entry := range w.steerEntries {
					failedSteers = append(failedSteers, entry)
					delete(w.steerEntries, id)
				}
				w.steerOrder = nil
				w.steerInbox.TakeAll()
				// Correct double counting: only add steers beyond works.
				// (Works already counted above; steers are extra.)
				// Recompute simply: steers add len(failedSteers) total.
				// Undo the запутанный adjustment by recounting below.
				_ = report
			}
		}
	}
	if len(failedSteers) > 0 {
		report.QueuedCanceled += len(failedSteers)
		if b.steerCount >= len(failedSteers) {
			b.steerCount -= len(failedSteers)
		} else {
			b.steerCount = 0
		}
	}
	b.mu.Unlock()
	for _, w := range canceled {
		w.finish(ErrAgentWorkCanceled)
	}
	for _, entry := range failedSteers {
		if entry == nil || entry.req == nil {
			continue
		}
		if entry.stopWatch != nil {
			entry.stopWatch()
		}
		deliverResult(entry.req.ResponseChan, Result{Principal: entry.req.Principal, Err: ErrAgentWorkCanceled, ExecutionComplete: true})
	}
	return report
}

// RunInLane runs a synchronous gateway operation in the same FIFO lane used by agent work.

func (b *Broker) rejectPendingWork() {
	b.mu.Lock()
	var pending []*work
	var failedSteers []*steerEntry
	for _, lane := range b.lanes {
		for _, w := range lane {
			pending = append(pending, w)
			if w.steerInbox != nil && len(w.steerEntries) > 0 {
				for id, entry := range w.steerEntries {
					failedSteers = append(failedSteers, entry)
					delete(w.steerEntries, id)
				}
				w.steerOrder = nil
				w.steerInbox.TakeAll()
			}
		}
	}
	b.lanes = make(map[LaneKey][]*work)
	b.outstanding = 0
	b.steerCount = 0
	b.inflight = make(map[inflightKey]int)
	b.mu.Unlock()
	for _, w := range pending {
		b.cancelFenceReservations(w)
		w.finish(ErrShuttingDown)
		b.workWG.Done()
	}
	for _, entry := range failedSteers {
		if entry == nil || entry.req == nil {
			continue
		}
		if entry.stopWatch != nil {
			entry.stopWatch()
		}
		deliverResult(entry.req.ResponseChan, Result{Principal: entry.req.Principal, Err: ErrShuttingDown, ExecutionComplete: true})
	}
}

func (b *Broker) runWorker(id int) {
	defer b.workerWG.Done()
	b.log.Debug("broker.worker.started", "broker worker started", config.F("worker_id", id))
	for w := range b.ready {
		b.mu.Lock()
		w.workerActive = true
		canceled := w.state == workCanceled
		if !canceled {
			w.state = workRunning
		}
		b.mu.Unlock()
		if w.request != nil {
			b.log.Debug("broker.worker.processing", "broker worker processing request",
				config.F("worker_id", id), config.F("request_id", w.request.RequestID),
				config.F("gateway", w.request.Principal.Gateway), config.F("chat_id", w.request.ChatID))
		}
		var err error
		if canceled {
			err = ErrAgentWorkCanceled
		} else {
			err = safeRun(w.run)
		}
		b.mu.Lock()
		w.executionFinished = true
		b.mu.Unlock()
		w.finish(err)
		if w.gated {
			for i := len(w.fences) - 1; i >= 0; i-- {
				if i >= len(w.releasedFences) || !w.releasedFences[i] {
					w.fences[i].release(w.exclusive)
				}
			}
		}
		b.complete(w)
	}
	b.log.Debug("broker.worker.stopped", "broker worker stopped", config.F("worker_id", id))
}

func (b *Broker) complete(completed *work) {
	b.mu.Lock()
	completed.state = workCompleted
	lane := b.lanes[completed.key]
	if len(lane) > 0 {
		lane = lane[1:]
	}
	b.outstanding--
	b.untrackInflightLocked(completed.request)
	var next *work
	if len(lane) == 0 {
		delete(b.lanes, completed.key)
	} else {
		b.lanes[completed.key] = lane
		next = lane[0]
	}
	b.mu.Unlock()
	b.workWG.Done()
	if next != nil {
		b.makeReady(next)
	}
}

func safeRun(run func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("broker operation panicked: %v", recovered)
		}
	}()
	return run()
}

func deliverResult(ch chan Result, result Result) (delivered bool) {
	defer func() {
		if recover() != nil {
			delivered = false
		}
	}()
	select {
	case ch <- result:
		return true
	default:
		return false
	}
}
