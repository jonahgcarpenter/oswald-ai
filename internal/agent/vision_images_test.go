package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	visionanalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/vision_analyze"
)

func TestVisionLoadsPreviousAttachmentWithoutGenerationOrDelivery(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Initial answer."}}}}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	if err := reg.RegisterDefinition(visionanalyze.Definition()); err != nil {
		t.Fatal(err)
	}
	policy := governance.ToolPolicy{BlockDuplicates: true, MaxFailures: 2, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: false}}
	if err := reg.RegisterHandler(visionanalyze.Name, policy, registry.Handler(visionanalyze.NewHandler(a.imageCache))); err != nil {
		t.Fatal(err)
	}
	input := testInputImage(t, 8, 6)
	if _, err := processAgent(a, "initial", "discord", "session", "user-1", "User", "See this?", []llm.InputImage{input}, nil); err != nil {
		t.Fatal(err)
	}
	paths := attachedImagePaths(chat.requests[0].Messages[len(chat.requests[0].Messages)-1].Content)
	if len(paths) != 1 || !requestHasTool(chat.requests[0], visionanalyze.Name) {
		t.Fatal("vision-only configuration did not cache and annotate the attachment")
	}
	var logs bytes.Buffer
	a.log = config.NewLogger(config.LevelInfo)
	a.log.SetOutput(&logs)
	a.toolPolicy = governance.GlobalPolicy{MaxExecutions: 6, MaxToolIterations: 10}
	var calls []llm.ToolCall
	for i := 0; i < 6; i++ {
		source := paths[0]
		if i == 5 {
			source = "data:" + input.MimeType + ";base64," + input.Data
		}
		calls = append(calls, llm.ToolCall{ID: fmt.Sprint(i), Function: llm.ToolFunction{Name: visionanalyze.Name, Arguments: map[string]interface{}{"image_url": source, "question": fmt.Sprintf("private-vision-question-%d", i)}}})
	}
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", ToolCalls: calls}}, {Message: llm.ChatMessage{Role: "assistant", Content: "I see it again."}}}
	var chunks []StreamChunk
	response, err := processAgent(a, "follow-up", "discord", "session", "user-1", "User", "Inspect that previous image", nil, func(chunk StreamChunk) { chunks = append(chunks, chunk) })
	if err != nil || response == nil || len(response.Attachments) != 0 {
		t.Fatalf("vision response delivered attachments or failed: err=%v", err)
	}
	final := chat.requests[len(chat.requests)-1]
	if len(final.Tools) != 0 {
		t.Fatal("vision did not reach tools-disabled final call")
	}
	imageCount, toolCount := 0, 0
	for _, message := range final.Messages {
		imageCount += len(message.Images)
		if len(message.Images) > 0 && (message.Role != "tool" || message.ToolName != visionanalyze.Name) {
			t.Fatal("vision image appeared outside its tool result")
		}
		if strings.HasPrefix(message.Content, "[Images loaded by vision_analyze") {
			t.Fatal("vision added a separate user context message")
		}
		if message.Role == "tool" {
			toolCount++
			if strings.HasPrefix(message.Content, "Error:") {
				t.Fatal("vision call failed")
			}
		}
	}
	if imageCount != 4 || toolCount != 6 {
		t.Fatalf("loaded images=%d tool results=%d", imageCount, toolCount)
	}
	loaded := final.Messages[len(final.Messages)-1]
	if !strings.Contains(loaded.Content, "private-vision-question-5") || strings.Contains(loaded.Content, "private-vision-question-0") {
		t.Fatal("did not retain only the four latest loaded images")
	}
	for _, chunk := range chunks {
		if len(chunk.Attachments) != 0 || (chunk.Tool != nil && chunk.Tool.Name == visionanalyze.Name && (len(chunk.Tool.Arguments) != 0 || chunk.Tool.ResultText != "")) {
			t.Fatal("vision payload leaked into gateway tool stream")
		}
	}
	turns, err := store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session", response.SessionGeneration, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(turns)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), input.Data) || !strings.Contains(string(encoded), "private-vision-question") || !strings.Contains(string(encoded), "[Inline image data omitted]") {
		t.Fatal("vision text history lost its question or retained inline image bytes")
	}
	for _, canary := range []string{paths[0], input.Data, "private-vision-question", "Inspect that previous image"} {
		if strings.Contains(logs.String(), canary) {
			t.Fatal("vision operation logged private content")
		}
	}
	completeCount := 0
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var record map[string]interface{}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["event"] == "agent.tool.complete" {
			completeCount++
			if record["level"] != "info" || record["request_id"] != "follow-up" || record["user_id"] != "user-1" || record["operation_id"] == "" {
				t.Fatalf("tool completion lacks safe correlation: %+v", record)
			}
		}
	}
	if completeCount != 6 {
		t.Fatalf("tool completions=%d", completeCount)
	}
}

