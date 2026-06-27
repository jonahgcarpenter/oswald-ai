package broker

import (
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
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
