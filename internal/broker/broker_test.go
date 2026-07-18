package broker

import (
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestSubmitRejectsWhenQueueFull(t *testing.T) {
	b := NewBroker(nil, 0, config.NewLogger(config.LevelError))

	for i := 0; i < requestQueueSize; i++ {
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

type captureProcessor struct {
	requests chan agent.Request
}

func (p *captureProcessor) Process(req agent.Request) (*agent.AgentResponse, error) {
	p.requests <- req
	return &agent.AgentResponse{Response: "ok"}, nil
}
