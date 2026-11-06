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
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
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
	// The canonical "iMessage" prefix is sent; the server normalizes service
	// prefixes (any/iMessage/SMS) to the stored chat on delivery.
	if err := sender.Deliver(context.Background(), "alice", "imessage:dm:stranger", "hi", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(guids) != 3 || guids[2] != "/api/v1/message/text iMessage;-;stranger" {
		t.Fatalf("expected derived direct-chat GUID fallback, got %v", guids)
	}
	mu.Unlock()
}

// TestCronSenderMentionBubbleAndThread covers the groupchat mention path: a
// single-part multipart mention bubble, then the body threaded under it via
// the bubble's GUID. The label is profile-scoped, the address is the
// transport-native handle.
func TestCronSenderMentionBubbleAndThread(t *testing.T) {
	var mu sync.Mutex
	type call struct {
		path    string
		payload map[string]interface{}
	}
	var calls []call
	g := &Gateway{Log: config.NewLogger(config.LevelError), BlueBubblesURL: "http://bb", BlueBubblesPassword: "pw"}
	g.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && r.URL.Path != "/api/v1/server/info" {
			var payload map[string]interface{}
			if r.Body != nil {
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
			}
			mu.Lock()
			calls = append(calls, call{path: r.URL.Path, payload: payload})
			mu.Unlock()
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"guid":"bubble-1"}}`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"private_api":true,"helper_connected":true}}`))}, nil
	})}
	sender := g.CronSender()

	mention := cronMention("bob", "pick")
	if err := sender.DeliverMention(context.Background(), "alice", "imessage:chat-guid;+;group:bob", mention, "pick body", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	first := calls[0]
	if first.path != "/api/v1/message/multipart" || toString(first.payload["chatGuid"]) != "chat-guid;+;group" {
		t.Fatalf("mention call = %+v", first)
	}
	parts, _ := first.payload["parts"].([]interface{})
	if len(parts) != 1 {
		t.Fatalf("mention parts = %+v", first.payload["parts"])
	}
	part, _ := parts[0].(map[string]interface{})
	if toString(part["text"]) != "alice - pick" || toString(part["mention"]) != "bob" {
		t.Fatalf("mention part = %+v", part)
	}
	second := calls[1]
	if second.path != "/api/v1/message/text" || toString(second.payload["selectedMessageGuid"]) != "bubble-1" || toString(second.payload["message"]) != "pick body" {
		t.Fatalf("threaded reply = %+v", second)
	}
}

// TestCronSenderMentionFallbacksPlain guards content delivery when the mention
// machinery fails: a failed bubble falls back to a plain send, and a failed
// thread follows up as plain text so the body is never lost.
func TestCronSenderMentionFallbacksPlain(t *testing.T) {
	seedPrivateAPI := func(g *Gateway) {
		g.capabilityMu.Lock()
		g.capabilitiesLoaded = true
		g.privateAPIEnabled = true
		g.helperConnected = true
		g.capabilityMu.Unlock()
	}
	newFailing := func(fail func(path string, payload map[string]interface{}) bool) (*Gateway, *[]string, *[]map[string]interface{}) {
		var mu sync.Mutex
		var paths []string
		var payloads []map[string]interface{}
		g := &Gateway{Log: config.NewLogger(config.LevelError), BlueBubblesURL: "http://bb", BlueBubblesPassword: "pw"}
		seedPrivateAPI(g)
		g.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var payload map[string]interface{}
			if r.Method == http.MethodPost && r.Body != nil {
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					payload = nil
				}
			}
			mu.Lock()
			paths = append(paths, r.URL.Path)
			if payload != nil {
				payloads = append(payloads, payload)
			}
			mu.Unlock()
			if fail(r.URL.Path, payload) {
				return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"guid":"m1"}}`))}, nil
		})}
		return g, &paths, &payloads
	}
	mention := cronMention("bob", "pick")

	// Mention bubble fails: single plain send carries the body.
	failMultipart := func(path string, _ map[string]interface{}) bool { return path == "/api/v1/message/multipart" }
	g, paths, _ := newFailing(failMultipart)
	if err := g.CronSender().DeliverMention(context.Background(), "alice", "imessage:chat-guid;+;group:bob", mention, "pick body", nil); err != nil {
		t.Fatalf("mention failure should fall back to plain send: %v", err)
	}
	if len(*paths) != 2 || (*paths)[0] != "/api/v1/message/multipart" || (*paths)[1] != "/api/v1/message/text" {
		t.Fatalf("fallback paths = %v", *paths)
	}

	// Threaded reply fails: the body follows up as plain text (no reply
	// fields), so the mention still tapped and nothing is lost.
	// Threaded reply fails: the body follows up as plain text (no reply
	// fields), so the mention still tapped and nothing is lost.
	failThreadedReply := func(path string, payload map[string]interface{}) bool {
		return path == "/api/v1/message/text" && payload != nil && toString(payload["selectedMessageGuid"]) != ""
	}
	g, paths, payloads := newFailing(failThreadedReply)
	_ = payloads
	if err := g.CronSender().DeliverMention(context.Background(), "alice", "imessage:chat-guid;+;group:bob", mention, "pick body", nil); err != nil {
		t.Fatalf("thread failure should follow up plain: %v", err)
	}
	if len(*paths) != 3 {
		t.Fatalf("thread-fallback paths = %v", *paths)
	}
	if len(*payloads) != 3 {
		t.Fatalf("thread-fallback payloads = %+v", *payloads)
	}
	if toString((*payloads)[1]["selectedMessageGuid"]) == "" {
		t.Fatalf("thread attempt missing reply target: %+v", (*payloads)[1])
	}
	followUp := (*payloads)[2]
	if _, hasReply := followUp["selectedMessageGuid"]; hasReply {
		t.Fatalf("follow-up still threads: %+v", followUp)
	}
	if toString(followUp["message"]) != "pick body" {
		t.Fatalf("follow-up lost the body: %+v", followUp)
	}
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

// TestCronChatCacheDropsEmptyBindings guards the routing cache against
// malformed webhooks (for example an empty chats array): empty session keys
// and chat GUIDs are never stored, so a later delivery can never match them.
func TestCronChatCacheDropsEmptyBindings(t *testing.T) {
	rememberCronChat("", "some-guid")
	rememberCronChat("imessage:dm:cache-guard-user", "")
	rememberCronChat("  ", "  ")
	if _, ok := lookupCronChat(""); ok {
		t.Fatal("empty session key cached")
	}
	if _, ok := lookupCronChat("imessage:dm:cache-guard-user"); ok {
		t.Fatal("empty chat GUID cached")
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

func cronMention(address, header string) cron.Mention {
	return cron.Mention{Address: address, Header: header}
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
