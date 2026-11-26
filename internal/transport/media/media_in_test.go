package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMediaPredicates(t *testing.T) {
	for _, mime := range []string{"video/mp4", "video/webm", "video/quicktime", "image/gif", "audio/ogg", "audio/mp4", "IMAGE/GIF; charset=x"} {
		if !LooksLikeMediaMIME(mime) {
			t.Fatalf("LooksLikeMediaMIME(%q) = false", mime)
		}
	}
	for _, mime := range []string{"image/jpeg", "image/png", "application/pdf", "text/plain", ""} {
		if LooksLikeMediaMIME(mime) {
			t.Fatalf("LooksLikeMediaMIME(%q) = true", mime)
		}
	}
	for name, want := range map[string]bool{
		"clip.mp4": true, "clip.MOV": true, "a.webm": true, "a.gif": true, "a.m4v": true, "a.mkv": true,
		"voice.m4a": true, "a.mp3": true, "a.ogg": true, "a.opus": true, "a.wav": true, "a.flac": true, "a.caf": true,
		"notes.txt": false, "photo.jpg": false, "bundle.zip": false, "noext": false,
	} {
		if got := IsMediaExtension(name); got != want {
			t.Fatalf("IsMediaExtension(%q) = %t, want %t", name, got, want)
		}
	}
	for name, want := range map[string]bool{
		"photo.jpg": true, "a.png": true, "a.bmp": true, "a.tiff": true, "a.heic": true,
		"a.gif": false, "a.mp4": false, "a.zip": false, "noext": false,
	} {
		if got := LooksLikeImageExtension(name); got != want {
			t.Fatalf("LooksLikeImageExtension(%q) = %t, want %t", name, got, want)
		}
	}
}

func TestSniffMedia(t *testing.T) {
	data := []byte("synthetic media bytes")
	name, mime, err := SniffMedia("clip.mp4", "video/mp4", data)
	if err != nil || name != "clip.mp4" || mime != "video/mp4" {
		t.Fatalf("declared mp4 = %q %q %v", name, mime, err)
	}
	name, mime, err = SniffMedia("funny.gif", "", data)
	if err != nil || name != "funny.gif" || mime != "image/gif" {
		t.Fatalf("extension gif = %q %q %v", name, mime, err)
	}
	name, mime, err = SniffMedia("clip", "video/webm", data)
	if err != nil || name != "clip.webm" || mime != "video/webm" {
		t.Fatalf("extensionless webm = %q %q %v", name, mime, err)
	}
	name, mime, err = SniffMedia("voice.m4a", "", data)
	if err != nil || name != "voice.m4a" || mime != "audio/mp4" {
		t.Fatalf("audio m4a = %q %q %v", name, mime, err)
	}
	name, mime, err = SniffMedia("voice", "audio/ogg", data)
	if err != nil || name != "voice.ogg" || mime != "audio/ogg" {
		t.Fatalf("declared ogg = %q %q %v", name, mime, err)
	}
	if _, _, err := SniffMedia("notes.txt", "", data); err == nil {
		t.Fatal("text file accepted as media")
	}
	if _, _, err := SniffMedia("clip.mp4", "video/mp4", nil); err == nil {
		t.Fatal("empty payload accepted")
	}
	if _, _, err := SniffMedia("clip.mp4", "video/mp4", make([]byte, MaxMediaBytes+1)); err == nil {
		t.Fatal("oversize payload accepted")
	}
}

func TestSniffAttachment(t *testing.T) {
	data := []byte("PK\x03\x04 synthetic zip")
	name, mime, err := SniffAttachment("bundle.zip", "application/zip", data)
	if err != nil || name != "bundle.zip" || mime != "application/zip" {
		t.Fatalf("zip = %q %q %v", name, mime, err)
	}
	// Unknown declarations sniff the payload instead.
	name, mime, err = SniffAttachment("bundle", "", data)
	if err != nil || name != "bundle" || mime == "" {
		t.Fatalf("sniffed = %q %q %v", name, mime, err)
	}
	// Unsafe extensions are dropped, not persisted.
	name, _, err = SniffAttachment("weird.name!", "", data)
	if err != nil || strings.Contains(name, "!") {
		t.Fatalf("unsafe name = %q %v", name, err)
	}
	if _, _, err := SniffAttachment("big.zip", "application/zip", make([]byte, MaxMediaBytes+1)); err == nil {
		t.Fatal("oversize attachment accepted")
	}
}

