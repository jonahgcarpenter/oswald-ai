package imessage

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBannedIMessageIdentitySkipsReplyAttachmentAndIndicators(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	g, b, model := newIMessageTestGateway(t, server.URL)
	defer b.Shutdown()
	g.processIncomingMessage(webhookMessage{GUID: "banned-message", Text: "inspect", ReplyToGUID: "synthetic-reply", Handle: messageHandle{Address: "+15557654321"}, Chats: []messageChat{{GUID: "synthetic-chat", Style: chatStyleDirect}}, Attachments: []attachment{{GUID: "synthetic-image", MimeType: "image/png"}}})
	model.mu.Lock()
	defer model.mu.Unlock()
	if calls.Load() != 0 || len(model.requests) != 0 {
		t.Fatal("banned identity performed reply, media, indicator, or model work")
	}
}
