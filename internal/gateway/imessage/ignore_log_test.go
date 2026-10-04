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
	if ignored["level"] != "debug" || ignored["reason_code"] != "group_message_without_invocation" ||
		ignored["is_group"] != true || ignored["is_mention"] != false || ignored["is_reply"] != false ||
		ignored["has_thread_root"] != false || ignored["has_explicit_target"] != false ||
		ignored["reply_lookup_attempted"] != false || ignored["reply_found"] != false ||
		ignored["reply_is_bot"] != false || ignored["allow_predecessor"] != true {
		t.Fatalf("ignored=%+v", ignored)
	}
	if requestID, ok := ignored["request_id"].(string); !ok || requestID == "" {
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
	if ignored["reason_code"] != "group_message_without_invocation" ||
		ignored["is_reply"] != true || ignored["has_thread_root"] != false || ignored["has_explicit_target"] != true ||
		ignored["reply_lookup_attempted"] != true || ignored["reply_found"] != true ||
		ignored["reply_is_bot"] != false || ignored["allow_predecessor"] != true || ignored["is_mention"] != false {
		t.Fatalf("ignored=%+v", ignored)
	}
}