func TestVisionContextSurvivesCompactionWithoutInlineBytesInDebt(t *testing.T) {
	compactor := &fakeForegroundCompactor{artifact: memory.SummaryArtifact{Narrative: "Inspected the earlier image."}}
	input := testInputImage(t, 2, 2)
	state := newForegroundCompactionState(compactor, 100, "policy", "", "inspect", nil, nil, nil, nil)
	message := llm.ChatMessage{Role: "tool", ToolName: visionanalyze.Name, ToolCallID: "vision", Content: "Question: Inspect", Images: []llm.InputImage{input}}
	args := map[string]interface{}{"image_url": "data:" + input.MimeType + ";base64," + input.Data, "question": "Inspect"}
	call := llm.ToolCall{ID: "vision", Function: llm.ToolFunction{Name: visionanalyze.Name, Arguments: args}}
	messages := []llm.ChatMessage{{Role: "assistant", ToolCalls: []llm.ToolCall{call}}, message}
	state.imageToolRounds = retainedImageToolRounds(messages)
	history := foregroundToolCall(call, governance.Decision{Allowed: true}, governance.Result{Outcome: governance.OutcomeProductive}, nil, "loaded", time.Now())
	state.addToolBatch(memory.ToolHistoryBatch{Calls: []memory.ToolHistoryCall{history}}, "inspect")
	rebuilt, stats, err := state.prepare(context.Background(), messages, nil, true)
	if err != nil || !stats.Compacted {
		t.Fatalf("compacted=%v err=%v", stats.Compacted, err)
	}
	last := rebuilt[len(rebuilt)-1]
	if len(last.Images) != 1 || last.Images[0].Data != input.Data || last.Content != message.Content || last.Role != "tool" || last.ToolCallID != "vision" {
		t.Fatal("foreground compaction lost the loaded image")
	}
	assistant := rebuilt[len(rebuilt)-2]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != last.ToolCallID || assistant.ToolCalls[0].Function.Arguments["image_url"] != "[Inline image data omitted]" {
		t.Fatal("compaction orphaned the image result or retained inline data URL bytes")
	}
	encoded, err := json.Marshal(compactor.calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), input.Data) || args["image_url"] != call.Function.Arguments["image_url"] {
		t.Fatal("compaction received inline bytes or mutated the original tool call")
	}
}

