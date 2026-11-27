package startup

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway"
	"os"
)

// drainBlockAgent holds one turn open until released.
type drainBlockAgent struct {
	started chan struct{}
	release chan struct{}
}

func (a *drainBlockAgent) Process(ctx context.Context, _ agent.Request) (*agent.Response, error) {
	select {
	case a.started <- struct{}{}:
	default:
	}
	select {
	case <-a.release:
		return &agent.Response{Response: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func drainTestPrincipal() identity.Principal {
	return identity.Principal{CanonicalUserID: "user", Gateway: "imessage", ExternalID: "user", Assurance: identity.AssuranceBlueBubblesWebhook}
}

func TestAwaitShutdownParentCancelSkipsDrain(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	b := broker.NewBroker(nil, 1, log)
	b.Start()
	defer b.Shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sigCh := make(chan os.Signal, 4)
	done := make(chan struct{})
	go func() {
		awaitShutdown(ctx, log, b, nil, nil, sigCh)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitShutdown did not return on parent cancel")
	}
	if b.Draining() {
		t.Fatal("parent cancel must not start a drain")
	}
}

func TestAwaitShutdownDrainsOnSIGTERM(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	agent := &drainBlockAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	b := broker.NewBroker(agent, 1, log)
	b.Start()
	defer b.Shutdown()
	ticker := cron.NewTicker(nil, nil, time.Second)
	ticker.Start()
	req := &broker.Request{RequestID: "active", Principal: drainTestPrincipal(), SessionKey: "session", Prompt: "hello", ResponseChan: make(chan broker.Result, 1)}
	if err := b.Submit(req); err != nil {
		t.Fatal(err)
	}
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never started")
	}
	sigCh := make(chan os.Signal, 4)
	done := make(chan struct{})
	go func() {
		awaitShutdown(context.Background(), log, b, nil, []*cron.Ticker{ticker}, sigCh)
		close(done)
	}()
	sigCh <- syscall.SIGTERM
	// Drain starts: admission stops, but the running turn is untouched.
	deadline := time.Now().Add(2 * time.Second)
	for !b.Draining() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !b.Draining() {
		t.Fatal("broker is not draining after SIGTERM")
	}
	select {
	case <-done:
		t.Fatal("awaitShutdown returned while the turn was still running")
	case <-time.After(100 * time.Millisecond):
	}
	if err := b.Submit(&broker.Request{RequestID: "late", Principal: drainTestPrincipal(), SessionKey: "other", Prompt: "hello", ResponseChan: make(chan broker.Result, 1)}); err == nil {
		t.Fatal("late submit accepted during drain")
	}
	close(agent.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("awaitShutdown did not return after the turn finished")
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

func TestAwaitShutdownSecondSignalAbortsDrain(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	agent := &drainBlockAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	b := broker.NewBroker(agent, 1, log)
	b.Start()
	defer b.Shutdown()
	req := &broker.Request{RequestID: "stuck", Principal: drainTestPrincipal(), SessionKey: "session", Prompt: "hello", ResponseChan: make(chan broker.Result, 1)}
	if err := b.Submit(req); err != nil {
		t.Fatal(err)
	}
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never started")
	}
	sigCh := make(chan os.Signal, 4)
	done := make(chan struct{})
	go func() {
		awaitShutdown(context.Background(), log, b, nil, nil, sigCh)
		close(done)
	}()
	sigCh <- syscall.SIGTERM
	deadline := time.Now().Add(2 * time.Second)
	for !b.Draining() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !b.Draining() {
		t.Fatal("broker is not draining after SIGTERM")
	}
	sigCh <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second signal did not abort the drain")
	}
	if b.WaitIdle(20 * time.Millisecond) {
		t.Fatal("broker went idle: the stuck turn should still be running")
	}
	close(agent.release)
}

// drainBlockGateway is a gateway.Service with a blocking outbound drain.
type drainBlockGateway struct {
	waited  chan struct{}
	release chan struct{}
}

func (g *drainBlockGateway) Start(_ *broker.Broker) error { return nil }
func (g *drainBlockGateway) Name() string                 { return "drain-block" }
func (g *drainBlockGateway) WaitOutboundIdle(timeout time.Duration) bool {
	select {
	case g.waited <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
		return true
	case <-time.After(timeout):
		return false
	}
}

func TestAwaitShutdownWaitsForOutboundAfterBrokerIdle(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	b := broker.NewBroker(nil, 1, log)
	b.Start()
	defer b.Shutdown()
	outbound := &drainBlockGateway{waited: make(chan struct{}, 1), release: make(chan struct{})}
	sigCh := make(chan os.Signal, 4)
	done := make(chan struct{})
	go func() {
		awaitShutdown(context.Background(), log, b, []gateway.Service{outbound}, nil, sigCh)
		close(done)
	}()
	sigCh <- syscall.SIGTERM
	select {
	case <-outbound.waited:
	case <-time.After(2 * time.Second):
		t.Fatal("outbound drain never started after broker idle")
	}
	select {
	case <-done:
		t.Fatal("awaitShutdown returned while outbound delivery was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(outbound.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("awaitShutdown did not return after outbound drained")
	}
}

func TestAwaitShutdownSecondSignalAbortsOutboundWait(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	b := broker.NewBroker(nil, 1, log)
	b.Start()
	defer b.Shutdown()
	outbound := &drainBlockGateway{waited: make(chan struct{}, 1), release: make(chan struct{})}
	sigCh := make(chan os.Signal, 4)
	done := make(chan struct{})
	go func() {
		awaitShutdown(context.Background(), log, b, []gateway.Service{outbound}, nil, sigCh)
		close(done)
	}()
	sigCh <- syscall.SIGTERM
	select {
	case <-outbound.waited:
	case <-time.After(2 * time.Second):
		t.Fatal("outbound drain never started")
	}
	sigCh <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second signal did not abort the outbound wait")
	}
}
