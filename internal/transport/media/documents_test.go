package media

import (
	"strings"
	"testing"
)

func TestDocumentLabel(t *testing.T) {
	if got := DocumentLabel("report.pdf", "application/pdf"); got != "report.pdf" {
		t.Fatalf("label = %q", got)
	}
	if got := DocumentLabel("", "text/plain"); got != "text/plain" {
		t.Fatalf("label = %q", got)
	}
	if got := DocumentLabel("", ""); got != "unknown file" {
		t.Fatalf("label = %q", got)
	}
}

func TestIsExtractableDocument(t *testing.T) {
	for _, name := range []string{"a.pdf", "b.DOCX", "c.xlsx", "d.txt", "e.md", "f.csv", "g.log", "h.json"} {
		if !IsExtractableDocument(name) {
			t.Fatalf("%s should be extractable", name)
		}
	}
	for _, name := range []string{"a.png", "b.zip", "c.mp3", "d", ""} {
		if IsExtractableDocument(name) {
			t.Fatalf("%s should not be extractable", name)
		}
	}
}

func TestLooksLikeDocumentMIME(t *testing.T) {
	if !LooksLikeDocumentMIME("application/pdf") {
		t.Fatal("pdf should look like a document")
	}
	if !LooksLikeDocumentMIME("") {
		t.Fatal("empty MIME stays eligible for extension sniffing")
	}
	for _, mime := range []string{"image/png", "audio/ogg", "video/mp4"} {
		if LooksLikeDocumentMIME(mime) {
			t.Fatalf("%s should not look like a document", mime)
		}
	}
}

func TestSniffDocument(t *testing.T) {
	name, mime, err := SniffDocument("notes.txt", "text/plain", []byte("hello"))
	if err != nil || name != "notes.txt" || !strings.HasPrefix(mime, "text/") {
		t.Fatalf("sniff = %q %q %v", name, mime, err)
	}
	if _, _, err := SniffDocument("photo.png", "image/png", []byte{0x89, 'P', 'N', 'G', 0, 0, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("image bytes should be rejected")
	}
	if _, _, err := SniffDocument("tool.exe", "application/octet-stream", []byte("MZ binary")); err == nil {
		t.Fatal("unsupported extension should be rejected")
	}
	if _, _, err := SniffDocument("big.pdf", "application/pdf", make([]byte, MaxDocumentBytes+1)); err == nil {
		t.Fatal("oversize should be rejected")
	}
}

func TestDocumentsNote(t *testing.T) {
	docs := []DocumentRef{
		{Path: "/private/a.pdf", Filename: "a.pdf", MIMEType: "application/pdf"},
		{Path: "/private/b.txt", Filename: "b.txt", MIMEType: "text/plain"},
	}
	note := DocumentsNote(docs)
	if !strings.Contains(note, "/private/a.pdf") || !strings.Contains(note, "/private/b.txt") {
		t.Fatalf("note missing paths:\n%s", note)
	}
	if !strings.Contains(note, "read_file") {
		t.Fatalf("note should direct the model to read_file:\n%s", note)
	}
	if got := DocumentsNote(nil); got != "" {
		t.Fatalf("empty docs should yield empty note, got %q", got)
	}
}