func TestVisionImageBoundRetainsCompleteMixedToolRounds(t *testing.T) {
	input := testInputImage(t, 2, 2)
	var messages []llm.ChatMessage
	for i := 0; i < 6; i++ {
		visionID, otherID := fmt.Sprintf("vision-%d", i), fmt.Sprintf("other-%d", i)
		messages = append(messages,
			llm.ChatMessage{Role: "assistant", Thinking: "private reasoning", ToolCalls: []llm.ToolCall{{ID: visionID, Function: llm.ToolFunction{Name: visionanalyze.Name}}, {ID: otherID, Function: llm.ToolFunction{Name: "other"}}}},
			llm.ChatMessage{Role: "tool", ToolName: visionanalyze.Name, ToolCallID: visionID, Content: "loaded", Images: []llm.InputImage{input}},
			llm.ChatMessage{Role: "tool", ToolName: "other", ToolCallID: otherID, Content: "other result"},
		)
	}
	bounded := boundImageToolResults(messages)
	if len(messages[1].Images) != 1 || len(bounded[1].Images) != 0 || !strings.Contains(bounded[1].Content, "no longer included") {
		t.Fatal("image bound mutated the original or failed to evict oldest images")
	}
	retained := retainedImageToolRounds(bounded)
	if len(retained) != 12 {
		t.Fatalf("retained %d messages, want four complete three-message rounds", len(retained))
	}
	for i := 0; i < len(retained); i += 3 {
		assistant := retained[i]
		if assistant.Role != "assistant" || assistant.Thinking != "" || len(assistant.ToolCalls) != 2 || assistant.ToolCalls[0].ID != retained[i+1].ToolCallID || assistant.ToolCalls[1].ID != retained[i+2].ToolCallID || retained[i+1].Role != "tool" || len(retained[i+1].Images) != 1 || retained[i+2].Content != "other result" {
			t.Fatal("vision retention orphaned or split a mixed tool batch")
		}
	}
}

func TestVisionRetentionNeverRebuildsOrphanToolResults(t *testing.T) {
	input := testInputImage(t, 2, 2)
	assistant := llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "vision", Function: llm.ToolFunction{Name: visionanalyze.Name}}, {ID: "other", Function: llm.ToolFunction{Name: "other"}}}}
	vision := llm.ChatMessage{Role: "tool", ToolCallID: "vision", ToolName: visionanalyze.Name, Content: "loaded", Images: []llm.InputImage{input}}
	other := llm.ChatMessage{Role: "tool", ToolCallID: "wrong-id", ToolName: "other", Content: "other result"}
	for _, messages := range [][]llm.ChatMessage{{vision}, {assistant, vision}, {assistant, vision, other}} {
		if len(retainedImageToolRounds(messages)) != 0 {
			t.Fatal("vision retention accepted an incomplete or mismatched tool round")
		}
	}
}

