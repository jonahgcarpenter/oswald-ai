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
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

// TestCronSenderGroupAndDM covers every origin shape the cron dispatcher can
// route. Group sessions carry their chat GUID in the key; DM sessions resolve
// through remembered inbound traffic, falling back to the canonical
// BlueBubbles direct-chat GUID so delivery works with no inbound traffic since
// process start.
func TestCronSenderGroupAndDM(t *testing.T) {
	var mu sync.Mutex
	var guids []string
	g := &Gateway{Log: config.NewLogger(config.LevelError), BlueBubblesURL: "http://bb", BlueBubblesPassword: "pw"}
	g.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		guids = append(guids, r.URL.Path+" "+toString(payload["chatGuid"]))
		mu.Unlock()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"guid":"m1"}}`))}, nil
	})}
	sender := g.CronSender()

	// Group session carries its chat GUID in the key; no lookup is triggered.
	if err := sender.Deliver(context.Background(), "alice", "imessage:chat-guid;+;group:user1", "hello group", nil); err != nil {
		t.Fatal(err)
	}
	// DM resolves through remembered inbound traffic.
	rememberCronChat("imessage:dm:user1", "dm-guid")
	if err := sender.Deliver(context.Background(), "alice", "imessage:dm:user1", "hello dm", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(guids) != 2 || guids[0] != "/api/v1/message/text chat-guid;+;group" || guids[1] != "/api/v1/message/text dm-guid" {
		t.Fatalf("guids = %v", guids)
	}
	mu.Unlock()

	// Unknown DM session falls back to the canonical direct-chat GUID derived
	// from the peer in the session key, so a fresh process can still deliver.
	// The service segment is the concrete "iMessage" service, not "any":
	// BlueBubbles send endpoints reject the "any" service.
	if err := sender.Deliver(context.Background(), "alice", "imessage:dm:stranger", "hi", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(guids) != 3 || guids[2] != "/api/v1/message/text iMessage;-;stranger" {
		t.Fatalf("expected derived direct-chat GUID fallback, got %v", guids)
	}
	mu.Unlock()
}

// TestCronSenderUnrecognizedKeyFailsClosed guards that a session key that is
// neither a group key nor a well-formed DM key is still rejected rather than
// silently routed to a fabricated GUID.
func TestCronSenderUnrecognizedKeyFailsClosed(t *testing.T) {
	g := &Gateway{Log: config.NewLogger(config.LevelError), BlueBubblesURL: "http://bb", BlueBubblesPassword: "pw"}
	g.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("no request should be sent for an unrecognized key")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"guid":"m1"}}`))}, nil
	})}
	sender := g.CronSender()
	if err := sender.Deliver(context.Background(), "alice", "unknown:shape:key", "hi", nil); err == nil {
		t.Fatal("unrecognized session key sent")
	}
}

// TestCronSenderDeliversAttachments covers the cron attachment boundary:
// text goes first and the file attachment follows top-level in the same chat.
func TestCronSenderDeliversAttachments(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var attachmentNames []string
	g := &Gateway{Log: config.NewLogger(config.LevelError), BlueBubblesURL: "http://bb", BlueBubblesPassword: "pw"}
	g.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/api/v1/message/attachment" {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			} else {
				_, header, err := r.FormFile("attachment")
				if err != nil {
					t.Error(err)
				} else {
					attachmentNames = append(attachmentNames, header.Filename)
				}
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"guid":"a1"}}`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"guid":"m1"}}`))}, nil
	})}
	sender := g.CronSender()

	attachments := []media.OutputAttachment{
		{Filename: "sales.txt", MIMEType: "text/plain", Data: []byte("row1")},
	}
	if err := sender.Deliver(context.Background(), "alice", "imessage:chat-guid;+;group:user1", "weekly sales", attachments); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/api/v1/message/text", "/api/v1/message/attachment"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths = %v want %v", paths, want)
		}
	}
	if len(attachmentNames) != 1 || attachmentNames[0] != "sales.txt" {
		t.Fatalf("attachments = %v", attachmentNames)
	}
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
