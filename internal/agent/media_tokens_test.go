package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

func TestStripMediaTokens(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"none", "Just a reply.", "Just a reply."},
		{"inline", "Here you go MEDIA:/cache/img_1.webp thanks", "Here you go thanks"},
		{"trailing period kept as text", "See MEDIA:/cache/img_1.webp.", "See"},
		{"only token", "MEDIA:/cache/img_1.webp", ""},
		{"relative untouched", "Run MEDIA:rebuild now", "Run MEDIA:rebuild now"},
		{"bare keyword untouched", "MEDIA: sent", "MEDIA: sent"},
	} {
		if got := collapseStrippedWhitespace(mediaTokenPattern.ReplaceAllString(tc.input, "")); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func mediaTestImage(t *testing.T, a *Agent, user string) (string, []byte) {
	t.Helper()
	input := testInputImage(t, 3, 2)
	data, err := base64.StdEncoding.DecodeString(input.Data)
	if err != nil {
		t.Fatal(err)
	}
	path, err := a.imageCache.Save(context.Background(), user, data, input.MimeType)
	if err != nil {
		t.Fatal(err)
	}
	return path, data
}

func TestMediaTokenAttachesOwnedImage(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, fixture := newTestAgent(t, chat, nil, reg)
	path, data := mediaTestImage(t, a, "user-1")
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Here it is MEDIA:" + path + " let me know"}}}
	response, err := processAgent(a, "req-1", "discord", "discord:dm:123", "user-1", "User", "Send it back", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Attachments) != 1 || string(response.Attachments[0].Data) != string(data) {
		t.Fatalf("attachments=%d", len(response.Attachments))
	}
	if strings.Contains(response.Response, "MEDIA:") {
		t.Fatalf("token leaked into response: %q", response.Response)
	}
	if response.SourceTurnID == 0 {
		t.Fatal("media turn was not persisted")
	}
	transcript, _, _, err := fixture.stores["user-1"].SessionTranscript(context.Background(), "user-1", sessionKeyToID(t, fixture.stores["user-1"], "user-1", "discord:dm:123"), 20, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range transcript {
		if strings.Contains(message.Content, "MEDIA:") {
			t.Fatalf("token persisted into history: %q", message.Content)
		}
	}
}

func TestMediaTokenRejectsForeignAndArbitraryPaths(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	owned, _ := mediaTestImage(t, a, "user-1")
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Yours MEDIA:" + owned + " mine"}}}
	response, err := processAgent(a, "req-1", "discord", "discord:dm:123", "user-2", "User", "Send it", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Attachments) != 0 || strings.Contains(response.Response, "MEDIA:") {
		t.Fatalf("foreign cache image attached: %+v", response)
	}

	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Secrets MEDIA:/etc/passwd here"}}}
	response, err = processAgent(a, "req-2", "discord", "discord:dm:123", "user-1", "User", "Leak", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Attachments) != 0 || strings.Contains(response.Response, "MEDIA:") {
		t.Fatalf("arbitrary path attached: %+v", response)
	}
}

func TestMediaTokenAttachesCWDImage(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	ws := t.TempDir()
	input := testInputImage(t, 3, 2)
	data, err := base64.StdEncoding.DecodeString(input.Data)
	if err != nil {
		t.Fatal(err)
	}
	imgPath := filepath.Join(ws, "spank_bank_1.jpg")
	if err := os.WriteFile(imgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	a.SetWorkspace(ws)
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Here it is MEDIA:" + imgPath + " enjoy"}}}
	response, err := processAgent(a, "req-1", "discord", "discord:dm:123", "user-1", "User", "Send it", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Attachments) != 1 || string(response.Attachments[0].Data) != string(data) {
		t.Fatalf("attachments=%d", len(response.Attachments))
	}
	if strings.Contains(response.Response, "MEDIA:") {
		t.Fatalf("token leaked into response: %q", response.Response)
	}
}

func TestMediaTokenRejectsOutsideAndSecretCWDPaths(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	ws := t.TempDir()
	input := testInputImage(t, 3, 2)
	data, err := base64.StdEncoding.DecodeString(input.Data)
	if err != nil {
		t.Fatal(err)
	}
	// Valid control images inside the workspace.
	good := filepath.Join(ws, "ok.jpg")
	if err := os.WriteFile(good, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// System-managed paths with image content attach: same-profile state
	// and soul basenames are readable, but the extension gate below still
	// requires a real image extension.
	stateImg := filepath.Join(ws, "state.db")
	if err := os.WriteFile(stateImg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	memImg := filepath.Join(ws, "memories", "pic.jpg")
	if err := os.MkdirAll(filepath.Dir(memImg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memImg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	crossDir := filepath.Join(ws, "profiles", "other")
	if err := os.MkdirAll(crossDir, 0o700); err != nil {
		t.Fatal(err)
	}
	crossImg := filepath.Join(crossDir, "pic.jpg")
	if err := os.WriteFile(crossImg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Secret and cache-managed paths stay denied; the extension gate below
	// still requires a real image extension regardless of group.
	for _, name := range []string{".env", "SOUL.md"} {
		if err := os.WriteFile(filepath.Join(ws, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	managed := filepath.Join(ws, "cache", "images")
	if err := os.MkdirAll(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	managedImg := filepath.Join(managed, "managed.jpg")
	if err := os.WriteFile(managedImg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(ws, "cache", "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	scratchImg := filepath.Join(scratch, "tmp.jpg")
	if err := os.WriteFile(scratchImg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Non-image content with an image extension must fail MIME validation.
	if err := os.WriteFile(filepath.Join(ws, "note.jpg"), []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.SetWorkspace(ws)
	for _, tc := range []struct {
		name   string
		path   string
		attach bool
	}{
		{"control", good, true},
		{"scratch", scratchImg, true},
		{"secret env", filepath.Join(ws, ".env"), false},
		{"state db extension gate", filepath.Join(ws, "state.db"), false},
		{"soul extension gate", filepath.Join(ws, "SOUL.md"), false},
		{"system memories image", memImg, true},
		{"cross-profile image", crossImg, true},
		{"managed cache bypass", managedImg, false},
		{"non image", filepath.Join(ws, "note.jpg"), false},
		{"outside cwd", "/etc/passwd", false},
		{"escape", filepath.Join(ws, "..", "outside.jpg"), false},
	} {
		stripped, attached := a.resolveResponseMedia(context.Background(), config.NewLogger(config.LevelError), "discord", "user-1", "See MEDIA:"+tc.path, nil)
		if strings.Contains(stripped, "MEDIA:") {
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

func TestMediaTokenDedupesAndStripsForAPI(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	path, _ := mediaTestImage(t, a, "user-1")
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "One MEDIA:" + path + " two MEDIA:" + path}}}
	response, err := processAgent(a, "req-1", "discord", "discord:dm:123", "user-1", "User", "Twice", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Attachments) != 1 {
		t.Fatalf("duplicate path attached %d times", len(response.Attachments))
	}

	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "One MEDIA:" + path}}}
	response, err = a.Process(context.Background(), Request{
		RequestID:     "req-2",
		Principal:     identity.Principal{CanonicalUserID: "user-1", Gateway: "openai", ExternalID: identity.LocalOpenAIIdentifier, Assurance: identity.AssuranceLocalLoopback},
		SessionKey:    "session",
		Prompt:        "API",
		Stateless:     true,
		ClientHistory: []llm.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Attachments) != 0 || strings.Contains(response.Response, "MEDIA:") {
		t.Fatalf("API response kept token or attachments: %+v", response)
	}
}

func TestMediaTokenRespectsAttachmentCaps(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	path, _ := mediaTestImage(t, a, "user-1")
	existing := make([]media.OutputAttachment, 0, media.MaxOutputAttachments)
	for i := 0; i < media.MaxOutputAttachments; i++ {
		existing = append(existing, media.OutputAttachment{Filename: string(rune('a'+i)) + ".bin", MIMEType: "application/octet-stream", Data: []byte{byte(i)}})
	}
	stripped, attached := a.resolveResponseMedia(context.Background(), config.NewLogger(config.LevelError), "discord", "user-1", "Full MEDIA:"+path, existing)
	if stripped != "Full" || len(attached) != 0 {
		t.Fatalf("over-cap media attached: %q %+v", stripped, attached)
	}
}

func sessionKeyToID(t *testing.T, store *memory.ProfileStore, owner, key string) string {
	t.Helper()
	session, err := store.ResolveSessionContext(context.Background(), owner, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.ActiveSessionID(context.Background(), owner, key, session.Generation)
	if err != nil || id == "" {
		t.Fatalf("active session id: %v", err)
	}
	return id
}
