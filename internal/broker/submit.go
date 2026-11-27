package broker

import (
	"context"
	"errors"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	toolclarify "github.com/jonahgcarpenter/oswald-ai/internal/tools/clarify"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
	"strings"
	"time"
)

// CancelReport describes foreground agent work affected by a cancellation request.

func (b *Broker) Submit(req *Request) error {
	if req.ResponseChan == nil {
		req.ResponseChan = make(chan Result, 1)
	}
	if req.Usage == nil {
		req.Usage = requestctx.NewUsageCollector()
	}
	if handled, err := b.trySteer(req); handled {
		return err
	}
	return b.submitEnqueue(req)
}

// trySteer attaches req to the running head of its lane when eligible.
// It reports whether the request was consumed as a steer (accepted or
// rejected with an immediate terminal result) plus the submission error to
// return: shutdown and queue-full rejections return ErrShuttingDown and
// ErrQueueFull so gateways mark them rejected instead of completed.

func (b *Broker) submitEnqueue(req *Request) error {
	w := b.buildAgentWork(req)
	b.log.Debug("broker.request.queued", "queued broker request",
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
		b.log.Warn("broker.request.rejected", "rejected broker request",
			config.F("request_id", req.RequestID), config.F("gateway", req.Principal.Gateway),
			config.F("profile", req.Principal.CanonicalUserID),
			config.F("chat_id", req.ChatID), config.F("status", "rejected"), config.F("reason", reason))
		deliverResult(req.ResponseChan, Result{Response: &agent.Response{Response: config.SafeText(text)}, ExecutionComplete: true})
		return err
	}
	return nil
}

// buildAgentWork creates lane work for one agent request with its own steer inbox.

func (b *Broker) trySteer(req *Request) (bool, error) {
	if req.ForceQueue || req.Stateless {
		return false, nil
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return false, nil
	}
	key := laneKey(req.Principal, req.SessionKey)
	b.mu.Lock()
	if !b.accepting {
		b.mu.Unlock()
		reason, text := "shutting_down", DrainNoticeText
		b.log.Warn("broker.request.rejected", "rejected broker request",
			config.F("request_id", req.RequestID), config.F("gateway", req.Principal.Gateway),
			config.F("profile", req.Principal.CanonicalUserID),
			config.F("chat_id", req.ChatID), config.F("status", "rejected"), config.F("reason", reason))
		deliverResult(req.ResponseChan, Result{Response: &agent.Response{Response: config.SafeText(text)}, ExecutionComplete: true})
		return true, ErrShuttingDown
	}
	lane := b.lanes[key]
	if len(lane) == 0 {
		b.mu.Unlock()
		return false, nil
	}
	head := lane[0]
	if head.state != workRunning || head.request == nil || head.request.Stateless || head.steerInbox == nil {
		b.mu.Unlock()
		return false, nil
	}
	limit := requestQueueSize + b.workerCount
	if b.outstanding+b.steerCount >= limit {
		b.mu.Unlock()
		reason, text := "queue_full", "request rejected: broker queue full"
		b.log.Warn("broker.request.rejected", "rejected broker request",
			config.F("request_id", req.RequestID), config.F("gateway", req.Principal.Gateway),
			config.F("profile", req.Principal.CanonicalUserID),
			config.F("chat_id", req.ChatID), config.F("status", "rejected"), config.F("reason", reason))
		deliverResult(req.ResponseChan, Result{Response: &agent.Response{Response: config.SafeText(text)}, ExecutionComplete: true})
		return true, ErrQueueFull
	}
	if head.steerEntries == nil {
		head.steerEntries = make(map[string]*steerEntry)
	}
	queuedAt := time.Now()
	head.steerInbox.Add(strings.TrimSpace(req.Prompt), req.RequestID, req.Images)
	head.steerEntries[req.RequestID] = &steerEntry{req: req, queuedAt: queuedAt}
	head.steerOrder = append(head.steerOrder, req.RequestID)
	b.steerCount++
	if b.background != nil {
		b.background.cancel()
	}
	b.mu.Unlock()
	transport := req.Ctx
	if transport == nil {
		transport = b.lifecycleCtx
	}
	if err := transport.Err(); err != nil {
		b.removeSteer(key, head, req.RequestID, context.Cause(transport))
		return true, nil
	}
	stop := context.AfterFunc(transport, func() {
		b.removeSteer(key, head, req.RequestID, context.Cause(transport))
	})
	b.mu.Lock()
	if entry, ok := head.steerEntries[req.RequestID]; ok {
		entry.stopWatch = stop
	} else {
		b.mu.Unlock()
		stop()
		return true, nil
	}
	b.mu.Unlock()
	b.log.Debug("broker.request.steered", "steered follow-up into running turn",
		config.F("request_id", req.RequestID), config.F("gateway", req.Principal.Gateway),
		config.F("chat_id", req.ChatID), config.F("session_id", req.SessionKey))
	return true, nil
}

