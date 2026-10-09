package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMapToGatewayMessagesSerializesImagesAsDataURLs(t *testing.T) {
	messages := mapToGatewayMessages([]ChatMessage{
		{
			Role:    "user",
			Content: "Analyze this image and describe what you see.",
			Images: []InputImage{
				{MimeType: "image/jpeg", Data: "abc123"},
			},
		},
	})

	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	raw, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatalf("marshal gateway message: %v", err)
	}

	var got struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal gateway message: %v", err)
	}

	if got.Role != "user" {
		t.Fatalf("expected role user, got %q", got.Role)
	}
	if len(got.Content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(got.Content))
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "Analyze this image and describe what you see." {
		t.Fatalf("unexpected text part: %+v", got.Content[0])
	}
	if got.Content[1].Type != "image_url" || got.Content[1].ImageURL == nil {
		t.Fatalf("unexpected image part: %+v", got.Content[1])
	}
	if got.Content[1].ImageURL.URL != "data:image/jpeg;base64,abc123" {
		t.Fatalf("unexpected image URL %q", got.Content[1].ImageURL.URL)
	}
}

func TestMapToGatewayMessagesKeepsMultimodalToolResultsCorrelated(t *testing.T) {
	messages := mapToGatewayMessages([]ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "text-call", Function: ToolFunction{Name: "text_tool"}}, {ID: "vision-call", Function: ToolFunction{Name: "vision_analyze"}}}},
		{Role: "tool", ToolCallID: "text-call", Content: "text result"},
		{Role: "tool", ToolCallID: "vision-call", Content: "Question: Inspect", Images: []InputImage{{MimeType: "image/png", Data: "abc123", Geometry: &ImageGeometry{SourceWidth: 100, SourceHeight: 80, Width: 100, Height: 80}}}},
	})
	if len(messages) != 3 || messages[1].Content != "text result" || messages[1].ToolCallID != "text-call" || messages[2].Role != "tool" || messages[2].ToolCallID != "vision-call" {
		t.Fatal("tool result correlation or text-only result changed")
	}
	parts, ok := messages[2].Content.([]gatewayContentPart)
	if !ok || len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "Question: Inspect" || parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,abc123" {
		t.Fatalf("tool result did not contain text and image parts: %+v", messages[2])
	}
	encoded, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Geometry") || strings.Contains(string(encoded), "SourceWidth") || strings.Contains(string(encoded), `"role":"user"`) {
		t.Fatal("multimodal tool serialization leaked geometry or added a user message")
	}
}

func TestMapFromGatewayMessageDecodesToolArgumentsAndThinking(t *testing.T) {
	msg := mapFromGatewayMessage(gatewayMessage{
		Role:             "assistant",
		Content:          []interface{}{map[string]interface{}{"type": "text", "text": "hello"}, map[string]interface{}{"type": "image_url"}},
		ReasoningContent: "reasoning",
		Thinking:         "thinking",
		ToolCalls: []gatewayToolCall{{ID: "call-1", Function: gatewayToolFunction{
			Name:      "test.tool",
			Arguments: `{"value":42}`,
		}}},
	})

	if msg.Content != "hello" || msg.Thinking != "reasoning" {
		t.Fatalf("unexpected message content/thinking: %+v", msg)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "test.tool" || msg.ToolCalls[0].Function.Arguments["value"].(float64) != 42 || msg.ToolCalls[0].Function.RawArguments != `{"value":42}` {
		t.Fatalf("unexpected tool calls: %+v", msg.ToolCalls)
	}
}

func TestMapFromGatewayMessageUsesReasoningFallback(t *testing.T) {
	msg := mapFromGatewayMessage(gatewayMessage{
		Role:      "assistant",
		Content:   "hello",
		Reasoning: "reasoning",
	})

	if msg.Thinking != "reasoning" {
		t.Fatalf("expected reasoning fallback, got %+v", msg)
	}
}

func TestDecodeToolArgumentsFallsBackToRaw(t *testing.T) {
	got := decodeToolArguments("not-json")
	if got["_raw"] != "not-json" {
		t.Fatalf("unexpected decoded args: %+v", got)
	}
	if responseFormat("json_object").Type != "json_object" {
		t.Fatal("expected canonical json_object response format")
	}
}
