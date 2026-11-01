package discord

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

type cronRoundTripper func(*http.Request) (*http.Response, error)

func (f cronRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestCronSenderGroupAndDM covers every origin shape the cron dispatcher can
// route. Server and group channels carry their channel ID in the key; DM
// sessions resolve through remembered inbound traffic, falling back to the
// Discord API so delivery works with no inbound traffic since process start.
func TestCronSenderGroupAndDM(t *testing.T) {
	var mu sync.Mutex
	var sends []string
	var dmOpens []string
	dg := &Gateway{Log: config.NewLogger(config.LevelError)}
	dg.HTTPClient = &http.Client{Transport: cronRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/users/@me/channels") {
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			mu.Lock()
			if rec, ok := payload["recipient_id"].(string); ok && rec != "" {
				dmOpens = append(dmOpens, r.Method+" "+r.URL.Path+" "+rec)
			} else {
				dmOpens = append(dmOpens, r.Method+" "+r.URL.Path+" <bad recipient_id>")
			}
			mu.Unlock()
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"resolved-dm-chan"}`))}, nil
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		sends = append(sends, r.URL.Path+" "+toString(payload["content"]))
		mu.Unlock()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"m1"}`))}, nil
	})}
	sender := dg.CronSender()

	// Server channel session carries its channel ID in the key; no lookup is
	// triggered.
	if err := sender.Deliver(context.Background(), "alice", "discord:chan9:user1", "hello group", nil); err != nil {
		t.Fatal(err)
	}
	// DM resolves through remembered inbound traffic.
	rememberCronChannel("discord:dm:user1", "dm-chan")
	if err := sender.Deliver(context.Background(), "alice", "discord:dm:user1", "hello dm", nil); err != nil {
		t.Fatal(err)
	}
	// Long text chunks to Discord-sized messages.
	long := strings.Repeat("x", 2500)
	if err := sender.Deliver(context.Background(), "alice", "discord:chan9:user1", long, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(sends) != 4 {
		t.Fatalf("messages = %d", len(sends))
	}
	if !strings.HasSuffix(sends[0], "/channels/chan9/messages hello group") || !strings.HasSuffix(sends[1], "/channels/dm-chan/messages hello dm") {
		t.Fatalf("sends = %v", sends)
	}
	if len(dmOpens) != 0 {
		t.Fatalf("expected no DM channel lookup for remembered/known channels, got %v", dmOpens)
	}
	mu.Unlock()

	// Unknown DM session falls back to the Discord API (POST /users/@me/channels
	// returns the existing DM channel when one already exists) and delivers
	// to the resolved channel, so a fresh process can still deliver.
	if err := sender.Deliver(context.Background(), "alice", "discord:dm:stranger", "hi", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(dmOpens) != 1 || !strings.HasPrefix(dmOpens[0], "POST ") || !strings.Contains(dmOpens[0], "/users/@me/channels stranger") {
		t.Fatalf("expected idempotent DM channel open for stranger, got %v", dmOpens)
	}
	if len(sends) != 5 || !strings.HasSuffix(sends[4], "/channels/resolved-dm-chan/messages hi") {
		t.Fatalf("expected delivery to resolved DM channel, got %v", sends)
	}
	mu.Unlock()
}

// TestCronSenderUnrecognizedKeyFailsClosed guards that a session key that is
// neither a channel key nor a well-formed DM key is still rejected rather than
// silently routed.
func TestCronSenderUnrecognizedKeyFailsClosed(t *testing.T) {
	dg := &Gateway{Log: config.NewLogger(config.LevelError)}
	dg.HTTPClient = &http.Client{Transport: cronRoundTripper(func(r *http.Request) (*http.Response, error) {
		t.Error("no request should be sent for an unrecognized key")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"m1"}`))}, nil
	})}
	sender := dg.CronSender()
	if err := sender.Deliver(context.Background(), "alice", "unknown:shape:key", "hi", nil); err == nil {
		t.Fatal("unrecognized session key sent")
	}
}

// TestCronChannelCacheDropsEmptyBindings guards the routing cache against
// malformed traffic: empty session keys and channel IDs are never stored,
// so a later delivery can never match them.
func TestCronChannelCacheDropsEmptyBindings(t *testing.T) {
	rememberCronChannel("", "some-channel")
	rememberCronChannel("discord:dm:cache-guard-user", "")
	rememberCronChannel("  ", "  ")
	if _, ok := lookupCronChannel(""); ok {
		t.Fatal("empty session key cached")
	}
	if _, ok := lookupCronChannel("discord:dm:cache-guard-user"); ok {
		t.Fatal("empty channel cached")
	}
}

// TestCronSenderDeliversAttachments covers the cron attachment boundary:
// text goes first and file attachments follow in one message.
func TestCronSenderDeliversAttachments(t *testing.T) {
	var mu sync.Mutex
	var contents []string
	var files []string
	dg := &Gateway{Log: config.NewLogger(config.LevelError)}
	dg.HTTPClient = &http.Client{Transport: cronRoundTripper(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			} else if r.MultipartForm != nil {
				for _, headers := range r.MultipartForm.File {
					for _, h := range headers {
						files = append(files, h.Filename)
					}
				}
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"m2"}`))}, nil
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		contents = append(contents, toString(payload["content"]))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"m1"}`))}, nil
	})}
	sender := dg.CronSender()

	attachments := []media.OutputAttachment{
		{Filename: "sales.txt", MIMEType: "text/plain", Data: []byte("row1")},
	}
	if err := sender.Deliver(context.Background(), "alice", "discord:chan9:user1", "weekly sales", attachments); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(contents) != 1 || contents[0] != "weekly sales" {
		t.Fatalf("contents = %v", contents)
	}
	if len(files) != 1 || files[0] != "sales.txt" {
		t.Fatalf("files = %v", files)
	}
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
