package discord

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/privacyruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestDiscordHandleDirectMessageSendsReply(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()

	dg.handleMessage(discordMessage("msg-1", "channel-1", "", "123", "Alice", "hello discord"))

	primary := primaryDiscordRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	last := primary[0].Messages[len(primary[0].Messages)-1]
	if last.Content != "hello discord" || !strings.Contains(primary[0].Messages[len(primary[0].Messages)-2].Content, "<tenant_profile") {
		t.Fatalf("unexpected prompt %q", last.Content)
	}
	if rest.lastMessageContent() != "discord response" {
		t.Fatalf("unexpected sent messages: %+v", rest.sentMessages())
	}
	if _, ok := dg.lookupReply("sent-1"); !ok {
		t.Fatal("expected sent bot reply remembered")
	}
	principal := chat.lastPrincipal()
	if principal.CanonicalUserID == "" || principal.Gateway != "discord" || principal.ExternalID != "123" || principal.Assurance != identity.AssuranceDiscordGateway {
		t.Fatalf("unexpected principal: %+v", principal)
	}
}

func TestPrivacyInvalidationPurgesOnlyMatchingDiscordReplyContext(t *testing.T) {
	dg := &Gateway{replyIndex: map[string]replyContext{
		"session": {SessionKey: "discord:channel:one", SenderID: "one"},
		"sender":  {SessionKey: "discord:other:one", SenderID: "one"},
		"foreign": {SessionKey: "discord:channel:two", SenderID: "two"},
	}}
	dg.HandlePrivacyInvalidation(privacyruntime.Event{SessionIDs: []string{"discord:channel:one"}, ExternalIdentities: []string{"discord:one", "imessage:one"}})
	if _, ok := dg.replyIndex["session"]; ok {
		t.Fatal("matching session reply context remained")
	}
	if _, ok := dg.replyIndex["sender"]; ok {
		t.Fatal("matching sender reply context remained")
	}
	if _, ok := dg.replyIndex["foreign"]; !ok || len(dg.replyIndex) != 1 {
		t.Fatalf("foreign reply context was purged: %+v", dg.replyIndex)
	}
}

func TestDiscordCommandAttachmentMultipart(t *testing.T) {
	var payloads []map[string]any
	var filenames, mimeTypes []string
	var attachmentData [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channels/channel-1/messages" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		var payload map[string]any
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(commands.MaxAttachmentBytes + 4096); err != nil {
				t.Fatalf("parse multipart: %v", err)
			}
			if err := json.Unmarshal([]byte(r.FormValue("payload_json")), &payload); err != nil {
				t.Fatalf("decode payload_json: %v", err)
			}
			file, header, err := r.FormFile("files[0]")
			if err != nil {
				t.Fatalf("read file part: %v", err)
			}
			data, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil {
				t.Fatal(err)
			}
			filenames = append(filenames, header.Filename)
			mimeTypes = append(mimeTypes, header.Header.Get("Content-Type"))
			attachmentData = append(attachmentData, data)
		} else {
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode message payload: %v", err)
			}
		}
		payloads = append(payloads, payload)
		_, _ = w.Write([]byte(`{"id":"sent-attachment"}`))
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError)}
	responder := runtimeResponder{gateway: dg, channelID: "channel-1", replyToID: "message-1"}
	err := responder.SendCommandResponse(commands.Result{Text: "export ready", Attachments: []commands.Attachment{
		{Filename: "export.json.part001", MIMEType: "application/octet-stream", Data: []byte("first")},
		{Filename: "export.json.part002", MIMEType: "application/octet-stream", Data: []byte("second")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 3 {
		t.Fatalf("request payload count=%d, want 3", len(payloads))
	}
	reference, _ := payloads[0]["message_reference"].(map[string]any)
	firstMetadata, _ := payloads[0]["attachments"].([]any)
	secondMetadata, _ := payloads[1]["attachments"].([]any)
	if reference["message_id"] != "message-1" || len(firstMetadata) != 1 || len(secondMetadata) != 1 || payloads[2]["content"] != "export ready" || len(filenames) != 2 || filenames[0] != "export.json.part001" || filenames[1] != "export.json.part002" || mimeTypes[0] != "application/octet-stream" || string(attachmentData[0]) != "first" || string(attachmentData[1]) != "second" {
		t.Fatalf("unexpected payloads=%+v filenames=%q mime=%q data=%q", payloads, filenames, mimeTypes, attachmentData)
	}
}

func TestDiscordCommandAttachmentProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private-content", http.StatusBadGateway)
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError)}
	_, err := dg.sendCommandAttachment("channel-1", commands.Result{Attachment: &commands.Attachment{
		Filename: "export.json", MIMEType: "application/json", Data: []byte("private-content"),
	}}, "")
	if err == nil || strings.Contains(err.Error(), "private-content") {
		t.Fatalf("unexpected provider error: %v", err)
	}
}

