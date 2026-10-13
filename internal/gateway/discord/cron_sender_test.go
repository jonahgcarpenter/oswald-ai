package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type cronRoundTripper func(*http.Request) (*http.Response, error)

func (f cronRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCronSenderGroupAndDM(t *testing.T) {
	var mu sync.Mutex
	var contents []string
	dg := &Gateway{Log: config.NewLogger(config.LevelError)}
	dg.HTTPClient = &http.Client{Transport: cronRoundTripper(func(r *http.Request) (*http.Response, error) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		contents = append(contents, payload["content"].(string))
		mu.Unlock()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"m1"}`))}, nil
	})}
	sender := dg.CronSender()
	// Group session carries its channel.
	if err := sender.SendText(context.Background(), "alice", "discord:chan9:user1", "hello group"); err != nil {
		t.Fatal(err)
	}
	// DM resolves through remembered inbound traffic.
	rememberCronChannel("discord:dm:user1", "dm-chan")
	if err := sender.SendText(context.Background(), "alice", "discord:dm:user1", "hello dm"); err != nil {
		t.Fatal(err)
	}
	// Long text chunks to Discord-sized messages.
	long := strings.Repeat("x", 2500)
	if err := sender.SendText(context.Background(), "alice", "discord:chan9:user1", long); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(contents) != 4 {
		t.Fatalf("messages = %d", len(contents))
	}
	// Unknown DM session fails closed with a remedy.
	if err := sender.SendText(context.Background(), "alice", "discord:dm:stranger", "hi"); err == nil {
		t.Fatal("unknown session sent")
	}
}
