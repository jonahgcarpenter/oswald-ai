package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestAttachmentAndEmbedShareOneDebugSummary(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "summary-log")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := os.Stderr
	os.Stderr = file
	log := config.NewLogger(config.LevelDebug)
	os.Stderr = old
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"id":"synthetic-response"}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "private-body")
	}))
	defer server.Close()
	g, b, _ := newDiscordTestGateway(t, server.URL)
	defer b.Shutdown()
	g.Log = log
	g.HTTPClient = server.Client()
	msg := MessageCreate{ChannelID: "private-chat", Attachments: []Attachment{{Filename: "private-filename", ContentType: "application/private-mime"}}, Embeds: []Embed{{Type: "image", Image: EmbedImage{URL: server.URL + "/asset"}}}}
	msg.Author.ID = "123"
	g.handleReceivedMessage(msg, "req-input-summary", time.Now())
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-filename", "private-address", "private-body", "private-mime", "private-chat", server.URL} {
		if strings.Contains(string(data), private) {
			t.Fatalf("log contains %s", private)
		}
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["event"] != "gateway.attachment.processed" {
			continue
		}
		count++
		d := logDetails(t, event)
		if event["level"] != "debug" || d["status"] != "degraded" || d["gateway"] != "discord" || d["request_id"] != "req-input-summary" || d["user_id"] != nil || d["accepted_count"] != float64(0) || d["downgraded_count"] != float64(2) || d["declared_format_count"] != float64(1) || d["declared_embed_count"] != float64(1) {
			t.Fatalf("summary=%+v", event)
		}
		if duration, ok := d["duration_ms"].(float64); !ok || duration < 0 {
			t.Fatalf("duration=%v", d["duration_ms"])
		}
	}
	if count != 1 {
		t.Fatalf("summary count=%d", count)
	}
}

func logDetails(t *testing.T, record map[string]any) map[string]any {
	t.Helper()
	details, ok := record["details"].(map[string]any)
	if !ok {
		t.Fatalf("missing details object: %#v", record)
	}
	return details
}
