package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
)

// drainBlockProcessor holds one turn open until released, reporting when the
// turn starts. It mirrors the cancelProcessor shape without cancellation.
type drainBlockProcessor struct {
	started chan struct{}
	release chan struct{}
}

func (p *drainBlockProcessor) Process(ctx context.Context, _ agent.Request) (*agent.Response, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return &agent.Response{Response: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func drainPrincipal() identity.Principal {
	return identity.Principal{CanonicalUserID: "user", Gateway: "imessage", ExternalID: "user", Assurance: identity.AssuranceBlueBubblesWebhook}
}

func TestBeginDrainRejectsNewWork(t *testing.T) {
	b := NewBroker(nil, 1, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	if b.Draining() {
		t.Fatal("fresh broker reports draining")
	}
	b.BeginDrain()
	if !b.Draining() {
		t.Fatal("broker does not report draining after BeginDrain")
	}
	b.BeginDrain() // idempotent: no panic, still draining.
	req := &Request{RequestID: "drained", Principal: drainPrincipal(), SessionKey: "session", Prompt: "hello", ResponseChan: make(chan Result, 1)}
	if err := b.Submit(req); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("drain submit = %v, want ErrShuttingDown", err)
	}
	select {
	case result := <-req.ResponseChan:
		if result.Response == nil || result.Response.Response != DrainNoticeText {
			t.Fatalf("drain result = %+v, want the retry notice", result)
		}
		if !result.ExecutionComplete {
			t.Fatal("drain result is not execution-complete")
		}
	default:
		t.Fatal("drain rejection delivered no result")
	}
	// Steers into a (nonexistent) running turn are rejected the same way.
	steer := &Request{RequestID: "steer", Principal: drainPrincipal(), SessionKey: "session", Prompt: "follow-up", ResponseChan: make(chan Result, 1)}
	if err := b.Submit(steer); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("drain steer submit = %v, want ErrShuttingDown", err)
	}
}

func TestDrainLetsInflightFinish(t *testing.T) {
	processor := &drainBlockProcessor{started: make(chan struct{}, 1), release: make(chan struct{})}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	req := &Request{RequestID: "active", Principal: drainPrincipal(), SessionKey: "session", Prompt: "hello", ResponseChan: make(chan Result, 1)}
	if err := b.Submit(req); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never started")
	}
	b.BeginDrain()
	if err := b.Submit(&Request{RequestID: "late", Principal: drainPrincipal(), SessionKey: "other", Prompt: "hello", ResponseChan: make(chan Result, 1)}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("late submit during drain = %v, want ErrShuttingDown", err)
	}
	close(processor.release)
	if !b.WaitIdle(5 * time.Second) {
		t.Fatal("broker did not go idle after the turn finished")
	}
	select {
	case result := <-req.ResponseChan:
		if result.Err != nil || result.Response == nil || result.Response.Response != "done" {
			t.Fatalf("turn result = %+v, want the completed answer", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("finished turn delivered no result")
	}
}

func TestWaitIdleTimesOutOnStuckTurn(t *testing.T) {
	processor := &drainBlockProcessor{started: make(chan struct{}, 1), release: make(chan struct{})}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	req := &Request{RequestID: "stuck", Principal: drainPrincipal(), SessionKey: "session", Prompt: "hello", ResponseChan: make(chan Result, 1)}
	if err := b.Submit(req); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never started")
	}
	b.BeginDrain()
	if b.WaitIdle(20 * time.Millisecond) {
		t.Fatal("WaitIdle reported idle with a turn still running")
	}
	// Shutdown cancels the stuck turn; the wait must resolve, not hang.
	close(processor.release)
	if !b.WaitIdle(5 * time.Second) {
		t.Fatal("broker did not go idle after release")
	}
}
