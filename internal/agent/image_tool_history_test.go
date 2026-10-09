package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	visionanalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/vision_analyze"
)

func TestImageToolTextHistorySurvivesFollowUpAndStoreReopen(t *testing.T) {
	for _, name := range []string{imagegenerate.Name, visionanalyze.Name} {
		t.Run(name, func(t *testing.T) {
			chat := &fakeChatter{}
			reg := registry.New(config.NewLogger(config.LevelError))
			a, fixture := newTestAgent(t, chat, nil, reg)
			input := testInputImage(t, 3, 2)
			data, err := base64.StdEncoding.DecodeString(input.Data)
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]interface{}{}
			var handler registry.Handler
			if name == imagegenerate.Name {
				args["prompt"] = "A portrait with private-generation-detail"
				handler = func(context.Context, map[string]interface{}) (governance.Result, error) {
					return governance.Result{Content: `{}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "image.jpg", MIMEType: input.MimeType, Data: data}}}, nil
				}
			} else {
				path, err := a.imageCache.Save(context.Background(), "user-1", data, input.MimeType)
				if err != nil {
					t.Fatal(err)
				}
				args["image_url"], args["question"] = path, "Read private-vision-detail"
				handler = visionanalyze.NewHandler(a.imageCache)
			}
			policy := testToolPolicy()
			policy.History = governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: false}
			if err := registerTestTool(t, reg, testToolSpec{Name: name}, policy, handler); err != nil {
				t.Fatal(err)
			}
			chat.responses = []*llm.ChatResponse{toolCallResponse("initial-image", name, args), {Message: llm.ChatMessage{Role: "assistant", Content: "Original answer."}}}
			first, err := processAgent(a, "initial-image", "discord", "session", "user-1", "User", "Work with the image", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			original := toolResultByID(chat.requests[1].Messages, "initial-image")
			if original == nil || len(original.Images) != 1 {
				t.Fatal("original image was not included in the live tool result")
			}
			if err := fixture.stores["user-1"].Close(); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(fixture.root, "user-1")
			reopened, err := memory.NewProfileStore(context.Background(), root, "user-1", config.NewLogger(config.LevelError))
			if err != nil {
				t.Fatal(err)
			}
			fixture.stores["user-1"] = reopened
			a = NewAgent(chat, reg, "test-model", "test-provider", a.soul, fixture, a.budget, testGlobalPolicy(), config.NewLogger(config.LevelError))
			a.SetImageCache(imagecache.NewProfileCache(root, "user-1"))
			chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Follow-up answer."}}}
			if _, err := processAgent(a, "follow-up", "discord", "session", "user-1", "User", "Hey", nil, nil); err != nil {
				t.Fatal(err)
			}
			messages := chat.requests[len(chat.requests)-1].Messages
			callID := fmt.Sprintf("hist_%d_1_1", first.SourceTurnID)
			result := toolResultByID(messages, callID)
			if result == nil || result.ToolName != name || !strings.HasSuffix(result.Content, original.Content) {
				t.Fatal("follow-up lost the original text result or its historical annotation")
			}
			if name == imagegenerate.Name && result.Content != original.Content {
				t.Fatal("historical generation result gained extra text")
			}
			if name == visionanalyze.Name && !strings.Contains(result.Content, "image bytes are omitted") {
				t.Fatal("historical vision result lost its historical annotation")
			}
			foundCall := false
			for _, message := range messages {
				if strings.Contains(message.Content, "[Session image catalog;") || strings.Contains(message.Content, "Available image_url paths") {
					t.Fatal("follow-up injected a redundant image catalog message")
				}
				if len(message.Images) != 0 {
					t.Fatal("follow-up replayed historical image bytes")
				}
				for _, call := range message.ToolCalls {
					if call.ID == callID {
						foundCall = true
						actual, err := json.Marshal(call.Function.Arguments)
						if err != nil {
							t.Fatal(err)
						}
						want, err := json.Marshal(args)
						if err != nil || string(actual) != string(want) {
							t.Fatal("historical image tool arguments changed")
						}
					}
				}
			}
			if !foundCall {
				t.Fatal("follow-up tool result has no correlated assistant call")
			}
			chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Other profile."}}}
			if _, err := processAgent(a, "other-profile", "discord", "session", "user-2", "User", "Hey", nil, nil); err != nil {
				t.Fatal(err)
			}
			other := chat.requests[len(chat.requests)-1].Messages
			if messagesContain(other, original.Content) || toolResultByID(other, callID) != nil {
				t.Fatal("image tool history leaked across profiles")
			}
		})
	}
}

func TestPersistedImageToolHistoryRedactsInlineDataAndKeepsText(t *testing.T) {
	for _, name := range []string{imagegenerate.Name, visionanalyze.Name} {
		for _, source := range []string{"data:image/png;base64,private-image-canary", " DATA:image/png;base64,private-image-canary "} {
			args := map[string]interface{}{"image_url": source, "question": "Text to retain", "region": []interface{}{0, 0, 10, 20}}
			call := llm.ToolCall{Function: llm.ToolFunction{Name: name, Arguments: args}}
			policy := governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: false}.Effective()
			history := persistedToolCall(call, policy, governance.Decision{Allowed: true}, governance.Result{Outcome: governance.OutcomeProductive}, nil, "Text result to retain", time.Now())
			encoded, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "private-image-canary") || history.Arguments["image_url"] != "[Inline image data omitted]" || history.Arguments["question"] != "Text to retain" || history.Result != "Text result to retain" || history.ArgumentsTruncated || history.SearchResult || args["image_url"] != source {
				t.Fatal("text history retained image data, lost text, or mutated live arguments")
			}
			projection := memory.ToolHistorySearchText(memory.ToolHistory{Version: memory.ToolHistoryVersion, Batches: []memory.ToolHistoryBatch{{Calls: []memory.ToolHistoryCall{history}}}})
			if projection != name {
				t.Fatalf("image text entered search projection: %q", projection)
			}
		}
	}
}

func TestImageHistoryReplaySurvivesMixedMetadataAndUnavailableTools(t *testing.T) {
	for _, name := range []string{imagegenerate.Name, visionanalyze.Name} {
		imageCall := memory.ToolHistoryCall{Name: name, HistoryMode: string(governance.HistoryFull), Arguments: map[string]interface{}{"image_url": "/private/image.png"}, Result: "Image result", Status: "succeeded"}
		for _, other := range []memory.ToolHistoryCall{
			{Name: "memory", HistoryMode: string(governance.HistoryMetadata)},
			{Name: "unavailable", HistoryMode: string(governance.HistoryFull)},
		} {
			turn := memory.SessionTurn{ID: 42, UserText: "Image please", AssistantText: "Done", ToolHistory: memory.ToolHistory{Version: memory.ToolHistoryVersion, Batches: []memory.ToolHistoryBatch{{Calls: []memory.ToolHistoryCall{other, imageCall}}}}}
			prepared := prepareHistoricalTurns([]memory.SessionTurn{turn}, nil)
			messages := memory.SessionTurnMessages(prepared[0])
			if len(messages) != 4 || len(messages[1].ToolCalls) != 1 || messages[1].ToolCalls[0].Function.Name != name || messages[2].ToolCallID != messages[1].ToolCalls[0].ID || !strings.HasSuffix(messages[2].Content, imageCall.Result) || len(messages[2].Images) != 0 {
				t.Fatal("mixed history removed a replayable image pair or broke correlation")
			}
			if len(turn.ToolHistory.Batches[0].Calls) != 2 {
				t.Fatal("preparation mutated stored history")
			}
			imageCall.ArgumentsTruncated = true
			turn.ToolHistory.Batches[0].Calls[1] = imageCall
			if len(prepareHistoricalTurns([]memory.SessionTurn{turn}, nil)[0].ToolHistory.Batches) != 0 {
				t.Fatal("truncated arguments were replayed as complete")
			}
			imageCall.ArgumentsTruncated = false
		}
	}
}
