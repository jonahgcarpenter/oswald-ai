package broker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestSubmitRejectsWhenQueueFull(t *testing.T) {
	b := NewBroker(nil, 0, config.NewLogger(config.LevelError))
	defer b.Shutdown()

	for i := 0; i < requestQueueSize+b.workerCount; i++ {
		req := &Request{RequestID: "queued", ResponseChan: make(chan Result, 1)}
		b.Submit(req)
		select {
		case result := <-req.ResponseChan:
			t.Fatalf("queued request %d received unexpected immediate result: %+v", i, result)
		default:
		}
	}

	rejected := &Request{RequestID: "rejected", ResponseChan: make(chan Result, 1)}
	b.Submit(rejected)

	select {
	case result := <-rejected.ResponseChan:
		if result.Response == nil || result.Response.Response != "request rejected: broker queue full" {
			t.Fatalf("rejected result = %+v, want queue-full fallback", result)
		}
	default:
		t.Fatal("full queue did not reject immediately")
	}
}

func TestNewBrokerUsesAtLeastOneWorker(t *testing.T) {
	b := NewBroker(&captureProcessor{requests: make(chan agent.Request, 1)}, 0, config.NewLogger(config.LevelError))
	if b.workerCount != 1 {
		t.Fatalf("workerCount = %d, want 1", b.workerCount)
	}
}

func TestDeliverResultDoesNotBlockOrPanic(t *testing.T) {
	full := make(chan Result, 1)
	full <- Result{}
	if deliverResult(full, Result{}) {
		t.Fatal("delivery to full channel succeeded")
	}

	closed := make(chan Result, 1)
	close(closed)
	if deliverResult(closed, Result{}) {
		t.Fatal("delivery to closed channel succeeded")
	}
}

func TestWorkerForwardsPrincipalToProcessor(t *testing.T) {
	processor := &captureProcessor{requests: make(chan agent.Request, 1)}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()

	principal := identity.Principal{CanonicalUserID: "usr_1", Gateway: "websocket", ExternalID: "alice", Assurance: identity.AssuranceSelfAsserted}
	req := &Request{
		RequestID:    "req-1",
		Principal:    principal,
		DisplayName:  "Alice",
		SessionKey:   "websocket:alice",
		Prompt:       "hello",
		ResponseChan: make(chan Result, 1),
	}
	b.Submit(req)

	select {
	case result := <-req.ResponseChan:
		if result.Err != nil || result.Response == nil || result.Response.Response != "ok" {
			t.Fatalf("unexpected result: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broker result")
	}

	select {
	case got := <-processor.requests:
		if got.Principal != principal || got.RequestID != req.RequestID || got.SessionKey != req.SessionKey || got.DisplayName != req.DisplayName || got.Prompt != req.Prompt {
			t.Fatalf("forwarded request = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for processor request")
	}
}

func TestBrokerSerializesSameLaneFIFO(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "websocket", ExternalID: "user", Assurance: identity.AssuranceSelfAsserted}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		done <- b.RunInLane(context.Background(), principal, "session", func() error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
	}()
	<-firstStarted
	go func() {
		done <- b.RunInLane(context.Background(), principal, "session", func() error {
			close(secondStarted)
			return nil
		})
	}()
	select {
	case <-secondStarted:
		t.Fatal("second same-lane operation started before first completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second same-lane operation did not start")
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestBrokerRunsDifferentLanesInParallel(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "websocket", ExternalID: "user", Assurance: identity.AssuranceSelfAsserted}
	started := make(chan string, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, sessionID := range []string{"one", "two"} {
		wg.Add(1)
		go func(sessionID string) {
			defer wg.Done()
			_ = b.RunInLane(context.Background(), principal, sessionID, func() error {
				started <- sessionID
				<-release
				return nil
			})
		}(sessionID)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("different lanes did not run concurrently")
		}
	}
	close(release)
	wg.Wait()
}

func TestBrokerHotLaneDoesNotOccupyOtherWorker(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "websocket", ExternalID: "user", Assurance: identity.AssuranceSelfAsserted}
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), principal, "hot", func() error { close(firstStarted); <-release; return nil })
	}()
	<-firstStarted
	secondHotStarted := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), principal, "hot", func() error { close(secondHotStarted); return nil })
	}()
	otherStarted := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), principal, "other", func() error { close(otherStarted); return nil })
	}()
	select {
	case <-otherStarted:
	case <-time.After(time.Second):
		t.Fatal("hot-lane follower occupied worker needed by another lane")
	}
	select {
	case <-secondHotStarted:
		t.Fatal("hot-lane follower started early")
	default:
	}
	close(release)
}

func TestBrokerSameSessionDifferentUsersRunInParallel(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	started := make(chan string, 2)
	release := make(chan struct{})
	for _, userID := range []string{"one", "two"} {
		principal := identity.Principal{CanonicalUserID: userID, Gateway: "websocket", ExternalID: userID, Assurance: identity.AssuranceSelfAsserted}
		go func() {
			_ = b.RunInLane(context.Background(), principal, "shared", func() error { started <- userID; <-release; return nil })
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("different users with shared session ID did not run concurrently")
		}
	}
	close(release)
}

func TestBrokerShutdownDrainsLaneFollowers(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "websocket", ExternalID: "user", Assurance: identity.AssuranceSelfAsserted}
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan string, 2)
	go func() {
		_ = b.RunInLane(context.Background(), principal, "session", func() error { close(firstStarted); <-release; completed <- "first"; return nil })
	}()
	<-firstStarted
	go func() {
		_ = b.RunInLane(context.Background(), principal, "session", func() error { completed <- "second"; return nil })
	}()
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		outstanding := b.outstanding
		b.mu.Unlock()
		if outstanding == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second lane operation was not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	shutdownDone := make(chan struct{})
	go func() { b.Shutdown(); close(shutdownDone) }()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before accepted lane work drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain lane followers")
	}
	if first, second := <-completed, <-completed; first != "first" || second != "second" {
		t.Fatalf("completion order=%q,%q", first, second)
	}
}

func TestBrokerRejectsAfterShutdown(t *testing.T) {
	b := NewBroker(nil, 1, config.NewLogger(config.LevelError))
	b.Start()
	b.Shutdown()
	req := &Request{RequestID: "late", ResponseChan: make(chan Result, 1)}
	if err := b.Submit(req); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("late submit error=%v", err)
	}
	select {
	case result := <-req.ResponseChan:
		if result.Response == nil || result.Response.Response != "request rejected: broker shutting down" {
			t.Fatalf("late result=%+v", result)
		}
	default:
		t.Fatal("late submit did not receive immediate result")
	}
}

type captureProcessor struct {
	requests chan agent.Request
}

func (p *captureProcessor) Process(req agent.Request) (*agent.AgentResponse, error) {
	p.requests <- req
	return &agent.AgentResponse{Response: "ok"}, nil
}
