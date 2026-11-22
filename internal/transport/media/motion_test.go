package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMotionPredicates(t *testing.T) {
	for _, mime := range []string{"video/mp4", "video/webm", "video/quicktime", "image/gif", "IMAGE/GIF; charset=x"} {
		if !LooksLikeMotionMIME(mime) {
			t.Fatalf("LooksLikeMotionMIME(%q) = false", mime)
		}
	}
	for _, mime := range []string{"image/jpeg", "image/png", "application/pdf", ""} {
		if LooksLikeMotionMIME(mime) {
			t.Fatalf("LooksLikeMotionMIME(%q) = true", mime)
		}
	}
	for name, want := range map[string]bool{
		"clip.mp4": true, "clip.MOV": true, "a.webm": true, "a.gif": true, "a.m4v": true, "a.mkv": true,
		"notes.txt": false, "photo.jpg": false, "noext": false,
	} {
		if got := IsMotionExtension(name); got != want {
			t.Fatalf("IsMotionExtension(%q) = %t, want %t", name, got, want)
		}
	}
}

func TestSniffMotion(t *testing.T) {
	data := []byte("synthetic motion bytes")
	name, mime, err := SniffMotion("clip.mp4", "video/mp4", data)
	if err != nil || name != "clip.mp4" || mime != "video/mp4" {
		t.Fatalf("declared mp4 = %q %q %v", name, mime, err)
	}
	name, mime, err = SniffMotion("funny.gif", "", data)
	if err != nil || name != "funny.gif" || mime != "image/gif" {
		t.Fatalf("extension gif = %q %q %v", name, mime, err)
	}
	name, mime, err = SniffMotion("clip", "video/webm", data)
	if err != nil || name != "clip.webm" || mime != "video/webm" {
		t.Fatalf("extensionless webm = %q %q %v", name, mime, err)
	}
	if _, _, err := SniffMotion("notes.txt", "", data); err == nil {
		t.Fatal("text file accepted as motion")
	}
	if _, _, err := SniffMotion("clip.mp4", "video/mp4", nil); err == nil {
		t.Fatal("empty payload accepted")
	}
	if _, _, err := SniffMotion("clip.mp4", "video/mp4", make([]byte, MaxMotionBytes+1)); err == nil {
		t.Fatal("oversize payload accepted")
	}
}

func TestProbeMotionParsesFFprobeJSON(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffprobe")
	body := "#!/bin/sh\nprintf '%s' '{\"streams\":[{\"width\":720,\"height\":1280}],\"format\":{\"duration\":\"12.4\"}}'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	duration, width, height := ProbeMotion(context.Background(), script, "unused.mp4")
	if duration != 12.4 || width != 720 || height != 1280 {
		t.Fatalf("probe = %v %dx%d", duration, width, height)
	}
	// Missing binary degrades to zeros instead of failing admission.
	if duration, width, height := ProbeMotion(context.Background(), filepath.Join(dir, "missing"), "x"); duration != 0 || width != 0 || height != 0 {
		t.Fatalf("missing ffprobe = %v %dx%d", duration, width, height)
	}
}

func TestMotionNotes(t *testing.T) {
	video := DocumentRef{Path: "/cache/videos/ab.mp4", Filename: "clip.mp4", MIMEType: "video/mp4", DurationSeconds: 12.6, Width: 720, Height: 1280}
	want := "[Video attached at: /cache/videos/ab.mp4 ('clip.mp4', 0:13, 720×1280)]"
	if got := DocumentContextNote(video); got != want {
		t.Fatalf("motion note = %q, want %q", got, want)
	}
	gif := DocumentRef{Path: "/cache/videos/cd.gif", Filename: "funny.gif", MIMEType: "image/gif"}
	if got := DocumentContextNote(gif); got != "[GIF attached at: /cache/videos/cd.gif ('funny.gif')]" {
		t.Fatalf("gif note = %q", got)
	}
	doc := DocumentRef{Path: "/cache/documents/x.pdf", Filename: "x.pdf", MIMEType: "application/pdf"}
	note := DocumentsNote([]DocumentRef{doc, video})
	if !strings.Contains(note, "[File attached at: /cache/documents/x.pdf ('x.pdf')]") {
		t.Fatalf("document note missing: %q", note)
	}
	if !strings.Contains(note, MotionRecipeNote) {
		t.Fatalf("motion recipe missing: %q", note)
	}
	if got := DocumentsNote([]DocumentRef{doc}); strings.Contains(got, MotionRecipeNote) {
		t.Fatalf("recipe injected without motion media: %q", got)
	}
}
