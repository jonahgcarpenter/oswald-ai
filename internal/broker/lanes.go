package broker

import (
	"context"
	"errors"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	toolclarify "github.com/jonahgcarpenter/oswald-ai/internal/tools/clarify"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
	"strings"
	"sync"
	"time"
)

const requestQueueSize = 10

// DrainNoticeText is the user-facing reply for work rejected while the
// server is draining for a restart. It names no internals: the reader only
// needs to know to retry shortly.
const DrainNoticeText = "Oswald is restarting, please retry in a couple of seconds"

// restartDrainTimeout bounds one graceful drain: in-flight turns finish,
// then the server exits even if a turn is still running.
const restartDrainTimeout = 5 * time.Minute

var (
	// ErrQueueFull indicates that the broker has reached its global work limit.
	ErrQueueFull = errors.New("broker queue full")
	// ErrShuttingDown indicates that the broker no longer accepts new work.
	ErrShuttingDown = errors.New("broker is shutting down")
	// ErrAgentWorkCanceled indicates that a stop command canceled foreground agent work.
	ErrAgentWorkCanceled = errors.New("agent work canceled")
)

// CancelReport describes foreground agent work affected by a cancellation request.

type LaneKey struct {
	CanonicalUserID string
	SessionID       string
	UserExclusive   bool
}

// inflightKey identifies one admitted turn by the chat and the transport
// message that invoked it. The platform message ID rides every gateway
// request but is otherwise only logged and persisted; the broker indexes it
// here so group-thread replies can prove they steer a live turn instead of
// trusting BlueBubbles isFromMe authorship.

type inflightKey struct {
	chatID            string
	platformMessageID string
}

// Request carries a single user request from a gateway into the broker.

type Request struct {
	Usage       *requestctx.UsageCollector
	Metadata    requestctx.Metadata
	RequestID   string
	ChatID      string
	Principal   identity.Principal
	DisplayName string
	SessionKey  string
	IsDirect    bool
	ChatLabel   string
	Prompt      string
	// ForceQueue forces lane-queueing even when steering would otherwise
	// attach to the running turn. Set by /queue; plain messages steer.
	ForceQueue bool
	// PlatformMessageID is the untrusted inbound transport message identifier.
	PlatformMessageID string
	// ChatDisplayName is the untrusted plain conversation name.
	ChatDisplayName string
	// TransportProfile names the profile owning the receiving adapter.
	TransportProfile string
	Stateless        bool
	ClientHistory    []llm.ChatMessage
	// SkipMemory mirrors agent.Request.SkipMemory for scheduled turns.
	SkipMemory bool
	// EnabledToolsets mirrors agent.Request.EnabledToolsets. Nil disables
	// filtering; an empty list hides every builtin. The MCP bridge is
	// governed independently by MCPServers.
	EnabledToolsets []string
	// MCPServers mirrors agent.Request.MCPServers. Nil exposes every profile
	// MCP server through the tool_search bridge; an empty list exposes none.
	// Passed through verbatim so nil and empty stay distinct.
	MCPServers       []string
	ClientTools      []llm.Tool
	ClientToolChoice agent.ClientToolChoice
	// ClientParallelToolCalls carries the caller's explicit
	// parallel_tool_calls preference through to the agent.
	ClientParallelToolCalls *bool
	// ClientGeneration carries the caller's sampling and stop parameters
	// through to the agent for stateless requests.
	ClientGeneration *agent.ClientGeneration
	// ServiceTier carries the caller's validated service_tier value through
	// to the agent for stateless requests.
	ServiceTier string
	// Ctx carries transport cancellation to the worker. Nil keeps the
	// previous lifecycle-only behavior.
	Ctx        context.Context
	Images     []llm.InputImage
	Documents  []media.DocumentRef
	StreamFunc func(agent.StreamChunk)
	// EscalationNotify reports a parked approval ask while the tool call
	// blocks. It is installed in the request context when set.
	EscalationNotify workspace.EscalationNotifier
	// ClarifyAsk reports a parked clarify ask while the tool call blocks.
	// It is installed in the request context when set.
	ClarifyAsk toolclarify.Notifier
	// ModelOverride pins the turn to an explicit provider/model pair (cron
	// jobs). Nil runs the router chain. Present but unresolvable fails.
	ModelOverride *agent.ModelOverride
	ResponseChan  chan Result
}

// Result is the response payload delivered to the originating gateway.

type Result struct {
	// ExecutionComplete is false for immediate cancellation of still-running work.
	ExecutionComplete bool
	QueueWaitMS       int64
	AgentDurationMS   int64
	// SteeredShared marks a consumed steer fanned out with the host response.
	// The host turn already delivers this response, so the steered transport
	// must suppress its own send to avoid duplicate delivery.
	SteeredShared bool
	Response      *agent.Response
	Principal     identity.Principal
	Err           error
}