func TestVisionHiddenAndBlockedForAPI(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{toolCallResponse("blocked", visionanalyze.Name, map[string]interface{}{"image_url": "/private/image.png", "question": "Inspect"}), {Message: llm.ChatMessage{Role: "assistant", Content: "Text only."}}}}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	if err := reg.RegisterDefinition(visionanalyze.Definition()); err != nil {
		t.Fatal(err)
	}
	executed := false
	if err := reg.RegisterHandler(visionanalyze.Name, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		executed = true
		return governance.Result{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := a.Process(context.Background(), Request{Principal: identity.Principal{CanonicalUserID: "user-1", Gateway: "openai", ExternalID: identity.LocalOpenAIIdentifier, Assurance: identity.AssuranceLocalLoopback}, Prompt: "Hello", Stateless: true})
	if err != nil {
		t.Fatal(err)
	}
	if executed || requestHasTool(chat.requests[0], visionanalyze.Name) || toolResultByID(chat.requests[1].Messages, "blocked") == nil {
		t.Fatal("API exposed or executed vision, or omitted the blocked result")
	}
}

func TestLoadedVisionImagesSurviveImageRetries(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{err: &llm.ChatHTTPError{StatusCode: 500, Body: "model runner has unexpectedly stopped"}},
		{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Visible."}}},
	}}
	a, _ := newTestAgent(t, chat, nil, nil)
	input := testInputImage(t, 100, 80)
	input.Geometry = &llm.ImageGeometry{SourceWidth: 100, SourceHeight: 80, Width: 100, Height: 80}
	message := llm.ChatMessage{Role: "tool", ToolCallID: "vision", ToolName: visionanalyze.Name, Content: "Question: Inspect", Images: []llm.InputImage{input}}
	assistant := llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "vision", Function: llm.ToolFunction{Name: visionanalyze.Name}}}}
	_, err, exhausted := a.chatWithImageRetries(context.Background(), llm.ChatRequest{Messages: []llm.ChatMessage{assistant, message}}, nil, a.log)
	if err != nil || exhausted || len(chat.requests) != 2 {
		t.Fatalf("image retry requests=%d exhausted=%v err=%v", len(chat.requests), exhausted, err)
	}
	for i, request := range chat.requests {
		if len(request.Messages) != 2 || request.Messages[1].Role != "tool" || request.Messages[1].ToolCallID != "vision" || len(request.Messages[1].Images) != 1 || !strings.HasPrefix(request.Messages[1].Content, message.Content) {
			t.Fatal("image retry lost loaded vision context")
		}
		if _, err := base64.StdEncoding.DecodeString(request.Messages[1].Images[0].Data); err != nil {
			t.Fatal(err)
		}
		if i == 0 && strings.Contains(request.Messages[1].Content, "downscaled") {
			t.Fatal("unchanged image received a resize note")
		}
		if i == 1 && (!strings.Contains(request.Messages[1].Content, "from 100×80 to 75×60") || !strings.Contains(request.Messages[1].Content, "by 1.333333")) {
			t.Fatal("retry note did not reflect the actual displayed dimensions")
		}
	}
	if message.Images[0].Data != input.Data || input.Geometry.Width != 100 || input.Geometry.Height != 80 {
		t.Fatal("retry mutated the original loaded image")
	}
}

func TestCroppedVisionRetryNotesUseActualScalesAndSourceOffsets(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{err: &llm.ChatHTTPError{StatusCode: 500, Body: "model runner has unexpectedly stopped"}},
		{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Visible."}}},
	}}
	a, _ := newTestAgent(t, chat, nil, nil)
	source := testInputImage(t, 800, 600)
	args := map[string]interface{}{"image_url": "data:" + source.MimeType + ";base64," + source.Data, "question": "Read the small text", "region": []interface{}{400, 200, 501, 280}}
	ctx := requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "user-1", Gateway: "discord", ExternalID: "synthetic", Assurance: identity.AssuranceDiscordGateway})
	result, err := visionanalyze.NewHandler(a.imageCache)(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	message := llm.ChatMessage{Role: "tool", ToolCallID: "crop", ToolName: visionanalyze.Name, Content: result.Content, Images: result.Images}
	assistant := llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "crop", Function: llm.ToolFunction{Name: visionanalyze.Name, Arguments: args}}}}
	_, err, exhausted := a.chatWithImageRetries(ctx, llm.ChatRequest{Messages: []llm.ChatMessage{assistant, message}}, nil, a.log)
	if err != nil || exhausted || len(chat.requests) != 2 {
		t.Fatalf("crop retry requests=%d exhausted=%v err=%v", len(chat.requests), exhausted, err)
	}
	retried := chat.requests[1].Messages[1]
	for _, text := range []string{"Question: Read the small text", "Source image: 800×600.", "source region [400, 200, 501, 280]", "Crop downscaled from 101×80 to 76×60", "x = displayed x × 1.328947 + 400", "y = displayed y × 1.333333 + 200"} {
		if !strings.Contains(retried.Content, text) {
			t.Fatalf("crop retry note missing %q: %q", text, retried.Content)
		}
	}
	if retried.Images[0].Geometry.Width != 76 || retried.Images[0].Geometry.Height != 60 || result.Images[0].Geometry.Width != 101 || result.Images[0].Geometry.Height != 80 || strings.Count(retried.Content, "Showing source region") != 1 {
		t.Fatal("retry used stale dimensions, mutated source geometry, or duplicated the crop note")
	}
}
