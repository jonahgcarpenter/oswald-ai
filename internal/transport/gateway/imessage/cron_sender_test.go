package imessage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func TestCronSenderGroupAndDM(t *testing.T) {
	var mu sync.Mutex
	var payloads []string
	g := &Gateway{Log: config.NewLogger(config.LevelError), BlueBubblesURL: "http://bb", BlueBubblesPassword: "pw"}
	g.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		payloads = append(payloads, r.URL.Path)
		mu.Unlock()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"guid":"m1"}}`))}, nil
	})}
	sender := g.CronSender()
	// Group session carries its chat GUID.
	if err := sender.SendText(context.Background(), "alice", "imessage:chat-guid;+;group:user1", "hello group"); err != nil {
		t.Fatal(err)
	}
	// DM resolves through remembered inbound traffic.
	rememberCronChat("imessage:dm:user1", "dm-guid")
	if err := sender.SendText(context.Background(), "alice", "imessage:dm:user1", "hello dm"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 2 || payloads[0] != "/api/v1/message/text" || payloads[1] != "/api/v1/message/text" {
		t.Fatalf("payloads = %v", payloads)
	}
	// Unknown DM session fails closed with a remedy.
	if err := sender.SendText(context.Background(), "alice", "imessage:dm:stranger", "hi"); err == nil {
		t.Fatal("unknown session sent")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
