package routing

import (
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

func TestDecideIgnoresUninvokedGroupMessage(t *testing.T) {
	decision := Decide(Input{IsGroup: true, Text: "hello"})
	if decision.Action != ActionIgnore {
		t.Fatalf("expected ignore, got %q", decision.Action)
	}
	if decision.Reason != "group_message_without_invocation" {
		t.Fatalf("unexpected reason %q", decision.Reason)
	}
}

func TestDecideHandlesMentionedGroupCommand(t *testing.T) {
	decision := Decide(Input{IsGroup: true, IsMention: true, IsCommandAttempt: true, Text: " /connect "})
	if decision.Action != ActionCommand {
		t.Fatalf("expected command, got %q", decision.Action)
	}
	if decision.Prompt != "/connect" {
		t.Fatalf("expected trimmed prompt, got %q", decision.Prompt)
	}
}

func TestDecideFallsBackForEmptyPromptWithoutImages(t *testing.T) {
	decision := Decide(Input{Text: " \n\t "})
	if decision.Action != ActionGatewayFallback {
		t.Fatalf("expected fallback, got %q", decision.Action)
	}
	if decision.ResponseText == "" {
		t.Fatal("expected fallback response")
	}
}

func TestBuildPromptIncludesReplyImagesAndUnsupportedAttachments(t *testing.T) {
	current := []llm.InputImage{{MimeType: "image/jpeg", Data: "current"}}
	reply := &ReplyContext{
		SenderName:  "Alice",
		Text:        "look at this",
		Images:      []llm.InputImage{{MimeType: "image/png", Data: "reply"}},
		Unsupported: []string{"doc.pdf"},
	}

	decision := Decide(Input{
		Text:               "what is it?",
		CurrentImages:      current,
		CurrentUnsupported: []string{"notes.txt", "notes.txt", " "},
		Reply:              reply,
	})

	if decision.Action != ActionLLM {
		t.Fatalf("expected llm, got %q", decision.Action)
	}
	if len(decision.Images) != 2 {
		t.Fatalf("expected current and reply image, got %d", len(decision.Images))
	}
	want := "[Replying to Alice: \"look at this\" with image; unsupported attachment: doc.pdf]\n\nwhat is it?\n\n[User sent an unsupported attachment: notes.txt]"
	if decision.Prompt != want {
		t.Fatalf("unexpected prompt:\n%s", decision.Prompt)
	}
}

func TestBuildPromptImageOnlyAndReplyImageLimit(t *testing.T) {
	current := make([]llm.InputImage, media.MaxImagesPerRequest)
	for i := range current {
		current[i] = llm.InputImage{MimeType: "image/jpeg", Data: "x"}
	}
	reply := &ReplyContext{Images: []llm.InputImage{{MimeType: "image/png", Data: "extra"}}}

	decision := Decide(Input{CurrentImages: current, Reply: reply})
	if len(decision.Images) != media.MaxImagesPerRequest {
		t.Fatalf("expected image cap %d, got %d", media.MaxImagesPerRequest, len(decision.Images))
	}
	if decision.Prompt != "[Replying to image]\n\n[User sent 4 images]" {
		t.Fatalf("unexpected prompt %q", decision.Prompt)
	}
}

func TestBuildPromptDescribesGIFContactSheets(t *testing.T) {
	gif := llm.InputImage{MimeType: "image/jpeg", Data: "gif", IsGIFContactSheet: true}
	image := llm.InputImage{MimeType: "image/jpeg", Data: "image"}
	tests := []struct {
		name   string
		images []llm.InputImage
		want   string
	}{
		{name: "single GIF", images: []llm.InputImage{gif}, want: "[User sent a GIF; the attached image is a contact sheet showing its contents over time]"},
		{name: "multiple GIFs", images: []llm.InputImage{gif, gif}, want: "[User sent 2 GIFs; the attached images are contact sheets showing their contents over time]"},
		{name: "mixed", images: []llm.InputImage{image, gif}, want: "[User sent 2 images, including GIF contact sheets showing their contents over time]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := BuildPrompt("", test.images, nil, nil); got != test.want {
				t.Fatalf("prompt = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildPromptDescribesGIFContactSheetInReply(t *testing.T) {
	reply := &ReplyContext{
		SenderName: "Alice",
		Images:     []llm.InputImage{{MimeType: "image/jpeg", Data: "gif", IsGIFContactSheet: true}},
	}
	if got := BuildPrompt("what happens?", nil, nil, reply); got != "[Replying to Alice's GIF contact sheet showing its contents over time]\n\nwhat happens?" {
		t.Fatalf("unexpected prompt %q", got)
	}
}

func TestMessagePreviewCollapsesWhitespaceAndLimitsRunes(t *testing.T) {
	got := MessagePreview("  one\n two\tthree four  ", 13)
	if got != "one two three" {
		t.Fatalf("unexpected preview %q", got)
	}
}

func TestMessagePreviewRedactsConnectionCodes(t *testing.T) {
	got := MessagePreview("/connect OSW-0123-4567-89AB-CDEF-GHJK", 100)
	if got != "/connect OSW-[redacted]" {
		t.Fatalf("unexpected redacted preview %q", got)
	}
}