type work struct {
	queuedAt          time.Time
	workerActive      bool
	processStartedAt  time.Time
	executionFinished bool
	key               LaneKey
	run               func() error
	finish            func(error)
	request           *Request
	fences            []*userFence
	reservedReaders   []bool
	releasedFences    []bool
	exclusive         bool
	gated             bool
	ctx               context.Context
	cancel            context.CancelCauseFunc
	ownerUserID       string
	state             workState
	deliverOnce       sync.Once
	// steerInbox collects follow-ups steered into this running turn.
	// steerEntries maps steered request IDs to their original requests in
	// arrival order (steerOrder) so consumed turns fan out and unconsumed
	// turns auto-run as lane followers.
	steerInbox   *agent.SteerInbox
	steerEntries map[string]*steerEntry
	steerOrder   []string
}

type workState uint8

const (
	workQueued workState = iota
	workRunning
	workCanceled
	workCompleted
)

type Broker struct {
	agent       Processor
	workerCount int
	ready       chan *work
	log         *config.Logger

	mu              sync.Mutex
	lanes           map[LaneKey][]*work
	userFences      map[string]*userFence
	inflight        map[inflightKey]int
	outstanding     int
	steerCount      int
	accepting       bool
	started         bool
	background      *backgroundPermit
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	workWG    sync.WaitGroup
	workerWG  sync.WaitGroup
	startOnce sync.Once
	shutdown  sync.Once
}

type Processor interface {
	Process(context.Context, agent.Request) (*agent.Response, error)
}

// NewBroker creates a lane-aware broker. Call Start before production use.

