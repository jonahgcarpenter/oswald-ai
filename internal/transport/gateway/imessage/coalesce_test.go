package imessage

import (
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func int64ptr(v int64) *int64 { return &v }

func TestMergeImageLinkTextDropsPreview(t *testing.T) {
	linkCreated := int64(1792770448206)
	textCreated := int64(1792770448258)
	imageCreated := int64(1792770447749)
	entries := []coalesceEntry{
		{msg: webhookMessage{GUID: "link-guid", Text: "https://a.co/d/04NJSvma", OriginalROWID: 9314, DateCreated: &linkCreated, BalloonBundleID: "com.apple.messages.URLBalloonProvider", Attachments: []attachment{{GUID: "preview-1", TransferName: "AF.pluginPayloadAttachment", UTI: "dyn.age81a5dzq7y066dbtf0g82peqf4hk2pdrb00n5xy"}, {GUID: "preview-1", TransferName: "AF.pluginPayloadAttachment"}}, Handle: messageHandle{Address: "+15551234567"}, Chats: []messageChat{{GUID: "chat", Style: chatStyleDirect}}}, requestID: "req-1", order: 0},
		{msg: webhookMessage{GUID: "text-guid", Text: "Image + Link + Text", OriginalROWID: 9315, DateCreated: &textCreated, Handle: messageHandle{Address: "+15551234567"}, Chats: []messageChat{{GUID: "chat", Style: chatStyleDirect}}}, requestID: "req-2", order: 1},
		{msg: webhookMessage{GUID: "image-guid", Text: "", OriginalROWID: 9313, DateCreated: &imageCreated, Attachments: []attachment{{GUID: "img-1", MimeType: "image/jpeg", TransferName: "IMG_5319.JPG", UTI: "public.jpeg"}}, Handle: messageHandle{Address: "+15551234567"}, Chats: []messageChat{{GUID: "chat", Style: chatStyleDirect}}}, requestID: "req-3", order: 2},
	}
	merged, requestID, _ := mergeCoalescedEntries(entries)
	if requestID != "req-1" {
		t.Fatalf("expected arrival request id, got %q", requestID)
	}
	if got := merged.Text; got != "https://a.co/d/04NJSvma\nImage + Link + Text" {
		t.Fatalf("unexpected merged text %q", got)
	}
	if len(merged.Attachments) != 1 || merged.Attachments[0].GUID != "img-1" {
		t.Fatalf("preview not filtered: %+v", merged.Attachments)
	}
	if merged.GUID != "image-guid" {
		t.Fatalf("expected earliest guid, got %q", merged.GUID)
	}
}

func TestMergeLinkText(t *testing.T) {
	first := int64(1792770426655)
	second := int64(1792770426704)
	entries := []coalesceEntry{
		{msg: webhookMessage{GUID: "link", Text: "https://a.co/d/04NJSvma", OriginalROWID: 9311, DateCreated: &first, BalloonBundleID: urlBalloonProvider, Attachments: []attachment{{GUID: "p", TransferName: "x.pluginPayloadAttachment"}}, Chats: []messageChat{{GUID: "chat", Style: chatStyleDirect}}}, order: 0},
		{msg: webhookMessage{GUID: "text", Text: "Link + Text", OriginalROWID: 9312, DateCreated: &second, Chats: []messageChat{{GUID: "chat", Style: chatStyleDirect}}}, order: 1},
	}
	merged, _, _ := mergeCoalescedEntries(entries)
	if merged.Text != "https://a.co/d/04NJSvma\nLink + Text" {
		t.Fatalf("unexpected text %q", merged.Text)
	}
	if len(merged.Attachments) != 0 {
		t.Fatalf("expected no attachments, got %+v", merged.Attachments)
	}
}

func TestIsPreviewAttachment(t *testing.T) {
	if !isPreviewAttachment(attachment{TransferName: "AF.pluginPayloadAttachment"}) {
		t.Fatal("plugin payload not detected")
	}
	if !isPreviewAttachment(attachment{UTI: "dyn.age81a5dzq7y066dbtf0g82peqf4hk2pdrb00n5xy"}) {
		t.Fatal("dyn uti not detected")
	}
	if isPreviewAttachment(attachment{MimeType: "image/png", TransferName: "IMG_5333.png", UTI: "public.png"}) {
		t.Fatal("real image flagged as preview")
	}
	if isPreviewAttachment(attachment{MimeType: "image/jpeg", TransferName: "IMG_5305.HEIC.jpeg", UTI: "public.heic"}) {
		t.Fatal("real photo flagged as preview")
	}
}

func TestEnqueueDedupesSameGUID(t *testing.T) {
	g := &Gateway{Log: config.NewLogger(config.LevelError), coalesceQuiet: 10 * time.Second, coalesceMaxHold: 20 * time.Second}
	msg := webhookMessage{GUID: "dup", Text: "hello", Handle: messageHandle{Address: "+15551234567"}, Chats: []messageChat{{GUID: "chat", Style: chatStyleDirect}}}
	g.enqueueCoalesced(msg, "req-1", time.Now())
	g.enqueueCoalesced(msg, "req-2", time.Now())
	g.coalesceMu.Lock()
	defer g.coalesceMu.Unlock()
	key := "chat\x00+15551234567"
	batch := g.coalesceBatches[key]
	if batch == nil || len(batch.entries) != 1 {
		t.Fatalf("duplicate guid not deduped: %+v", batch)
	}
	if batch.timer != nil {
		batch.timer.Stop()
	}
}

func TestFilterRealAttachments(t *testing.T) {
	preview := attachment{GUID: "p", TransferName: "AF.pluginPayloadAttachment", UTI: "dyn.age81a5dzq7y066dbtf0g82peqf4hk2pdrb00n5xy"}
	real := attachment{GUID: "img", MimeType: "image/jpeg", TransferName: "IMG_5319.JPG", UTI: "public.jpeg"}
	if got := filterRealAttachments([]attachment{preview, real}, ""); len(got) != 1 || got[0].GUID != "img" {
		t.Fatalf("attachment-level preview not filtered: %+v", got)
	}
	if got := filterRealAttachments([]attachment{real}, urlBalloonProvider); len(got) != 0 {
		t.Fatalf("balloon provider not filtered: %+v", got)
	}
}

func TestReplyContextFromLinkPreviewDropsImages(t *testing.T) {
	g := &Gateway{Log: config.NewLogger(config.LevelError)}
	data := messageLookupData{
		GUID: "link-guid", Text: "https://x.com/popcrave/status/123", BalloonBundleID: urlBalloonProvider,
		Attachments: []attachment{{GUID: "p1", TransferName: "A.pluginPayloadAttachment"}, {GUID: "p2", UTI: "dyn.age81a5dzq7y066dbtf0g82peqf4hk2pdrb00n5xy"}},
		Handle:      messageHandle{Address: "+15551234567"}, Chats: []messageChat{{GUID: "chat", Style: chatStyleDirect}},
	}
	ctx := g.replyContextFromMessage(data, "chat")
	if ctx.Text != "https://x.com/popcrave/status/123" {
		t.Fatalf("reply text lost: %q", ctx.Text)
	}
	if len(ctx.Attachments) != 0 {
		t.Fatalf("reply preview not filtered: %+v", ctx.Attachments)
	}
}
