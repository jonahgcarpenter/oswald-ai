package broker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

const requestQueueSize = 10

var (
	// ErrQueueFull indicates that the broker has reached its global work limit.
	ErrQueueFull = errors.New("broker queue full")
	// ErrShuttingDown indicates that the broker no longer accepts new work.
	ErrShuttingDown = errors.New("broker is shutting down")
)

// LaneKey identifies work that must execute serially and in acceptance order.
type LaneKey struct {
	CanonicalUserID string
	SessionID       string
}

// Request carries a single user request from a gateway into the broker.
type Request struct {
	RequestID    string
	ChatID       string
	Principal    identity.Principal
	DisplayName  string
	SessionKey   string
	IsDirect     bool
	Prompt       string
	Images       []llm.InputImage
	StreamFunc   func(agent.StreamChunk)
	ResponseChan chan Result
}

// Result is the response payload delivered to the originating gateway.
type Result struct {
	Response *agent.AgentResponse
	Err      error
}

type work struct {
	key     LaneKey
	run     func() error
	finish  func(error)
	request *Request
}

// Broker schedules FIFO work per canonical-user/session lane while allowing
// unrelated lanes to use the worker pool concurrently.
type Broker struct {
	agent       Processor
	workerCount int
	ready       chan *work
	log         *config.Logger

	mu          sync.Mutex
	lanes       map[LaneKey][]*work
	outstanding int
	accepting   bool
	started     bool

	workWG    sync.WaitGroup
	workerWG  sync.WaitGroup
	startOnce sync.Once
	shutdown  sync.Once
}

// Processor handles one typed agent request.
type Processor interface {
	Process(agent.Request) (*agent.AgentResponse, error)
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
	return &Broker{
		agent:       aiAgent,
		workerCount: workerCount,
		ready:       make(chan *work, capacity),
		log:         log,
		lanes:       make(map[LaneKey][]*work),
		accepting:   true,
	}
}

// Start launches the worker pool once.
func (b *Broker) Start() {
	b.startOnce.Do(func() {
		b.mu.Lock()
		b.started = true
		b.mu.Unlock()
		for i := 0; i < b.workerCount; i++ {
			b.workerWG.Add(1)
			go b.runWorker(i + 1)
		}
		b.log.Info("broker.started", "started broker worker pool", config.F("worker_count", b.workerCount))
	})
}

// Submit enqueues an agent request. Rejected requests receive one immediate result.
func (b *Broker) Submit(req *Request) error {
	if req.ResponseChan == nil {
		req.ResponseChan = make(chan Result, 1)
	}
	w := &work{
		key:     laneKey(req.Principal, req.SessionKey),
		request: req,
		run: func() error {
			resp, err := b.agent.Process(agent.Request{
				RequestID: req.RequestID, Principal: req.Principal, DisplayName: req.DisplayName,
				SessionKey: req.SessionKey, Prompt: req.Prompt, Images: req.Images, StreamFunc: req.StreamFunc,
				IsDirect: req.IsDirect,
			})
			deliverResult(req.ResponseChan, Result{Response: resp, Err: err})
			return nil
		},
		finish: func(err error) {
			if err != nil {
				deliverResult(req.ResponseChan, Result{Err: err})
			}
		},
	}
	b.log.Debug("broker.request.queued", "queued broker request",
		config.F("request_id", req.RequestID), config.F("gateway", req.Principal.Gateway),
		config.F("chat_id", req.ChatID), config.F("session_id", req.SessionKey))
	if err := b.enqueue(w); err != nil {
		reason := "queue_full"
		text := "request rejected: broker queue full"
		if errors.Is(err, ErrShuttingDown) {
			reason = "shutting_down"
			text = "request rejected: broker shutting down"
		}
		b.log.Warn("broker.request.rejected", "rejected broker request",
			config.F("request_id", req.RequestID), config.F("gateway", req.Principal.Gateway),
			config.F("chat_id", req.ChatID), config.F("status", "rejected"), config.F("reason", reason))
		deliverResult(req.ResponseChan, Result{Response: &agent.AgentResponse{Response: config.SafeText(text)}})
		return err
	}
	return nil
}

// RunInLane runs a synchronous gateway operation in the same FIFO lane used by agent work.
func (b *Broker) RunInLane(ctx context.Context, principal identity.Principal, sessionID string, operation func() error) error {
	if operation == nil {
		return fmt.Errorf("broker lane operation is required")
	}
	done := make(chan error, 1)
	w := &work{key: laneKey(principal, sessionID), run: operation, finish: func(err error) { done <- err }}
	if err := b.enqueue(w); err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Broker) enqueue(w *work) error {
	b.mu.Lock()
	if !b.accepting {
		b.mu.Unlock()
		return ErrShuttingDown
	}
	limit := requestQueueSize + b.workerCount
	if b.outstanding >= limit {
		b.mu.Unlock()
		return ErrQueueFull
	}
	lane := b.lanes[w.key]
	b.lanes[w.key] = append(lane, w)
	b.outstanding++
	b.workWG.Add(1)
	isHead := len(lane) == 0
	if isHead {
		b.ready <- w
	}
	b.mu.Unlock()
	return nil
}

// Shutdown rejects new work, drains every accepted lane, and stops workers.
func (b *Broker) Shutdown() {
	b.shutdown.Do(func() {
		b.mu.Lock()
		b.accepting = false
		queued := b.outstanding
		started := b.started
		b.mu.Unlock()
		b.log.Info("broker.shutdown.start", "shutting down broker", config.F("queued_request_count", queued))
		if started && b.workerCount > 0 {
			b.workWG.Wait()
		} else {
			b.rejectPendingWork()
		}
		close(b.ready)
		b.workerWG.Wait()
		b.log.Info("broker.shutdown.complete", "broker shutdown complete")
	})
}

func (b *Broker) rejectPendingWork() {
	b.mu.Lock()
	var pending []*work
	for _, lane := range b.lanes {
		pending = append(pending, lane...)
	}
	b.lanes = make(map[LaneKey][]*work)
	b.outstanding = 0
	b.mu.Unlock()
	for _, w := range pending {
		w.finish(ErrShuttingDown)
		b.workWG.Done()
	}
}

func (b *Broker) runWorker(id int) {
	defer b.workerWG.Done()
	b.log.Debug("broker.worker.started", "broker worker started", config.F("worker_id", id))
	for w := range b.ready {
		if w.request != nil {
			b.log.Debug("broker.worker.processing", "broker worker processing request",
				config.F("worker_id", id), config.F("request_id", w.request.RequestID),
				config.F("gateway", w.request.Principal.Gateway), config.F("chat_id", w.request.ChatID))
		}
		err := safeRun(w.run)
		w.finish(err)
		b.complete(w)
	}
	b.log.Debug("broker.worker.stopped", "broker worker stopped", config.F("worker_id", id))
}

func (b *Broker) complete(completed *work) {
	b.mu.Lock()
	lane := b.lanes[completed.key]
	if len(lane) > 0 {
		lane = lane[1:]
	}
	b.outstanding--
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
		b.ready <- next
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

func laneKey(principal identity.Principal, sessionID string) LaneKey {
	return LaneKey{CanonicalUserID: principal.CanonicalUserID, SessionID: sessionID}
}