// removeSteer drops one pending steer after transport cancellation.

func (b *Broker) buildAgentWork(req *Request) *work {
	requestCtx, cancel := context.WithCancelCause(b.lifecycleCtx)
	meta := req.Metadata
	meta.RequestID = req.RequestID
	if meta.Workload == "" {
		meta.Workload = "foreground"
	}
	requestCtx = requestctx.WithMetadata(requestCtx, meta)
	requestCtx = requestctx.WithPrincipal(requestCtx, req.Principal)
	if req.EscalationNotify != nil {
		requestCtx = workspace.WithEscalationNotify(requestCtx, req.EscalationNotify)
	}
	if req.ClarifyAsk != nil {
		requestCtx = toolclarify.WithNotifier(requestCtx, req.ClarifyAsk)
	}
	requestCtx = requestctx.WithUsageCollector(requestCtx, req.Usage)
	inbox := agent.NewSteerInbox()
	w := &work{key: laneKey(req.Principal, req.SessionKey), request: req, ctx: requestCtx, cancel: cancel, ownerUserID: req.Principal.CanonicalUserID, steerInbox: inbox, steerEntries: make(map[string]*steerEntry)}
	// Transport cancellation cancels this request's work without touching
	// the conversation lane or queued prompts. The watch is released on first delivery.
	transport := req.Ctx
	if transport == nil {
		transport = b.lifecycleCtx
	}
	stopWatch := context.AfterFunc(transport, func() {
		cancel(context.Cause(transport))
	})
	if err := transport.Err(); err != nil {
		cancel(context.Cause(transport))
	}
	deliver := func(result Result) {
		b.mu.Lock()
		if !w.queuedAt.IsZero() {
			if w.processStartedAt.IsZero() {
				result.QueueWaitMS = time.Since(w.queuedAt).Milliseconds()
			} else {
				result.QueueWaitMS = w.processStartedAt.Sub(w.queuedAt).Milliseconds()
				result.AgentDurationMS = time.Since(w.processStartedAt).Milliseconds()
			}
		}
		b.mu.Unlock()
		w.deliverOnce.Do(func() { stopWatch(); deliverResult(req.ResponseChan, result) })
	}
	w.run = func() error {
		if cause := context.Cause(w.ctx); cause != nil {
			deliver(Result{Err: cause, ExecutionComplete: true})
			b.settleSteers(w, nil, cause)
			return nil
		}
		if cause := context.Cause(w.ctx); cause != nil {
			deliver(Result{Principal: req.Principal, Err: cause, ExecutionComplete: true})
			b.settleSteers(w, nil, cause)
			return nil
		}
		b.mu.Lock()
		w.processStartedAt = time.Now()
		b.mu.Unlock()
		resp, err := b.agent.Process(requestctx.WithPrincipal(w.ctx, req.Principal), agent.Request{
			RequestID: req.RequestID, Principal: req.Principal, DisplayName: req.DisplayName,
			SessionKey: req.SessionKey, Prompt: req.Prompt, Stateless: req.Stateless, SkipMemory: req.SkipMemory, ModelOverride: req.ModelOverride, EnabledToolsets: req.EnabledToolsets, MCPServers: req.MCPServers, ClientHistory: req.ClientHistory, ClientTools: req.ClientTools, ClientToolChoice: req.ClientToolChoice, ClientParallelToolCalls: req.ClientParallelToolCalls, ClientGeneration: req.ClientGeneration, ServiceTier: req.ServiceTier, Images: req.Images, Documents: req.Documents, StreamFunc: req.StreamFunc,
			IsDirect: req.IsDirect, ChatLabel: req.ChatLabel, PlatformMessageID: req.PlatformMessageID, ChatDisplayName: req.ChatDisplayName, ChatID: req.ChatID, TransportProfile: req.TransportProfile, Steer: inbox,
		})
		b.logExecutionComplete(requestctx.WithPrincipal(w.ctx, req.Principal), req.Usage, resp, err)
		if cause := context.Cause(w.ctx); cause != nil {
			err = cause
			resp = nil
		}
		deliver(Result{Response: resp, Principal: req.Principal, Err: err, ExecutionComplete: true})
		b.settleSteers(w, resp, err)
		return nil
	}
	w.finish = func(err error) {
		if err != nil {
			b.mu.Lock()
			complete := !w.workerActive || w.executionFinished
			b.mu.Unlock()
			deliver(Result{Err: err, ExecutionComplete: complete})
			b.settleSteers(w, nil, err)
		}
	}
	return w
}

// settleSteers fans out consumed follow-ups and requeues unconsumed ones.
// Consumed entries share the host response so no dead model call fires for
// them. Unconsumed entries (and every steer when the host fails) auto-run as
// the next turns in arrival order.