func TestProbeMediaParsesFFprobeJSON(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffprobe")
	body := "#!/bin/sh\nprintf '%s' '{\"streams\":[{\"width\":720,\"height\":1280}],\"format\":{\"duration\":\"12.4\"}}'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	duration, width, height := ProbeMedia(context.Background(), script, "unused.mp4")
	if duration != 12.4 || width != 720 || height != 1280 {
		t.Fatalf("probe = %v %dx%d", duration, width, height)
	}
	// Missing binary degrades to zeros instead of failing admission.
	if duration, width, height := ProbeMedia(context.Background(), filepath.Join(dir, "missing"), "x"); duration != 0 || width != 0 || height != 0 {
		t.Fatalf("missing ffprobe = %v %dx%d", duration, width, height)
	}
}

func TestMediaNotes(t *testing.T) {
	video := DocumentRef{Path: "/cache/media/ab.mp4", Filename: "clip.mp4", MIMEType: "video/mp4", Lane: LaneMedia, DurationSeconds: 12.6, Width: 720, Height: 1280}
	want := "[Video attached at: /cache/media/ab.mp4 ('clip.mp4', 0:13, 720×1280)]"
	if got := DocumentContextNote(video); got != want {
		t.Fatalf("media note = %q, want %q", got, want)
	}
	gif := DocumentRef{Path: "/cache/media/cd.gif", Filename: "funny.gif", MIMEType: "image/gif", Lane: LaneMedia}
	if got := DocumentContextNote(gif); got != "[GIF attached at: /cache/media/cd.gif ('funny.gif')]" {
		t.Fatalf("gif note = %q", got)
	}
	audio := DocumentRef{Path: "/cache/media/ef.m4a", Filename: "voice.m4a", MIMEType: "audio/mp4", Lane: LaneMedia, DurationSeconds: 7.2}
	if got := DocumentContextNote(audio); got != "[Audio attached at: /cache/media/ef.m4a ('voice.m4a', 0:07)]" {
		t.Fatalf("audio note = %q", got)
	}
	attachment := DocumentRef{Path: "/cache/media/gh.zip", Filename: "bundle.zip", MIMEType: "application/zip", Lane: LaneAttachment}
	if got := DocumentContextNote(attachment); got != "[Attachment cached at: /cache/media/gh.zip ('bundle.zip')]" {
		t.Fatalf("attachment note = %q", got)
	}

	doc := DocumentRef{Path: "/cache/documents/x.pdf", Filename: "x.pdf", MIMEType: "application/pdf"}
	audioDoc := DocumentRef{Path: "/cache/media/ef.m4a", Filename: "voice.m4a", MIMEType: "audio/mp4", Lane: LaneMedia, DurationSeconds: 7.2}
	note := DocumentsNote([]DocumentRef{doc, audioDoc})
	if !strings.Contains(note, "[File attached at: /cache/documents/x.pdf ('x.pdf')]") {
		t.Fatalf("document note missing: %q", note)
	}
	if !strings.Contains(note, MediaRecipeNote) {
		t.Fatalf("media recipe missing: %q", note)
	}
	if !strings.Contains(MediaRecipeNote, "cannot listen") {
		t.Fatalf("media recipe must state the audio limitation: %q", MediaRecipeNote)
	}
	if got := DocumentsNote([]DocumentRef{doc, video}); strings.Contains(got, MediaRecipeNote) {
		t.Fatalf("recipe injected for video-only turn: %q", got)
	}
	if got := DocumentsNote([]DocumentRef{doc, attachment}); strings.Contains(got, MediaRecipeNote) {
		t.Fatalf("recipe injected without AV media: %q", got)
	}
}
