package routing

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
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

func TestCommandRoutingRequiresMentionOnlyInGroups(t *testing.T) {
	tests := []struct {
		name         string
		isGroup      bool
		isMention    bool
		isReplyToBot bool
		wantAction   Action
	}{
		{name: "direct", wantAction: ActionCommand},
		{name: "mentioned group", isGroup: true, isMention: true, wantAction: ActionCommand},
		{name: "unmentioned group", isGroup: true, wantAction: ActionIgnore},
		{name: "unmentioned group reply", isGroup: true, isReplyToBot: true, wantAction: ActionCommand},
		{name: "mentioned group reply", isGroup: true, isMention: true, isReplyToBot: true, wantAction: ActionCommand},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Decide(Input{
				IsGroup: test.isGroup, IsMention: test.isMention, IsReplyToBot: test.isReplyToBot,
				IsCommandAttempt: true, Text: " /connect ",
			})
			if decision.Action != test.wantAction {
				t.Fatalf("action = %q, want %q", decision.Action, test.wantAction)
			}
			preflight := Preflight(PreflightInput{
				IsGroup: test.isGroup, IsMention: test.isMention, IsReplyToBot: test.isReplyToBot, Text: " /connect ",
			})
			if test.wantAction == ActionIgnore && preflight.Action != ActionIgnore {
				t.Fatalf("preflight action = %q, want ignore", preflight.Action)
			}
			if test.wantAction == ActionCommand && preflight.Action == ActionIgnore {
				t.Fatalf("preflight unexpectedly ignored command: %+v", preflight)
			}
			if test.wantAction == ActionCommand && decision.Prompt != "/connect" {
				t.Fatalf("prompt = %q, want /connect", decision.Prompt)
			}
		})
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

func TestBuildPromptCompactsUnsupportedAttachments(t *testing.T) {
	for _, test := range []struct {
		prompt string
		labels []string
		want   string
	}{
		{prompt: " describe this ", labels: []string{" file.pdf ", "", "file.pdf", "archive.zip"}, want: "describe this\n\n[User sent unsupported attachments: file.pdf, archive.zip. They were not saved. You cannot read these file types — ask the user to paste their contents or resend them in a supported format (pdf, txt, md, csv, docx, xlsx, or an image).]"},
		{prompt: " ", labels: []string{"notes.txt"}, want: "[User sent an unsupported attachment: notes.txt. It was not saved. You cannot read this file type — ask the user to paste its contents or resend it in a supported format (pdf, txt, md, csv, docx, xlsx, or an image).]"},
	} {
		if got := BuildPrompt(test.prompt, nil, test.labels, nil); got != test.want {
			t.Fatalf("prompt=%q want=%q", got, test.want)
		}
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
	want := "[Replying to Alice: \"look at this\" with image; unsupported attachment: doc.pdf]\n\nwhat is it?\n\n[User sent an unsupported attachment: notes.txt. It was not saved. You cannot read this file type — ask the user to paste its contents or resend it in a supported format (pdf, txt, md, csv, docx, xlsx, or an image).]"
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

func TestBuildPromptIncludesDocuments(t *testing.T) {
	docs := []media.DocumentRef{{Path: "/private/a.pdf", Filename: "a.pdf", MIMEType: "application/pdf"}}
	if got := BuildPrompt("summarize", nil, nil, nil, docs); got != "summarize\n\n[The user sent a document: 'a.pdf'. It is saved at: /private/a.pdf. Its text is not inlined (binary format). Extract the document text with read_file before answering, instead of asking the user to paste the contents.]" {
		t.Fatalf("unexpected prompt %q", got)
	}
}

func TestDecideAcceptsDocumentOnlyPrompt(t *testing.T) {
	docs := []media.DocumentRef{{Path: "/private/a.txt", Filename: "a.txt", MIMEType: "text/plain"}}
	decision := Decide(Input{Text: " ", CurrentDocuments: docs})
	if decision.Action != ActionLLM {
		t.Fatalf("expected llm, got %q", decision.Action)
	}
	if len(decision.Documents) != 1 {
		t.Fatalf("expected 1 document, got %d", len(decision.Documents))
	}
	if !strings.Contains(decision.Prompt, "/private/a.txt") {
		t.Fatalf("prompt missing document path: %q", decision.Prompt)
	}
}

func TestCombineDocumentsCapsAtMax(t *testing.T) {
	var current []media.DocumentRef
	for i := 0; i < media.MaxDocumentsPerRequest+2; i++ {
		current = append(current, media.DocumentRef{Path: "/private/a.txt"})
	}
	if got := combineDocuments(current, nil); len(got) != media.MaxDocumentsPerRequest {
		t.Fatalf("expected cap %d, got %d", media.MaxDocumentsPerRequest, len(got))
	}
}
