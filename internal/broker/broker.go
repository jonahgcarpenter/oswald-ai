package broker

import (
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

const (
	// requestQueueSize is the maximum number of requests that can be buffered
	// in the broker channel before Submit rejects new callers.
	requestQueueSize = 10
)

// Request carries a single user request from a gateway into the broker.
// The gateway populates routing metadata, the user prompt, and an optional
// stream callback for real-time token delivery. The broker writes exactly
// one Result to ResponseChan when processing completes.
type Request struct {
	RequestID    string                  // Per-prompt correlation identifier
	Channel      string                  // Gateway name (e.g., "discord", "websocket")
	ChatID       string                  // Conversation/room identifier
	SenderID     string                  // User identifier
	DisplayName  string                  // Human-readable display name for the sender (optional)
	SessionKey   string                  // Unique conversation context key
	Prompt       string                  // The user's message text
	Images       []ollama.InputImage     // Optional: current-turn image attachments for multimodal models
	StreamFunc   func(agent.StreamChunk) // Optional: streaming callback (nil for non-streaming gateways)
	ResponseChan chan Result             // Broker writes the final result here; must be buffered(1)
}

// Result is the response payload the broker delivers back to the originating
// gateway via Request.ResponseChan.
type Result struct {
	Response *agent.AgentResponse
	Err      error
}

// Broker sits between gateways and the agent. It owns a fixed-size worker pool
// that consumes Requests from a shared channel and routes responses back to the
// originating gateway via each request's ResponseChan.
//
// This decouples gateway transport logic from agent processing and provides
// concurrency control: at most workerCount requests are processed in parallel,
// with excess requests queued in the channel.
type Broker struct {
	agent       *agent.Agent
	requests    chan *Request
	workerCount int
	wg          sync.WaitGroup
	log         *config.Logger
}

// NewBroker creates a Broker with the given agent, fixed worker pool size,
// and logger. Call Start() to begin dispatching requests.
func NewBroker(aiAgent *agent.Agent, workerCount int, log *config.Logger) *Broker {
	return &Broker{
		agent:       aiAgent,
		requests:    make(chan *Request, requestQueueSize),
		workerCount: workerCount,
		log:         log,
	}
}

// Start launches the worker pool goroutines. Each worker processes requests
// from the shared channel until it is closed by Shutdown().
func (b *Broker) Start() {
	for i := range b.workerCount {
		b.wg.Add(1)
		go b.runWorker(i + 1)
	}
	b.log.Info("broker.started", "started broker worker pool", config.F("worker_count", b.workerCount))
}

// Submit enqueues a request for processing. If the internal queue is full
// (i.e., all workers are busy and requestQueueSize requests are already
// waiting), it immediately returns a hardcoded response to the caller instead of
// blocking. The caller must set req.ResponseChan to a buffered(1) channel before
// calling Submit; the broker will write exactly one result to it.
func (b *Broker) Submit(req *Request) {
	b.log.Debug("broker.request.queued", "queued broker request",
		config.F("request_id", req.RequestID),
		config.F("gateway", req.Channel),
		config.F("chat_id", req.ChatID),
		config.F("session_id", req.SessionKey),
	)

	select {
	case b.requests <- req:
	default:
		b.log.Warn("broker.request.rejected", "rejected broker request",
			config.F("request_id", req.RequestID),
			config.F("gateway", req.Channel),
			config.F("chat_id", req.ChatID),
			config.F("status", "rejected"),
			config.F("reason", "queue_full"),
		)
		req.ResponseChan <- Result{
			Response: &agent.AgentResponse{
				Response: "The queue is full, Try again later or help fragsap buy a new GPU to fix these issues.",
			},
		}
	}
}

// Shutdown closes the request channel, signalling all workers to stop after
// draining any queued requests, then waits for all in-flight Process() calls
// to complete before returning. New Submit() calls must not be made after
// Shutdown() is called.
func (b *Broker) Shutdown() {
	b.log.Info("broker.shutdown.start", "shutting down broker", config.F("queued_request_count", len(b.requests)))
	close(b.requests)
	b.wg.Wait()
	b.log.Info("broker.shutdown.complete", "broker shutdown complete")
}

// runWorker is the body of a single broker worker goroutine. It reads
// Requests from the shared channel, calls Agent.Process(), and delivers
// the result back to the gateway via the request's ResponseChan.
func (b *Broker) runWorker(id int) {
	defer b.wg.Done()
	b.log.Debug("broker.worker.started", "broker worker started", config.F("worker_id", id))

	for req := range b.requests {
		b.log.Debug("broker.worker.processing", "broker worker processing request",
			config.F("worker_id", id),
			config.F("request_id", req.RequestID),
			config.F("gateway", req.Channel),
			config.F("chat_id", req.ChatID),
		)

		resp, err := b.agent.Process(req.RequestID, req.Channel, req.SessionKey, req.SenderID, req.DisplayName, req.Prompt, req.Images, req.StreamFunc)

		req.ResponseChan <- Result{
			Response: resp,
			Err:      err,
		}
	}

	b.log.Debug("broker.worker.stopped", "broker worker stopped", config.F("worker_id", id))
}
