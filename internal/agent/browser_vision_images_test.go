package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	toolbrowser "github.com/jonahgcarpenter/oswald-ai/internal/tools/browser"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// TestBrowserVisionImagesReachModel guards the browser_vision screenshot path:
// a productive single-image result must ride the tool message to the model,
// not fail with "tool returned unsupported model images".
func TestBrowserVisionImagesReachModel(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	var visionDef llm.ToolDefinition
	for _, def := range toolbrowser.Definitions() {
		if def.Name == toolbrowser.VisionName {
			visionDef = def
		}
	}
	if visionDef.Name == "" {
		t.Fatal("browser_vision definition is missing")
	}
	if err := reg.RegisterDefinition(visionDef); err != nil {
		t.Fatal(err)
	}
	input := testInputImage(t, 8, 6)
	policy := governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: false}}
	handler := func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: "Screenshot visible.", Outcome: governance.OutcomeProductive, Images: []llm.InputImage{input}}, nil
	}
	if err := reg.RegisterHandler(toolbrowser.VisionName, policy, registry.Handler(handler)); err != nil {
		t.Fatal(err)
	}
	chat.responses = []*llm.ChatResponse{
		toolCallResponse("shot", toolbrowser.VisionName, map[string]interface{}{"question": "What does this page look like?"}),
		{Message: llm.ChatMessage{Role: "assistant", Content: "A login page."}},
	}
	response, err := processAgent(a, "browser-vision", "discord", "session", "user-1", "User", "Describe the page", nil, nil)
	if err != nil || response == nil {
		t.Fatalf("browser_vision turn failed: err=%v", err)
	}
	if len(chat.requests) == 0 {
		t.Fatal("model was never called")
	}
	last := chat.requests[len(chat.requests)-1]
	found := false
	for _, message := range last.Messages {
		if message.Role == "tool" && message.ToolName == toolbrowser.VisionName {
			found = true
			if len(message.Images) != 1 {
				t.Fatalf("browser_vision tool message carries %d images", len(message.Images))
			}
		}
		if message.Role == "tool" && strings.HasPrefix(message.Content, "Error:") {
			t.Fatalf("tool result is an error: %q", message.Content)
		}
	}
	if !found {
		t.Fatal("browser_vision tool result is missing from the final request")
	}
}

// TestImageInputToolCoversBrowserVision keeps the shared image-input gate in
// sync with the browser tool surface.
func TestImageInputToolCoversBrowserVision(t *testing.T) {
	if !isImageInputTool(toolbrowser.VisionName) {
		t.Fatal("browser_vision is not an image-input tool")
	}
	if isImageInputTool(toolbrowser.NavigateName) || isImageInputTool("web_search") {
		t.Fatal("non-image browser tool classified as image input")
	}
}
