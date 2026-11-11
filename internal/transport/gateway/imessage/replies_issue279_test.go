package imessage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/docache"
)

// TestIssue279AttachmentSendsCacheGUIDs pins the fast path: a turn that
// sends text followed by a file attachment caches both message GUIDs, so a
// direct reply to either bubble resolves without a BlueBubbles round trip.
func TestIssue279AttachmentSendsCacheGUIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/message/text":
			_, _ = w.Write([]byte(`{"data":{"guid":"text-1"}}`))
		case "/api/v1/message/attachment":
			_, _ = w.Write([]byte(`{"data":{"guid":"attach-1"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError), messageIndex: make(map[string]messageContext)}

	responder := &runtimeResponder{gateway: g, requestID: "req-279", chatGUID: "chat-1", sessionKey: "imessage:dm:+15550001111", senderID: "+15550001111"}
	if err := responder.SendAgentResponse(&agent.Response{Response: "here is your file", Attachments: []media.OutputAttachment{{Filename: "report.png", MIMEType: "image/png", Data: []byte("image-data")}}}); err != nil {
		t.Fatal(err)
	}
	if !g.isCachedBotMessage("text-1", "chat-1") {
		t.Fatal("text reply GUID was not cached")
	}
	if !g.isCachedBotMessage("attach-1", "chat-1") {
		t.Fatal("attachment reply GUID was not cached")
	}

	responder = &runtimeResponder{gateway: g, requestID: "req-279-cmd", chatGUID: "chat-1", sessionKey: "imessage:dm:+15550001111", senderID: "+15550001111"}
	if err := responder.SendCommandResponse(commands.Result{Text: "command file", Attachments: []commands.Attachment{{Filename: "export.json", MIMEType: "application/json", Data: []byte("private-content")}}}); err != nil {
		t.Fatal(err)
	}
	if !g.isCachedBotMessage("text-1", "chat-1") || !g.isCachedBotMessage("attach-1", "chat-1") {
		t.Fatal("command text/attachment GUIDs were not cached")
	}
}

// TestIssue279FileBubbleReplyContextIsBot proves the content-validity half
// of file-reply admission: a bot file bubble with empty text and an
// unknown-type attachment (empty MIME, dynamic UTI, real transfer name)
// still counts as bot-authored context instead of vanishing as a preview.
func TestIssue279FileBubbleReplyContextIsBot(t *testing.T) {
	g := &Gateway{Log: config.NewLogger(config.LevelError)}
	zero, no := 0, false
	data := messageLookupData{
		GUID: "file-1", OriginalROWID: 2, Text: "",
		IsFromMe: true, SendError: &zero, IsSystemMessage: &no, IsServiceMessage: &no,
		Attachments: []attachment{{GUID: "a1", TransferName: "report.llir", MimeType: "", UTI: "dyn.abcdef12", TotalBytes: 13607}},
		Handle:      messageHandle{Address: "+15550001111"}, Chats: []messageChat{{GUID: "chat-1", Style: chatStyleDirect}},
	}
	ctx := g.replyContextFromMessage(data, "chat-1")
	if !ctx.IsFromBot {
		t.Fatal("file bubble did not resolve as bot-authored")
	}
	if len(ctx.Attachments) != 1 || ctx.Attachments[0].TransferName != "report.llir" {
		t.Fatalf("file attachment lost from reply context: %+v", ctx.Attachments)
	}
}

// TestIssue279FilePredecessorAdmittedWithoutCache is the end-to-end form of
// the filed repro: a threaded reply whose chronological predecessor is a
// bot file bubble is admitted with an empty fast-path cache.
func TestIssue279FilePredecessorAdmittedWithoutCache(t *testing.T) {
	root := replyFixture("root", 1, false)
	file := replyFixture("file", 2, true)
	file.Text = ""
	file.Attachments = []attachment{{GUID: "file-attachment", TransferName: "report.llir", MimeType: "", UTI: "dyn.abcdef12", TotalBytes: 13607}}
	anchor := replyFixture("incoming", 10, false)
	file.ThreadOriginatorGUID, anchor.ThreadOriginatorGUID = "root", "root"
	file.ThreadOriginatorPart, anchor.ThreadOriginatorPart = "0:0", "0:0"

	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	bb.mu.Lock()
	bb.messageLookup = sqliteReplyLookup(t, []messageLookupData{root, file, anchor}, map[string]int64{"root": 100, "file": 200, "incoming": 300})
	bb.mu.Unlock()
	g, b, model := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	g.processIncomingMessage(threadIncoming())
	if len(model.primaryRequests()) != 1 {
		t.Fatalf("file predecessor was not admitted, requests=%d", len(model.primaryRequests()))
	}
}

// TestIssue279FileReplyAttachesDocument is the enrichment half of the
// filed repro: replying to a bot text file downloads it into the private
// document cache and names it in the prompt, mirroring image replies,
// instead of reporting Message unavailable.
func TestIssue279FileReplyAttachesDocument(t *testing.T) {
	no, zero := false, 0
	file := messageLookupData{
		GUID: "file-1", OriginalROWID: 2, Text: "",
		IsFromMe: true, SendError: &zero, IsSystemMessage: &no, IsServiceMessage: &no,
		Attachments: []attachment{{GUID: "att-1", TransferName: "test.txt", MimeType: "text/plain", TotalBytes: 4}},
		Chats:       []messageChat{{GUID: "direct-chat", Style: chatStyleDirect}},
	}
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	bb.mu.Lock()
	bb.messageLookup = sqliteReplyLookup(t, []messageLookupData{file}, map[string]int64{"file": 200})
	bb.mu.Unlock()
	g, b, model := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	// The document cache enforces owner-only directories like production;
	// test tempdirs need the same tightening.
	docRoot := t.TempDir()
	if err := os.Chmod(docRoot, 0700); err != nil {
		t.Fatal(err)
	}
	g.DocCache = docache.New(docRoot)

	g.processIncomingMessage(webhookMessage{
		GUID: "incoming-1", Text: "translate it", ReplyToGUID: "file-1",
		Handle: messageHandle{Address: "+15551234567"}, Chats: []messageChat{{GUID: "direct-chat", Style: chatStyleDirect}},
	})
	primary := model.primaryRequests()
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	prompt := primary[0].Messages[len(primary[0].Messages)-1].Content
	if !strings.Contains(prompt, "[Replying to Oswald's file]") || !strings.Contains(prompt, "test.txt") {
		t.Fatalf("prompt missing file reply context: %q", prompt)
	}
}

// TestDropAttachedLabelsKeepsSingleTruth pins the dedup: a file that rode
// into the turn as a document must not also be reported unreadable.
func TestDropAttachedLabelsKeepsSingleTruth(t *testing.T) {
	labels := []string{"test.txt", "notes.txt"}
	docs := []media.DocumentRef{{Path: "/private/test.txt", Filename: "test.txt", MIMEType: "text/plain"}}
	got := dropAttachedLabels(labels, docs)
	if len(got) != 1 || got[0] != "notes.txt" {
		t.Fatalf("attached label not dropped: %+v", got)
	}
	if got := dropAttachedLabels(labels, nil); len(got) != 2 {
		t.Fatalf("labels changed without docs: %+v", got)
	}
}

// TestIssue279DirectReplyToAttachmentAdmittedWithoutCache proves L2
// authority: with an empty fast-path cache (simulating a restart past any
// TTL), a direct reply targeting the attachment GUID still resolves as a
// bot reply from the BlueBubbles record alone.
func TestIssue279DirectReplyToAttachmentAdmittedWithoutCache(t *testing.T) {
	no, zero := false, 0
	botAttachment := messageLookupData{
		GUID: "attach-1", OriginalROWID: 2, Text: "report",
		IsFromMe: true, SendError: &zero, IsSystemMessage: &no, IsServiceMessage: &no,
		Chats: []messageChat{{GUID: "chat-1", Style: chatStyleDirect}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/message/text":
			_, _ = w.Write([]byte(`{"data":{"guid":"text-1"}}`))
		case "/api/v1/message/attachment":
			_, _ = w.Write([]byte(`{"data":{"guid":"attach-1"}}`))
		case "/api/v1/message/query":
			_ = json.NewEncoder(w).Encode(messageQueryResponse{Data: []messageLookupData{botAttachment}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError), messageIndex: make(map[string]messageContext)}

	responder := &runtimeResponder{gateway: g, requestID: "req-279", chatGUID: "chat-1", sessionKey: "imessage:dm:+15550001111", senderID: "+15550001111"}
	if err := responder.SendAgentResponse(&agent.Response{Response: "here is your file", Attachments: []media.OutputAttachment{{Filename: "report.png", MIMEType: "image/png", Data: []byte("image-data")}}}); err != nil {
		t.Fatal(err)
	}
	// Simulate a restart: the fast-path cache is gone.
	g.messageIndex = make(map[string]messageContext)

	incoming := webhookMessage{
		GUID: "incoming-1", Text: "thanks", ReplyToGUID: "attach-1",
		Handle: messageHandle{Address: "+15550001111"}, Chats: []messageChat{{GUID: "chat-1", Style: chatStyleDirect}},
	}
	resolved, found := g.resolveReply(context.Background(), incoming, false, "req-279")
	if !found || !resolved.IsFromBot {
		t.Fatalf("direct reply to attachment was not admitted without cache: found=%v isFromBot=%v", found, resolved.IsFromBot)
	}
}

// TestMessageIndexByteBudgetEvictsOldest pins the L1 bound: the fast-path
// index sheds oldest-first past its byte budget, so RAM cannot grow
// indefinitely. Eviction only costs a future round trip, never admission.
func TestMessageIndexByteBudgetEvictsOldest(t *testing.T) {
	g := &Gateway{Log: config.NewLogger(config.LevelError), messageIndex: make(map[string]messageContext)}
	filler := strings.Repeat("x", 100<<10)
	const entries = 25
	for i := 0; i < entries; i++ {
		g.rememberMessage(string(rune('a'+i))+string(rune('0'+i%10)), messageContext{
			ChatGUID: "chat-1", Text: filler, IsFromBot: true, CreatedAt: time.Now().Add(-time.Duration(entries-i) * time.Second),
		})
	}
	total := 0
	for guid, ctx := range g.messageIndex {
		total += messageContextSize(guid, ctx)
	}
	if total > messageIndexMaxBytes {
		t.Fatalf("index exceeds budget after eviction: %d > %d", total, messageIndexMaxBytes)
	}
	if len(g.messageIndex) >= entries {
		t.Fatalf("no eviction happened: %d entries", len(g.messageIndex))
	}
	if _, ok := g.lookupMessage("a0"); ok {
		t.Fatal("oldest entry survived eviction")
	}
}
