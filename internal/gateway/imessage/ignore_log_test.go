package imessage

import (
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// ignoredEvent returns the single gateway.message.ignored record from the
// captured log stream, failing when it is missing or duplicated.
func ignoredEvent(t *testing.T, events []map[string]any) map[string]any {
	t.Helper()
	var found map[string]any
	for _, event := range events {
		if event["event"] != "gateway.message.ignored" {
			continue
		}
		if found != nil {
			t.Fatalf("multiple ignored events: %+v", events)
		}
		found = event
	}
	if found == nil {
		t.Fatalf("no ignored event: %+v", events)
	}
	return found
}

func logDetails(t *testing.T, record map[string]any) map[string]any {
	t.Helper()
	details, ok := record["details"].(map[string]any)
	if !ok {
		t.Fatalf("missing details object: %#v", record)
	}
	return details
}

// checkStatus asserts a details status: success is implicit (absent), every
// other status is emitted verbatim.
func checkStatus(t *testing.T, details map[string]any, want string) {
	t.Helper()
	got, exists := details["status"]
	if want == "ok" {
		if exists {
			t.Fatalf("success status emitted: %#v", details)
		}
		return
	}
	if got != want {
		t.Fatalf("status=%v, want %q", got, want)
	}
}

func TestIMessageIgnoredLogDistinguishesMissingReplyReference(t *testing.T) {
	log, events := captureInfoSummaries(t, config.LevelDebug)
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	g.Log = log

	g.processIncomingMessage(webhookMessage{
		GUID:   "msg-1",
		Text:   "casual group chatter",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
	})

	ignored := ignoredEvent(t, events())
	d := logDetails(t, ignored)
	if ignored["level"] != "debug" || d["reason_code"] != "group_message_without_invocation" ||
		d["is_group"] != true || d["is_mention"] != false || d["is_reply"] != false ||
		d["has_thread_root"] != false || d["has_explicit_target"] != false ||
		d["reply_lookup_attempted"] != false || d["reply_found"] != false ||
		d["reply_is_bot"] != false || d["allow_predecessor"] != true {
		t.Fatalf("ignored=%+v", ignored)
	}
	if requestID, ok := d["request_id"].(string); !ok || requestID == "" {
		t.Fatalf("missing request correlation: %+v", ignored)
	}
}

func TestIMessageIgnoredLogReportsIneligibleReplyReference(t *testing.T) {
	log, events := captureInfoSummaries(t, config.LevelDebug)
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	g.Log = log
	g.rememberInboundMessage(webhookMessage{
		GUID:   "human-target",
		Text:   "earlier human message",
		Chats:  []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
		Handle: messageHandle{Address: "+15551234567"},
	}, "unused", "human", "Human")

	g.processIncomingMessage(webhookMessage{
		GUID:        "msg-1",
		Text:        "why was I ignored",
		ReplyToGUID: "human-target",
		Handle:      messageHandle{Address: "+15551234567"},
		Chats:       []messageChat{{GUID: "chat;+;group", Style: chatStyleGroup}},
	})

	ignored := ignoredEvent(t, events())
	d := logDetails(t, ignored)
	if d["reason_code"] != "group_message_without_invocation" ||
		d["is_reply"] != true || d["has_thread_root"] != false || d["has_explicit_target"] != true ||
		d["reply_lookup_attempted"] != true || d["reply_found"] != true ||
		d["reply_is_bot"] != false || d["allow_predecessor"] != true || d["is_mention"] != false {
		t.Fatalf("ignored=%+v", ignored)
	}
}
