package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	videonalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/video_analyze"
)

// TestVideoAnalyzeImagesReachModel guards the video_analyze contact-sheet
// path: a productive single-image result must ride the tool message to the
// model, not fail with "tool returned unsupported model images".
func TestVideoAnalyzeImagesReachModel(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	def := videonalyze.Definition()
	if def.Name != videonalyze.Name {
		t.Fatal("video_analyze definition is missing")
	}
	if err := reg.RegisterDefinition(def); err != nil {
		t.Fatal(err)
	}
	input := testInputImage(t, 8, 6)
	policy := governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}
	handler := func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: "Video loaded.", Outcome: governance.OutcomeProductive, Images: []llm.InputImage{input}}, nil
	}
	if err := reg.RegisterHandler(videonalyze.Name, policy, registry.Handler(handler)); err != nil {
		t.Fatal(err)
	}
	chat.responses = []*llm.ChatResponse{
		toolCallResponse("watch", videonalyze.Name, map[string]interface{}{"question": "What happens?"}),
		{Message: llm.ChatMessage{Role: "assistant", Content: "A person picks up a mug."}},
	}
	response, err := processAgent(a, "video-frames", "discord", "session", "user-1", "User", "Describe the video", nil, nil)
	if err != nil || response == nil {
		t.Fatalf("video_analyze turn failed: err=%v", err)
	}
	if len(chat.requests) == 0 {
		t.Fatal("model was never called")
	}
	last := chat.requests[len(chat.requests)-1]
	found := false
	for _, message := range last.Messages {
		if message.Role == "tool" && message.ToolName == videonalyze.Name {
			found = true
			if len(message.Images) != 1 {
				t.Fatalf("video_analyze tool message carries %d images", len(message.Images))
			}
		}
		if message.Role == "tool" && strings.HasPrefix(message.Content, "Error:") {
			t.Fatalf("tool result is an error: %q", message.Content)
		}
	}
	if !found {
		t.Fatal("video_analyze tool result is missing from the final request")
	}
}

// TestImageInputToolCoversVideoAnalyze keeps the shared image-input gate in
// sync with the video tool surface.
func TestImageInputToolCoversVideoAnalyze(t *testing.T) {
	if !isImageInputTool(videonalyze.Name) {
		t.Fatal("video_analyze is not an image-input tool")
	}
	if isImageInputTool("terminal") || isImageInputTool("web_search") {
		t.Fatal("non-image tool classified as image input")
	}
}