func TestDiscordIgnoresUnmentionedGuildMessage(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()

	dg.handleMessage(discordMessage("msg-1", "channel-1", "guild-1", "123", "Alice", "hello guild"))

	if len(primaryDiscordRequests(chat.requests)) != 0 {
		t.Fatalf("expected no LLM request, got %d", len(primaryDiscordRequests(chat.requests)))
	}
	if len(rest.sentMessages()) != 0 {
		t.Fatalf("expected no sent messages, got %+v", rest.sentMessages())
	}
}

func TestDiscordMentionedGuildMessageStripsMentionAndResolvesMentions(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()

	msg := discordMessage("msg-1", "channel-1", "guild-1", "123", "Alice", "<@bot-1> hello <@456>")
	msg.Mentions = []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}{{ID: "456", Username: "Bob"}}
	dg.handleMessage(msg)

	primary := primaryDiscordRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	prompt := primary[0].Messages[len(primary[0].Messages)-1].Content
	if prompt != "hello @Bob" || !strings.Contains(primary[0].Messages[len(primary[0].Messages)-2].Content, "<tenant_profile") {
		t.Fatalf("unexpected prompt %q", prompt)
	}
}

func TestDiscordReplyToBotIncludesReplyContext(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()
	dg.rememberReply("bot-msg", replyContext{SessionKey: "discord:channel-1:123", ChannelID: "channel-1", DisplayName: "Oswald", Text: "prior answer", IsFromBot: true, CreatedAt: time.Now()})

	msg := discordMessage("msg-2", "channel-1", "guild-1", "123", "Alice", "follow up")
	msg.ReferencedMessage = &struct {
		ID          string       `json:"id"`
		Content     string       `json:"content"`
		Attachments []Attachment `json:"attachments,omitempty"`
		Embeds      []Embed      `json:"embeds,omitempty"`
		Author      struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"author"`
	}{ID: "bot-msg"}
	msg.ReferencedMessage.Author.ID = "bot-1"
	msg.ReferencedMessage.Author.Username = "Oswald"
	dg.handleMessage(msg)

	primary := primaryDiscordRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	prompt := primary[0].Messages[len(primary[0].Messages)-1].Content
	if !strings.Contains(prompt, "[Replying to Oswald: \"prior answer\"]") || !strings.Contains(prompt, "follow up") {
		t.Fatalf("unexpected prompt %q", prompt)
	}
}

func TestDiscordHelpers(t *testing.T) {
	chunks := splitMessage("one. two three\nfour", 10)
	if len(chunks) != 3 || chunks[0] != "one." || chunks[1] != "two three" || chunks[2] != "four" {
		t.Fatalf("unexpected chunks: %+v", chunks)
	}
	mentions := []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}{{ID: "123", Username: "Alice"}}
	if got := resolveMentions("hi <@123> and <@!999>", mentions); got != "hi @Alice and <@!999>" {
		t.Fatalf("unexpected resolved mentions %q", got)
	}
	if resolveGatewayURL("gateway.discord.gg") != "wss://gateway.discord.gg/?v=10&encoding=json" {
		t.Fatal("unexpected resolved gateway URL")
	}
}

func TestDiscordGIFVEmbedPrefersAnimatedVideo(t *testing.T) {
	videoPayload := []byte("mock mp4")
	requestedThumbnail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video.mp4":
			_, _ = w.Write(videoPayload)
		case "/thumbnail.jpg":
			requestedThumbnail = true
			_, _ = w.Write(testDiscordJPEG(t))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	extractor := &fakeVideoFrameExtractor{image: llm.InputImage{MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(testDiscordJPEG(t))}}
	dg := &Gateway{HTTPClient: server.Client(), VideoFrames: extractor, Log: config.NewLogger(config.LevelError)}
	embed := Embed{
		Type:      "gifv",
		Video:     EmbedImage{ProxyURL: server.URL + "/video.mp4"},
		Thumbnail: EmbedImage{ProxyURL: server.URL + "/thumbnail.jpg"},
	}

	images, unsupported := dg.loadEmbedImagesLimit([]Embed{embed}, 1)
	if len(images) != 1 || len(unsupported) != 0 {
		t.Fatalf("images=%d unsupported=%v", len(images), unsupported)
	}
	if !bytes.Equal(extractor.data, videoPayload) {
		t.Fatalf("extractor payload = %q, want %q", extractor.data, videoPayload)
	}
	if requestedThumbnail {
		t.Fatal("static thumbnail was requested after successful video extraction")
	}
}

func TestDiscordGIFVEmbedFallsBackToStaticThumbnail(t *testing.T) {
	requestedThumbnail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video.mp4":
			_, _ = w.Write([]byte("mock mp4"))
		case "/thumbnail.jpg":
			requestedThumbnail = true
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(testDiscordJPEG(t))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dg := &Gateway{
		HTTPClient:  server.Client(),
		VideoFrames: &fakeVideoFrameExtractor{err: errors.New("ffmpeg unavailable")},
		Log:         config.NewLogger(config.LevelError),
	}
	embed := Embed{
		Type:      "gifv",
		Video:     EmbedImage{URL: server.URL + "/video.mp4"},
		Thumbnail: EmbedImage{URL: server.URL + "/thumbnail.jpg"},
	}

	images, unsupported := dg.loadEmbedImagesLimit([]Embed{embed}, 1)
	if len(images) != 1 || len(unsupported) != 0 || !requestedThumbnail {
		t.Fatalf("images=%d unsupported=%v thumbnail_requested=%t", len(images), unsupported, requestedThumbnail)
	}
}

func TestDiscordGIFVEmbedJSONIncludesVideo(t *testing.T) {
	var embed Embed
	err := json.Unmarshal([]byte(`{"type":"gifv","video":{"url":"https://cdn.example/video.mp4","proxy_url":"https://proxy.example/video.mp4","width":480,"height":270}}`), &embed)
	if err != nil {
		t.Fatal(err)
	}
	if got := discordEmbedVideoURL(embed); got != "https://proxy.example/video.mp4" {
		t.Fatalf("video URL = %q", got)
	}
}

type fakeVideoFrameExtractor struct {
	data  []byte
	image llm.InputImage
	err   error
}

func (f *fakeVideoFrameExtractor) Extract(_ context.Context, data []byte, _ string) (llm.InputImage, error) {
	f.data = append([]byte(nil), data...)
	return f.image, f.err
}

func testDiscordJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 40, G: 80, B: 120, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

type discordFakeChatter struct {
	mu        sync.Mutex
	requests  []llm.ChatRequest
	principal identity.Principal
}

func (f *discordFakeChatter) Chat(ctx context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.principal, _ = requestctx.PrincipalFromContext(ctx)
	f.mu.Unlock()
	if req.Format == "json_object" {
		return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: `{"session_updates":{"summary":"","open_threads":[],"decisions":[],"user_goals":[]},"memory_candidates":[]}`}}, nil
	}
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "discord response"}}, nil
}

func (f *discordFakeChatter) lastPrincipal() identity.Principal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.principal
}

func primaryDiscordRequests(requests []llm.ChatRequest) []llm.ChatRequest {
	out := make([]llm.ChatRequest, 0, len(requests))
	for _, req := range requests {
		if req.Format != "json_object" {
			out = append(out, req)
		}
	}
	return out
}

type fakeDiscordREST struct {
	server *httptest.Server
	mu     sync.Mutex
	sent   []map[string]interface{}
}

func newFakeDiscordREST(t *testing.T) *fakeDiscordREST {
	t.Helper()
	rest := &fakeDiscordREST{}
	rest.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode discord message payload: %v", err)
			}
			rest.mu.Lock()
			rest.sent = append(rest.sent, payload)
			rest.mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"sent-1"}`))
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/messages/") {
			_, _ = w.Write([]byte(`{"id":"fetched","content":"fetched reply","author":{"id":"bot-1","username":"Oswald"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	return rest
}

func (r *fakeDiscordREST) sentMessages() []map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]interface{}(nil), r.sent...)
}

func (r *fakeDiscordREST) lastMessageContent() string {
	sent := r.sentMessages()
	if len(sent) == 0 {
		return ""
	}
	content, _ := sent[len(sent)-1]["content"].(string)
	return content
}

func newDiscordTestGateway(t *testing.T, apiBaseURL string) (*Gateway, *broker.Broker, *discordFakeChatter) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oswald.db")
	memories := usermemory.NewStore(dbPath, log)
	links := accountlinking.NewService(dbPath, memories, nil, log)
	soulStore := soul.NewStore(filepath.Join(dir, "soul.md"), log)
	if err := soulStore.Write("You are Oswald."); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	chat := &discordFakeChatter{}
	ai := agent.NewAgent(chat, registry.New(log), "test-model", soulStore, memories, promptbudget.ContextBudget{PromptLimit: 100000}, 3, time.Minute, log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	commandService, err := commands.NewService()
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	dg := &Gateway{
		Token:      "token",
		BotID:      "bot-1",
		Broker:     b,
		Links:      links,
		Runtime:    gatewayruntime.Dependencies{Commands: commandService, Log: log},
		Log:        log,
		APIBaseURL: apiBaseURL,
		replyIndex: make(map[string]replyContext),
		hbAcked:    true,
	}
	return dg, b, chat
}

func discordMessage(id, channelID, guildID, authorID, username, content string) MessageCreate {
	var msg MessageCreate
	msg.ID = id
	msg.ChannelID = channelID
	msg.GuildID = guildID
	msg.Content = content
	msg.Author.ID = authorID
	msg.Author.Username = username
	return msg
}
