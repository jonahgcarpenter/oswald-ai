package discord

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

// TestAttachmentReplyCached proves an outbound file bubble is replyable: the
// attachment message ID is cached as a bot message so reply context resolves
// through the cache even when the referenced payload is thin.
func TestAttachmentReplyCached(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			file, header, err := r.FormFile("files[0]")
			if err != nil {
				t.Error(err)
			} else {
				_, _ = io.ReadAll(file)
				_ = file.Close()
			}
			calls = append(calls, "attachment:"+header.Filename)
		} else {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			calls = append(calls, "text:"+fmt.Sprint(payload["content"]))
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"message-%d"}`, len(calls))))
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	responder := newRuntimeResponder(dg, "request", "channel-1", "source-1", "session", "user")
	if err := responder.SendAgentResponse(&agent.Response{Response: "generated", Attachments: []media.OutputAttachment{{Filename: "generated.png", MIMEType: "image/png", Data: []byte("image-data")}}}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"attachment:generated.png", "text:generated"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
	cached, ok := dg.lookupReply("message-1")
	if !ok || !cached.IsFromBot {
		t.Fatalf("attachment message was not cached as bot reply: ok=%v cached=%+v", ok, cached)
	}
	if len(cached.Attachments) != 1 || cached.Attachments[0].Filename != "generated.png" {
		t.Fatalf("cached attachment metadata=%+v want filename generated.png", cached.Attachments)
	}
	if _, ok := dg.lookupReply("message-2"); !ok {
		t.Fatal("text message was not cached")
	}

	msg := MessageCreate{ID: "reply-1", ChannelID: "channel-1"}
	msg.ReferencedMessage = &struct {
		ID          string       `json:"id"`
		Content     string       `json:"content"`
		Attachments []Attachment `json:"attachments,omitempty"`
		Embeds      []Embed      `json:"embeds,omitempty"`
		Author      struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"author"`
	}{ID: "message-1"}
	re := regexp.MustCompile(`<a?:([^:]+):\d+>`)
	reply := dg.resolveReplyContext(msg, re, nil, "request")
	if reply == nil || !reply.IsFromBot {
		t.Fatalf("reply to attachment did not resolve as bot reply: %+v", reply)
	}
}
