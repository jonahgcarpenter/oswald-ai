package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

func TestStripFileTokens(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"none", "Just a reply.", "Just a reply."},
		{"inline", "Here you go FILE:/tmp/report.pdf thanks", "Here you go thanks"},
		{"trailing period kept as text", "See FILE:/tmp/report.pdf.", "See"},
		{"only token", "FILE:/tmp/report.pdf", ""},
		{"relative untouched", "Run FILE:rebuild now", "Run FILE:rebuild now"},
		{"bare keyword untouched", "FILE: sent", "FILE: sent"},
	} {
		if got := collapseStrippedWhitespace(fileTokenPattern.ReplaceAllString(tc.input, "")); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestFileTokenAttachesCWDFile(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	ws := t.TempDir()
	a.SetWorkspace(ws)
	content := []byte("hello file world")
	filePath := filepath.Join(ws, "report.txt")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Here it is FILE:" + filePath + " enjoy"}}}
	response, err := processAgent(a, "req-1", "discord", "discord:dm:123", "user-1", "User", "Send it", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Attachments) != 1 || string(response.Attachments[0].Data) != string(content) {
		t.Fatalf("attachments=%d", len(response.Attachments))
	}
	if strings.Contains(response.Response, "FILE:") {
		t.Fatalf("token leaked into response: %q", response.Response)
	}
}

func TestFileTokenRejectsProtectedAndManagedPaths(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	ws := t.TempDir()
	a.SetWorkspace(ws)
	good := filepath.Join(ws, "notes.md")
	if err := os.WriteFile(good, []byte("# notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	scratchDir := filepath.Join(ws, "cache", "scratch")
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(scratchDir, "tmp.txt")
	if err := os.WriteFile(scratch, []byte("scratch"), 0o600); err != nil {
		t.Fatal(err)
	}
	managedDir := filepath.Join(ws, "cache", "images")
	if err := os.MkdirAll(managedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(managedDir, "managed.txt")
	if err := os.WriteFile(managed, []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Same-trust model: outside-workspace non-protected files may attach.
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", "SOUL.md"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		path   string
		attach bool
	}{
		{"control", good, true},
		{"scratch", scratch, true},
		{"secret env", filepath.Join(ws, ".env"), false},
		{"managed cache bypass", managed, false},
		{"outside same-trust", outside, true},
		{"protected proc", "/proc/self/environ", false},
		{"escape missing", filepath.Join(ws, "..", "outside.txt"), false},
	} {
		stripped, attached := a.resolveResponseMedia(context.Background(), config.NewLogger(config.LevelError), "discord", "user-1", "See FILE:"+tc.path, nil)
		if strings.Contains(stripped, "FILE:") {
			t.Fatalf("%s: token leaked: %q", tc.name, stripped)
		}
		if tc.attach && len(attached) != 1 {
			t.Fatalf("%s: expected attachment, got %d", tc.name, len(attached))
		}
		if !tc.attach && len(attached) != 0 {
			t.Fatalf("%s: expected drop, got %d attachments", tc.name, len(attached))
		}
	}
}

func TestFileAndMediaShareDedupeAndCaps(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	ws := t.TempDir()
	a.SetWorkspace(ws)
	data := []byte("shared content")
	shared := filepath.Join(ws, "shared.txt")
	if err := os.WriteFile(shared, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Same path via both directives attaches once.
	stripped, attached := a.resolveResponseMedia(context.Background(), config.NewLogger(config.LevelError), "discord", "user-1", "One FILE:"+shared+" two FILE:"+shared, nil)
	if stripped != "One two" || len(attached) != 1 {
		t.Fatalf("dedupe failed: %q %+v", stripped, attached)
	}

	// Over-cap drops the file token.
	existing := make([]media.OutputAttachment, 0, media.MaxOutputAttachments)
	for i := 0; i < media.MaxOutputAttachments; i++ {
		existing = append(existing, media.OutputAttachment{Filename: string(rune('a'+i)) + ".bin", MIMEType: "application/octet-stream", Data: []byte{byte(i)}})
	}
	stripped, attached = a.resolveResponseMedia(context.Background(), config.NewLogger(config.LevelError), "discord", "user-1", "Full FILE:"+shared, existing)
	if stripped != "Full" || len(attached) != 0 {
		t.Fatalf("over-cap file attached: %q %+v", stripped, attached)
	}
}

func TestFileTokenStrippedForAPI(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	ws := t.TempDir()
	a.SetWorkspace(ws)
	filePath := filepath.Join(ws, "api.txt")
	if err := os.WriteFile(filePath, []byte("api"), 0o600); err != nil {
		t.Fatal(err)
	}
	stripped, attached := a.resolveResponseMedia(context.Background(), config.NewLogger(config.LevelError), "openai", "user-1", "See FILE:"+filePath, nil)
	if strings.Contains(stripped, "FILE:") || len(attached) != 0 {
		t.Fatalf("API response kept token or attachments: %q %+v", stripped, attached)
	}
}
