package agent

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
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