func NewBroker(aiAgent Processor, workerCount int, log *config.Logger) *Broker {
	if workerCount <= 0 {
		workerCount = 1
	}
	capacity := requestQueueSize + workerCount
	if capacity < requestQueueSize {
		capacity = requestQueueSize
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	return &Broker{
		agent:           aiAgent,
		workerCount:     workerCount,
		ready:           make(chan *work, capacity),
		log:             log,
		lanes:           make(map[LaneKey][]*work),
		userFences:      make(map[string]*userFence),
		inflight:        make(map[inflightKey]int),
		accepting:       true,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
}

// IsInflight reports whether the broker currently holds an admitted turn for
// the given chat that was invoked by the given platform message ID. Empty
// IDs never match. Gateways use this to admit threaded follow-ups as steers
// of a live turn without trusting transport authorship flags.

func (b *Broker) IsInflight(chatID, platformMessageID string) bool {
	chatID = strings.TrimSpace(chatID)
	platformMessageID = strings.TrimSpace(platformMessageID)
	if chatID == "" || platformMessageID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inflight[inflightKey{chatID: chatID, platformMessageID: platformMessageID}] > 0
}

// trackInflightLocked registers one admitted request. Callers must hold b.mu
// and only call after the work has been accepted into a lane.

func (b *Broker) trackInflightLocked(req *Request) {
	if req == nil {
		return
	}
	chatID := strings.TrimSpace(req.ChatID)
	platformMessageID := strings.TrimSpace(req.PlatformMessageID)
	if chatID == "" || platformMessageID == "" {
		return
	}
	if b.inflight == nil {
		b.inflight = make(map[inflightKey]int)
	}
	b.inflight[inflightKey{chatID: chatID, platformMessageID: platformMessageID}]++
}

// untrackInflightLocked releases one admitted request. Callers must hold b.mu.

func (b *Broker) untrackInflightLocked(req *Request) {
	if req == nil || len(b.inflight) == 0 {
		return
	}
	chatID := strings.TrimSpace(req.ChatID)
	platformMessageID := strings.TrimSpace(req.PlatformMessageID)
	if chatID == "" || platformMessageID == "" {
		return
	}
	key := inflightKey{chatID: chatID, platformMessageID: platformMessageID}
	count, ok := b.inflight[key]
	if !ok {
		return
	}
	if count <= 1 {
		delete(b.inflight, key)
		return
	}
	b.inflight[key] = count - 1
}

// Start launches the worker pool once.

func (b *Broker) Start() {
	b.startOnce.Do(func() {
		b.mu.Lock()
		b.started = true
		var heads []*work
		for _, lane := range b.lanes {
			if len(lane) > 0 {
				heads = append(heads, lane[0])
			}
		}
		b.mu.Unlock()
		for i := 0; i < b.workerCount; i++ {
			b.workerWG.Add(1)
			go b.runWorker(i + 1)
		}
		for _, head := range heads {
			b.makeReady(head)
		}
		b.log.Debug("broker.started", "started broker worker pool", config.F("worker_count", b.workerCount))
	})
}

// Submit enqueues an agent request. A plain message to a busy lane steers
// into the running turn; /queue sets ForceQueue to wait for the next turn.
// Rejected requests receive one immediate result.

func (b *Broker) enqueue(w *work) error {
	b.mu.Lock()
	if !b.accepting {
		b.mu.Unlock()
		return ErrShuttingDown
	}
	limit := requestQueueSize + b.workerCount
	if b.outstanding+b.steerCount >= limit {
		b.mu.Unlock()
		return ErrQueueFull
	}
	if b.background != nil {
		b.background.cancel()
	}
	lane := b.lanes[w.key]
	if len(w.fences) == 0 {
		fence := b.userFences[w.key.CanonicalUserID]
		if fence == nil {
			fence = newUserFence()
			b.userFences[w.key.CanonicalUserID] = fence
		}
		w.fences = []*userFence{fence}
		if w.exclusive {
			fence.reserveWriter()
		} else {
			w.reservedReaders = []bool{fence.reserveReader()}
		}
	}
	b.lanes[w.key] = append(lane, w)
	if w.queuedAt.IsZero() {
		w.queuedAt = time.Now()
	}
	b.outstanding++
	b.trackInflightLocked(w.request)
	b.workWG.Add(1)
	isHead := len(lane) == 0
	if isHead {
		if b.started {
			b.makeReady(w)
		}
	}
	b.mu.Unlock()
	return nil
}

// TryAcquireLowPriority reserves model capacity only while no foreground work
// is active or queued. Accepted foreground work cancels the returned context.

func (b *Broker) makeReady(w *work) {
	go func() {
		for i, fence := range w.fences {
			reservedReader := i < len(w.reservedReaders) && w.reservedReaders[i]
			fence.acquire(w.exclusive, reservedReader)
		}
		w.gated = true
		b.ready <- w
	}()
}

func (b *Broker) RunInLane(ctx context.Context, principal identity.Principal, sessionID string, operation func() error) error {
	return b.runOperationForUsers(ctx, LaneKey{CanonicalUserID: principal.CanonicalUserID, SessionID: sessionID}, []string{principal.CanonicalUserID}, operation)
}

// RunUsersExclusive waits for active work for every listed user and blocks new
// work for those users until completion. IDs are deduplicated and locked in a
// stable order so overlapping multi-user operations cannot deadlock.

func (b *Broker) LaneBusy(principal identity.Principal, sessionID string) bool {
	if b == nil {
		return false
	}
	key := laneKey(principal, sessionID)
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.lanes[key]) > 0
}

func laneKey(principal identity.Principal, sessionID string) LaneKey {
	return LaneKey{CanonicalUserID: principal.CanonicalUserID, SessionID: sessionID}
}

// LaneBusy reports whether the lane for one user session currently holds
// queued or running work. Background delivery consults it to avoid
// snapshotting stale persisted history while a foreground turn is still
// active; busy lanes defer delivery to the next sweep.

func (b *Broker) BeginDrain() {
	b.mu.Lock()
	if !b.accepting {
		b.mu.Unlock()
		return
	}
	b.accepting = false
	if b.background != nil {
		b.background.cancel()
	}
	queued := b.outstanding + b.steerCount
	started := b.started
	b.mu.Unlock()
	b.log.Debug("broker.drain.started", "draining broker for restart", config.F("queued_request_count", queued), config.F("workers_started", started))
}

// Draining reports whether the broker has stopped admission for a restart.

func (b *Broker) Draining() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.accepting
}

// WaitIdle blocks until all admitted work completes or the timeout elapses,
// reporting whether the broker went idle. It never cancels work itself:
// callers fall through to Shutdown to cancel leftovers on timeout.

func (b *Broker) WaitIdle(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		b.workWG.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		b.log.Debug("broker.drain.complete", "broker drain complete")
		return true
	case <-timer.C:
		b.log.Warn("broker.drain.timeout", "broker drain timed out with work still running", config.F("status", "degraded"))
		return false
	}
}

// Shutdown rejects new work, cancels active agent requests, drains accepted lanes, and stops workers.

func (b *Broker) Shutdown() {
	b.shutdown.Do(func() {
		b.mu.Lock()
		b.accepting = false
		b.lifecycleCancel()
		if b.background != nil {
			b.background.cancel()
		}
		queued := b.outstanding + b.steerCount
		started := b.started
		b.mu.Unlock()
		b.log.Debug("broker.shutdown.start", "shutting down broker", config.F("queued_request_count", queued))
		if started && b.workerCount > 0 {
			b.workWG.Wait()
		} else {
			b.rejectPendingWork()
		}
		close(b.ready)
		b.workerWG.Wait()
		b.log.Debug("broker.shutdown.complete", "broker shutdown complete")
	})
}
