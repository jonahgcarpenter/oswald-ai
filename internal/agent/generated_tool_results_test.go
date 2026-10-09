package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	visionanalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/vision_analyze"
)

func TestGeneratedToolResultPreviewAndRetryPreserveOriginalDelivery(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{response: toolCallResponse("generate", imagegenerate.Name, map[string]interface{}{"prompt": "private-generation-prompt"})},
		{err: &llm.ChatHTTPError{StatusCode: 500, Body: "model runner has unexpectedly stopped"}},
		{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Ready."}}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	input := testInputImage(t, 2000, 1200)
	data, err := base64.StdEncoding.DecodeString(input.Data)
	if err != nil {
		t.Fatal(err)
	}
	policy := testToolPolicy()
	policy.History = governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: false}
	if err := registerTestTool(t, reg, testToolSpec{Name: imagegenerate.Name}, policy, func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: `{"status":"generated"}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "original.jpg", MIMEType: input.MimeType, Data: data}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	a.log = config.NewLogger(config.LevelInfo)
	a.log.SetOutput(&logs)
	response, err := processAgent(a, "generated-tool", "discord", "session", "user-1", "User", "Generate an image", nil, nil)
	if err != nil || response == nil || len(response.Attachments) != 1 || !bytes.Equal(response.Attachments[0].Data, data) {
		t.Fatalf("generated original was not preserved for delivery: err=%v", err)
	}
	if len(chat.requests) != 3 {
		t.Fatalf("model calls=%d, want generation, preview, and retry", len(chat.requests))
	}
	var path string
	for i, dimensions := range [][2]int{{1500, 900}, {1125, 675}} {
		messages := chat.requests[i+1].Messages
		if len(messages) != 4 || messages[2].Role != "assistant" || messages[2].ToolCalls[0].ID != "generate" {
			t.Fatal("generated preview added a separate message or lost the assistant tool call")
		}
		result := messages[3]
		if result.Role != "tool" || result.ToolCallID != "generate" || len(result.Images) != 1 || !strings.Contains(result.Content, "Image generated successfully") {
			t.Fatal("generated preview was not inside the correlated tool result")
		}
		previewBytes, err := base64.StdEncoding.DecodeString(result.Images[0].Data)
		if err != nil {
			t.Fatal(err)
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(previewBytes))
		if err != nil || config.Width != dimensions[0] || config.Height != dimensions[1] {
			t.Fatalf("incorrect preview dimensions: %+v err=%v", config, err)
		}
		if result.Images[0].Geometry.Width != config.Width || result.Images[0].Geometry.Height != config.Height || !strings.Contains(result.Content, "from 2000×1200 to ") {
			t.Fatal("generated preview geometry is stale")
		}
		currentPath := generatedResultID(messages, "generate")
		if currentPath == "" || (path != "" && path != currentPath) {
			t.Fatal("generated cache path was missing or changed during retry")
		}
		path = currentPath
	}
	if !strings.Contains(chat.requests[1].Messages[3].Content, "to 1500×900") || !strings.Contains(chat.requests[2].Messages[3].Content, "to 1125×675") {
		t.Fatal("generated tool result did not refresh its retry note")
	}
	resolved, _, err := a.imageCache.Resolve(context.Background(), "user-1", path)
	if err != nil || !bytes.Equal(resolved, data) {
		t.Fatal("model preview replaced the cached original")
	}
	turns, err := store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session", response.SessionGeneration, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(turns)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), input.Data) || strings.Contains(string(encoded), "SourceWidth") || !strings.Contains(string(encoded), "private-generation-prompt") || !strings.Contains(string(encoded), path) {
		t.Fatal("generated text history lost its prompt/path or retained image bytes/geometry")
	}
	for _, canary := range []string{path, input.Data, "private-generation-prompt"} {
		if strings.Contains(logs.String(), canary) {
			t.Fatal("generated tool result logged private data")
		}
	}
}

func TestGeneratedAndLoadedToolPreviewsHaveIndependentBounds(t *testing.T) {
	input := testInputImage(t, 2, 2)
	var messages []llm.ChatMessage
	for i := 0; i < 6; i++ {
		for _, name := range []string{imagegenerate.Name, visionanalyze.Name} {
			messages = append(messages, llm.ChatMessage{Role: "tool", ToolName: name, Content: "image", Images: []llm.InputImage{input}})
		}
	}
	bounded := boundImageToolResults(messages)
	counts := map[string]int{}
	for _, message := range bounded {
		counts[message.ToolName] += len(message.Images)
	}
	if counts[imagegenerate.Name] != 4 || counts[visionanalyze.Name] != 4 || len(messages[0].Images) != 1 {
		t.Fatal("generated and loaded preview limits collided or mutated original messages")
	}
}

func TestGeneratedGIFToolPreviewUsesFirstFrameAndDeliversOriginal(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
	second := image.NewPaletted(first.Bounds(), palette)
	second.SetColorIndex(0, 0, 1)
	var original bytes.Buffer
	if err := gif.EncodeAll(&original, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	chat := &fakeChatter{responses: []*llm.ChatResponse{toolCallResponse("gif", imagegenerate.Name, map[string]interface{}{"prompt": "animation"}), {Message: llm.ChatMessage{Role: "assistant", Content: "Ready."}}}}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	if err := registerTestTool(t, reg, testToolSpec{Name: imagegenerate.Name}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: `{}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "original.gif", MIMEType: "image/gif", Data: original.Bytes()}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	response, err := processAgent(a, "generated-gif", "discord", "session", "user-1", "User", "Generate an animation", nil, nil)
	if err != nil || len(response.Attachments) != 1 || !bytes.Equal(response.Attachments[0].Data, original.Bytes()) {
		t.Fatalf("original GIF delivery changed: err=%v", err)
	}
	result := toolResultByID(chat.requests[1].Messages, "gif")
	if result == nil || len(result.Images) != 1 || !strings.Contains(result.Content, "Showing only the first frame") || result.Images[0].IsGIFContactSheet {
		t.Fatal("generated GIF tool result did not disclose first-frame loading")
	}
	preview, err := base64.StdEncoding.DecodeString(result.Images[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(preview))
	if err != nil || decoded.Bounds().Dx() != 3 || decoded.Bounds().Dy() != 2 {
		t.Fatalf("GIF preview dimensions changed: err=%v", err)
	}
	r, g, b, _ := decoded.At(0, 0).RGBA()
	if r > 1000 || g > 1000 || b > 1000 {
		t.Fatal("GIF preview used the second frame instead of the first")
	}
}
