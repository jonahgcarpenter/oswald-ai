package discord

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBannedDiscordIdentitySkipsAttachmentDownloadAndModel(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	g, b, model := newDiscordTestGateway(t, server.URL)
	defer b.Shutdown()
	msg := discordMessage("banned-message", "banned-chat", "", "456", "Synthetic", "inspect")
	msg.Attachments = []Attachment{{Filename: "synthetic.png", ContentType: "image/png", URL: server.URL + "/image"}}
	g.handleMessage(msg)
	model.mu.Lock()
	defer model.mu.Unlock()
	if calls.Load() != 0 || len(model.requests) != 0 {
		t.Fatal("banned identity performed network or model work")
	}
}
