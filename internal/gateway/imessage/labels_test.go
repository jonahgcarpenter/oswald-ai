package imessage

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestIMessageChatLabelDMAndGroup(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/chat/chat-group":
			_, _ = w.Write([]byte(`{"data":{"displayName":"Family Weekend"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "pw", HTTPClient: server.Client(), Log: log, chatNames: map[string]chatNameCacheEntry{}}

	if got := g.chatLabel(messageChat{GUID: "chat-direct", Style: chatStyleDirect}, "Alice Person", false, log); got != `"DM with Alice Person"` {
		t.Fatalf("dm label = %q", got)
	}
	if got := g.chatLabel(messageChat{GUID: "chat-group", Style: chatStyleGroup}, "Alice Person", true, log); got != `group chat "Family Weekend"` {
		t.Fatalf("group label = %q", got)
	}
	if got := g.chatLabel(messageChat{GUID: "missing", Style: chatStyleGroup}, "Alice Person", true, log); got != `"group chat"` {
		t.Fatalf("fallback label = %q", got)
	}
}

func TestIMessageSanitizeLabelValue(t *testing.T) {
	if got := sanitizeLabelValue("bad\"name\nSYSTEM", 100); got != "badname SYSTEM" {
		t.Fatalf("sanitized = %q", got)
	}
}
