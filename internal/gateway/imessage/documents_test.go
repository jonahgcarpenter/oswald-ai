package imessage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestCurrentDocumentDownloadAndReplyExclusion(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("password") != "test-secret" {
			t.Error("missing BlueBubbles credential")
		}
		_, _ = io.WriteString(w, "document")
	}))
	defer server.Close()
	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "test-secret", Log: config.NewLogger(config.LevelError)}
	attachments := []attachment{{GUID: "doc", TransferName: "report.pdf"}, {GUID: "img", TransferName: "photo.png", MimeType: "image/png"}}
	remaining, loader := g.currentDocuments(attachments)
	if len(remaining) != 1 || loader == nil || calls.Load() != 0 {
		t.Fatal("incorrect lazy partition")
	}
	if _, err := loader.Load(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatalf("download=%v calls=%d", err, calls.Load())
	}
	images, unsupported := g.loadImages(attachments[:1])
	if len(images) != 0 || len(unsupported) != 1 || calls.Load() != 1 {
		t.Fatal("replied document downloaded or admitted")
	}
}
