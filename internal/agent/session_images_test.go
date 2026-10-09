package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func generatedResultID(messages []llm.ChatMessage, callID string) string {
	result := toolResultByID(messages, callID)
	if result == nil {
		return ""
	}
	for _, line := range strings.Split(result.Content, "\n") {
		if path, ok := strings.CutPrefix(line, "Image: "); ok {
			return path
		}
	}
	return ""
}

func attachedImagePaths(content string) []string {
	var paths []string
	for _, line := range strings.Split(content, "\n") {
		if path, ok := strings.CutPrefix(line, "[Image attached at: "); ok {
			paths = append(paths, strings.TrimSuffix(path, "]"))
		}
	}
	return paths
}

func TestSessionMemoryUserContentKeepsImagePathsAndStripsReplyContext(t *testing.T) {
	images := []requestctx.InputImage{{Path: "/private/first.jpg"}, {Path: "/private/second.jpg"}}
	for _, test := range []struct {
		name   string
		prompt string
		text   string
	}{
		{name: "reply", prompt: "[Replying to Alice: \"old\"]\n\nnew prompt", text: "new prompt"},
		{name: "reply only", prompt: "[Replying to Alice: \"old\"]", text: "[User replied to a prior message]"},
		{name: "image only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := strings.TrimSpace(test.text + "\n\n[Image attached at: /private/first.jpg]\n\n[Image attached at: /private/second.jpg]")
			if got := sessionMemoryUserContent(test.prompt, images); got != want {
				t.Fatalf("stored content = %q, want %q", got, want)
			}
		})
	}
}

func TestCurrentImagePathsStayInUserMessage(t *testing.T) {
	for _, count := range []int{1, 2, 4} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			chat := &fakeChatter{responses: []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Yes."}}}}
			reg := registry.New(config.NewLogger(config.LevelError))
			a, store := newTestAgent(t, chat, nil, reg)
			if err := registerTestTool(t, reg, testToolSpec{Name: imagegenerate.Name}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
				t.Fatal("unexpected image generation")
				return governance.Result{}, nil
			}); err != nil {
				t.Fatal(err)
			}
			var images []llm.InputImage
			for i := 0; i < count; i++ {
				images = append(images, testInputImage(t, 2+i, 3+i))
			}
			response, err := processAgent(a, "attached", "discord", "session", "user-1", "User", "Can you see this?", images, nil)
			if err != nil {
				t.Fatal(err)
			}
			messages := chat.requests[0].Messages
			if len(messages) != 2 {
				t.Fatalf("wanted system and one user message, got %d messages", len(messages))
			}
			current := messages[1]
			paths := attachedImagePaths(current.Content)
			if current.Role != "user" || !strings.HasPrefix(current.Content, "Can you see this?\n\n[Image attached at: ") || len(paths) != count || len(current.Images) != count {
				t.Fatalf("attachment message has %d paths and %d images", len(paths), len(current.Images))
			}
			for i, path := range paths {
				if !filepath.IsAbs(path) || current.Images[i].Data != images[i].Data {
					t.Fatal("attachment path or image order changed")
				}
				if _, _, err := a.imageCache.Resolve(context.Background(), "user-1", path); err != nil {
					t.Fatal(err)
				}
			}
			turns, err := store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session", response.SessionGeneration, 0, 10)
			if err != nil || len(turns) != 1 || turns[0].UserText != current.Content {
				t.Fatalf("stored attachment paths lost: turns=%d err=%v", len(turns), err)
			}
			chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Still yes."}}}
			if _, err := processAgent(a, "follow-up", "discord", "session", "user-1", "User", "What about that image?", nil, nil); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, message := range chat.requests[len(chat.requests)-1].Messages {
				if message.Role == "user" && message.Content == current.Content {
					found = true
					if len(message.Images) != 0 {
						t.Fatal("historical image bytes replayed")
					}
				}
			}
			if !found {
				t.Fatal("follow-up history lost attachment paths")
			}
			chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Separate profile."}}}
			if _, err := processAgent(a, "other-profile", "discord", "session", "user-2", "User", "What image?", nil, nil); err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				if messagesContain(chat.requests[len(chat.requests)-1].Messages, path) {
					t.Fatal("attachment path leaked into another profile's history")
				}
			}
		})
	}
}

func TestAttachedImagePathsSurviveCompaction(t *testing.T) {
	compactor := &fakeForegroundCompactor{artifact: memory.SummaryArtifact{Narrative: "Earlier context."}}
	image := requestctx.InputImage{ID: "current-1", Path: "/private/current.png", MIMEType: "image/png", Data: "payload"}
	prompt := promptWithAttachedImages("Describe this", []requestctx.InputImage{image})
	state := newForegroundCompactionState(compactor, 100, "policy", "", prompt, []llm.InputImage{{MimeType: image.MIMEType, Data: image.Data}}, nil, []memory.SessionTurn{{ID: 1, UserText: "old", AssistantText: "answer"}}, nil)
	rebuilt, stats, err := state.prepare(context.Background(), []llm.ChatMessage{state.current}, nil, true)
	if err != nil || !stats.Compacted {
		t.Fatalf("compacted=%v err=%v", stats.Compacted, err)
	}
	last := rebuilt[len(rebuilt)-1]
	if last.Content != prompt || len(last.Images) != 1 || last.Images[0].Data != image.Data {
		t.Fatal("compaction separated or lost the current attachment")
	}
}

func TestImageCatalogPathsStableAcrossTurnsAndNoIDsInModelResults(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	input := testInputImage(t, 2, 3)
	data, _ := base64.StdEncoding.DecodeString(input.Data)
	if err := registerTestTool(t, reg, testToolSpec{Name: imagegenerate.Name}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: `{"status":"generated","image_id":"untrusted-id","source_image_id":"untrusted-source"}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "output.jpg", MIMEType: input.MimeType, Data: data}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	chat.responses = []*llm.ChatResponse{toolCallResponse("image", imagegenerate.Name, map[string]interface{}{"prompt": "picture"}), {Message: llm.ChatMessage{Role: "assistant", Content: "Ready."}}}
	response, err := processAgent(a, "image-path", "discord", "session", "user-1", "User", "picture", []llm.InputImage{input}, nil)
	if err != nil {
		t.Fatal(err)
	}
	attachedPaths := attachedImagePaths(chat.requests[0].Messages[len(chat.requests[0].Messages)-1].Content)
	if len(attachedPaths) != 1 {
		t.Fatal("current attachment path missing")
	}
	occurrences := 0
	for _, message := range chat.requests[1].Messages {
		occurrences += strings.Count(message.Content, attachedPaths[0])
	}
	if occurrences != 1 {
		t.Fatal("current attachment path repeated in generated-image catalog")
	}
	result := toolResultByID(chat.requests[1].Messages, "image")
	if result == nil || result.Role != "tool" || len(result.Images) != 1 {
		t.Fatal("missing image result")
	}
	for _, private := range []string{"image_id", "source_image_id", "parent_source_image_id", "untrusted-id", "untrusted-source"} {
		if strings.Contains(result.Content, private) {
			t.Fatalf("internal metadata %s exposed", private)
		}
	}
	path := generatedResultID(chat.requests[1].Messages, "image")
	if !filepath.IsAbs(path) {
		t.Fatal("invalid model image path")
	}
	if _, _, err := a.imageCache.Resolve(context.Background(), "user-1", path); err != nil {
		t.Fatalf("generated path cannot be resolved: %v", err)
	}
	if _, _, err := imagecache.New(t.TempDir()).Resolve(context.Background(), "user-1", path); err == nil {
		t.Fatal("image path escaped private cache")
	}
	if err := store.MarkSessionTurnDelivered(context.Background(), "user-1", response.SourceTurnID); err != nil {
		t.Fatal(err)
	}
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Again."}}}
	if _, err := processAgent(a, "image-path-next", "discord", "session", "user-1", "User", "describe", nil, nil); err != nil {
		t.Fatal(err)
	}
	last := chat.requests[len(chat.requests)-1]
	if !messagesContain(last.Messages, path) {
		t.Fatal("prior image path changed across turns")
	}
	assets, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
	if err != nil || len(assets) != 1 || messagesContain(last.Messages, assets[0].ID) {
		t.Fatal("persisted ID entered model catalog")
	}
}

func TestPlanGeneratedImageUsesExactCatalogPathForEdits(t *testing.T) {
	sources := []requestctx.InputImage{{ID: "current-1", Path: "/private/current.jpg", ImageID: "logical-1"}}
	for _, args := range []map[string]interface{}{
		{"image_url": ""}, {"image_url": " /private/current.jpg "}, {"image_url": "current-1"}, {"image_url": nil},
	} {
		if _, _, err := planGeneratedImage(args, sources, nil); err == nil {
			t.Fatalf("accepted invalid selector: %v", args)
		}
	}
	created, slot, err := planGeneratedImage(map[string]interface{}{}, sources, nil)
	if err != nil || slot != -1 || created.ImageID != "" || created.ParentSourceImageID != "" {
		t.Fatalf("omission did not create a new logical image: %+v slot=%d err=%v", created, slot, err)
	}
	selected, slot, err := planGeneratedImage(map[string]interface{}{"image_url": "/private/current.jpg"}, sources, nil)
	if err != nil || slot != -1 || selected.ImageID != "logical-1" || selected.ParentSourceImageID != "current-1" {
		t.Fatalf("explicit edit did not bind its source: %+v slot=%d err=%v", selected, slot, err)
	}
	for _, selector := range []string{"/private/other.jpg", "https://example.com/image.png"} {
		other, slot, err := planGeneratedImage(map[string]interface{}{"image_url": selector}, sources, nil)
		if err != nil || slot != -1 || other.ImageID != "" || other.ParentSourceImageID != "" {
			t.Fatalf("unknown source inherited catalog identity: %+v slot=%d err=%v", other, slot, err)
		}
	}
}

func TestModelFailureAfterImagesFinalizesSelectedOutputs(t *testing.T) {
	for _, mode := range []string{"ordinary", "parser_retry", "repeated_parser", "parser_context", "corrective", "tools_disabled"} {
		for _, generated := range []bool{false, true} {
			for _, canceled := range []bool{false, true} {
				for _, streaming := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/images=%t/canceled=%t/stream=%t", mode, generated, canceled, streaming), func(t *testing.T) {
						chat := &fakeChatter{}
						reg := registry.New(config.NewLogger(config.LevelError))
						a, store := newTestAgent(t, chat, nil, reg)
						var logs bytes.Buffer
						a.log = config.NewLogger(config.LevelDebug)
						a.log.SetOutput(&logs)
						policy := testToolPolicy()
						policy.MaxExecutions = 0
						count := 0
						editArgs := map[string]interface{}{"prompt": "edit"}
						chat.onChat = func(req llm.ChatRequest) {
							if id := generatedResultID(req.Messages, "0"); id != "" {
								editArgs["image_url"] = id
							}
						}
						for _, name := range []string{imagegenerate.Name, "work"} {
							if err := registerTestTool(t, reg, testToolSpec{Name: name, Description: name}, policy, func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
								count++
								result := governance.Result{Content: `{}`, Outcome: governance.OutcomeProductive}
								if generated {
									image := testInputImage(t, 2+count, 3+count)
									data, _ := base64.StdEncoding.DecodeString(image.Data)
									result.Attachments = []media.OutputAttachment{{Filename: fmt.Sprintf("image-%d.jpg", count), MIMEType: image.MimeType, Data: data}}
								}
								return result, nil
							}); err != nil {
								t.Fatal(err)
							}
						}
						for i := 0; i < 2; i++ {
							name := imagegenerate.Name
							args := map[string]interface{}{"prompt": fmt.Sprint(i)}
							if i == 1 && generated {
								args = editArgs
							}
							if !generated {
								name = "work"
							}
							chat.outcomes = append(chat.outcomes, fakeChatOutcome{response: toolCallResponse(fmt.Sprint(i), name, args)})
						}
						parserErr := &llm.ChatHTTPError{StatusCode: 500, Body: "XML syntax error on line 7: unexpected EOF"}
						var failure error = errors.New("synthetic provider failure")
						switch mode {
						case "parser_retry", "repeated_parser", "parser_context":
							chat.outcomes = append(chat.outcomes, fakeChatOutcome{err: parserErr})
							if mode == "repeated_parser" {
								failure = parserErr
							}
							if mode == "parser_context" {
								failure = &llm.ChatHTTPError{StatusCode: 400, Body: "maximum context length exceeded"}
							}
						case "corrective":
							chat.outcomes = append(chat.outcomes, fakeChatOutcome{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant"}}})
						case "tools_disabled":
							a.toolPolicy = governance.GlobalPolicy{MaxExecutions: 2, MaxToolIterations: 10}
						}
						if canceled {
							failure = context.Canceled
						}
						chat.outcomes = append(chat.outcomes, fakeChatOutcome{err: failure})
						wantContent, wantKind := generatedImagePartialResponse, "image_partial"
						if mode == "parser_context" {
							wantContent, wantKind = contextCompactionFallback, "context_fallback"
						}
						fallbackChunks := 0
						var callback func(StreamChunk)
						if streaming {
							callback = func(chunk StreamChunk) {
								if len(chunk.Attachments) > 0 {
									t.Fatal("attachments streamed")
								}
								if chunk.Type == ChunkContent && chunk.Text == wantContent {
									fallbackChunks++
								}
							}
						}
						response, err := a.Process(context.Background(), Request{RequestID: "failure", Principal: identity.Principal{CanonicalUserID: "user-1", ExternalID: "user-1", Gateway: "discord", Assurance: identity.AssuranceDiscordGateway}, SessionKey: "session", Prompt: "generate then edit", StreamFunc: callback})
						for _, request := range chat.requests {
							if !request.Stream {
								t.Fatal("model retry or final call did not retain streaming transport")
							}
						}
						if canceled {
							if !errors.Is(err, context.Canceled) || response != nil {
								t.Fatalf("response=%v err=%v", response, err)
							}
							var turns int
							if err := store.sql.QueryRow(`SELECT COUNT(*) FROM messages WHERE role='assistant'`).Scan(&turns); err != nil || turns != 0 {
								t.Fatalf("canceled turns=%d err=%v", turns, err)
							}
							return
						}
						if err != nil || response == nil {
							t.Fatalf("response=%v err=%v", response, err)
						}
						if !generated {
							if mode == "corrective" || mode == "repeated_parser" {
								if response.Response != emptyResponseFallback || response.Error != "" {
									t.Fatal("changed existing empty/parser fallback")
								}
							} else if response.Error == "" || response.SourceTurnID != 0 {
								t.Fatal("changed no-image provider-error semantics")
							}
							return
						}
						if response.Error != "" || response.Response != wantContent || response.Kind != wantKind || response.SourceTurnID == 0 || response.PersistenceStatus != "pending" {
							t.Fatalf("partial response not finalized: %+v", response)
						}
						if len(response.Attachments) != 1 || response.Attachments[0].Filename != "image-2.jpg" {
							t.Fatal("lost selected final image")
						}
						if streaming && fallbackChunks != 1 {
							t.Fatalf("fallback chunks=%d", fallbackChunks)
						}
						if strings.Count(logs.String(), `"event":"agent.response.complete"`) != 1 || !strings.Contains(logs.String(), `"response_kind":"`+wantKind+`"`) {
							t.Fatal("missing partial response measurement")
						}
						images, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
						if err != nil || len(images) != 0 {
							t.Fatal("pending images became editable")
						}
						if err := a.userMemory.MarkSessionTurnDelivered(context.Background(), "user-1", response.SourceTurnID); err != nil {
							t.Fatal(err)
						}
						images, err = store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
						if err != nil || len(images) != 1 || images[0].Version != 2 {
							t.Fatal("delivered selected image unavailable")
						}
						turns, err := store.RecentSessionTurns("user-1", "session", response.SessionGeneration, 10)
						if err != nil || len(turns) != 1 || turns[0].AssistantText != wantContent {
							t.Fatal("partial text not persisted")
						}
					})
				}
			}
		}
	}
}

func TestExplicitImageEditUsesProductionRecencyNotDeliveryOrder(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	editArgs := map[string]interface{}{"prompt": "edit A"}
	var logicalA string
	chat.onChat = func(req llm.ChatRequest) {
		if id := generatedResultID(req.Messages, "A"); id != "" {
			editArgs["image_url"] = id
		}
	}
	count := 0
	if err := registerTestTool(t, reg, testToolSpec{Name: imagegenerate.Name}, testToolPolicy(), func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		count++
		sources := requestctx.InputImagesFromContext(ctx)
		if count == 2 {
			for _, source := range sources {
				if source.Path == editArgs["image_url"] {
					logicalA = source.ImageID
				}
			}
		}
		if count == 4 && (sources[0].ImageID != logicalA || sources[0].Version != 2 || args["image_url"] != sources[0].Path) {
			t.Fatal("explicit selection did not use newest A-v2")
		}
		image := testInputImage(t, 2+count, 3+count)
		data, _ := base64.StdEncoding.DecodeString(image.Data)
		return governance.Result{Content: `{}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("image-%d.jpg", count), MIMEType: image.MimeType, Data: data}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	chat.responses = []*llm.ChatResponse{toolCallResponse("A", imagegenerate.Name, map[string]interface{}{"prompt": "A"}), toolCallResponse("B", imagegenerate.Name, map[string]interface{}{"prompt": "B"}), toolCallResponse("A2", imagegenerate.Name, editArgs), {Message: llm.ChatMessage{Role: "assistant", Content: "A and B"}}}
	response, err := processAgent(a, "recency", "discord", "session", "user-1", "User", "generate A B then edit A", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Attachments) != 2 || response.Attachments[0].Filename != "image-3.jpg" || response.Attachments[1].Filename != "image-2.jpg" {
		t.Fatal("delivery order changed")
	}
	images, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
	if err != nil || len(images) != 2 || images[0].ImageID != logicalA || images[0].Version != 2 {
		t.Fatal("storage lost generation recency")
	}
	chat.responses = []*llm.ChatResponse{toolCallResponse("next", imagegenerate.Name, map[string]interface{}{"prompt": "edit newest", "image_url": generatedResultID(chat.requests[len(chat.requests)-1].Messages, "A2")}), {Message: llm.ChatMessage{Role: "assistant", Content: "A-v3"}}}
	if _, err := processAgent(a, "next", "discord", "session", "user-1", "User", "edit", nil, nil); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("calls=%d", count)
	}
}

func TestGeneratedImageVersionsSelectFinalsAndPreserveOtherAttachments(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	policy := testToolPolicy()
	policy.MaxExecutions = 0
	policy.History.Mode = governance.HistoryMetadata
	firstEditArgs := map[string]interface{}{"prompt": "purple"}
	ancestorArgs := map[string]interface{}{"prompt": "retry ancestor"}
	variantArgs := map[string]interface{}{"prompt": "improve blue"}
	failureArgs := map[string]interface{}{"prompt": "failed retry"}
	chat.onChat = func(req llm.ChatRequest) {
		for _, target := range []struct {
			call string
			args map[string]interface{}
		}{{"1", firstEditArgs}, {"1", ancestorArgs}, {"4", variantArgs}, {"5", failureArgs}} {
			if id := generatedResultID(req.Messages, target.call); id != "" {
				target.args["image_url"] = id
			}
		}
	}
	var ancestor, originalLogical, variantLogical string
	count := 0
	handler := func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		count++
		sources := requestctx.InputImagesFromContext(ctx)
		if count == 2 {
			for _, source := range sources {
				if source.Path == firstEditArgs["image_url"] {
					ancestor, originalLogical = source.ID, source.ImageID
				}
			}
		}
		if count == 4 {
			if sources[0].Version != 3 || sources[0].ParentSourceImageID != ancestor || sources[0].ImageID != originalLogical {
				t.Fatal("ancestor retry lost version or parent")
			}
		}
		if count == 5 {
			variantLogical = sources[0].ImageID
			if variantLogical == originalLogical || sources[0].Version != 1 || sources[0].ParentSourceImageID != "" {
				t.Fatal("text generation did not create a new logical image")
			}
		}
		if count == 6 {
			return governance.Result{}, errors.New("synthetic failed edit")
		}
		image := testInputImage(t, 2+count, 3+count)
		data, _ := base64.StdEncoding.DecodeString(image.Data)
		return governance.Result{Content: `{"status":"generated"}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("image-%d.jpg", count), MIMEType: image.MimeType, Data: data}}}, nil
	}
	if err := registerTestTool(t, reg, testToolSpec{Name: imagegenerate.Name}, policy, handler); err != nil {
		t.Fatal(err)
	}
	if err := registerTestTool(t, reg, testToolSpec{Name: "report", Description: "report"}, policy, func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: "report", Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "report.txt", MIMEType: "text/plain", Data: []byte("report")}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	chat.responses = []*llm.ChatResponse{
		toolCallResponse("1", imagegenerate.Name, map[string]interface{}{"prompt": "car"}),
		toolCallResponse("2", imagegenerate.Name, firstEditArgs),
		toolCallResponse("3", imagegenerate.Name, ancestorArgs),
		toolCallResponse("report", "report", nil),
		toolCallResponse("4", imagegenerate.Name, map[string]interface{}{"prompt": "blue alternative"}),
		toolCallResponse("5", imagegenerate.Name, variantArgs),
		toolCallResponse("6", imagegenerate.Name, failureArgs),
		{Message: llm.ChatMessage{Role: "assistant", Content: "Two alternatives and report."}},
	}
	response, err := processAgent(a, "versions", "discord", "session", "user-1", "User", "make alternatives", nil, func(chunk StreamChunk) {
		if len(chunk.Attachments) > 0 {
			t.Fatal("attachment streamed before final")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 6 || len(response.Attachments) != 3 {
		t.Fatalf("calls=%d attachments=%d", count, len(response.Attachments))
	}
	for i, want := range []string{"image-3.jpg", "report.txt", "image-5.jpg"} {
		if response.Attachments[i].Filename != want {
			t.Fatalf("slot %d=%s", i, response.Attachments[i].Filename)
		}
	}
	images, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
	if err != nil || len(images) != 2 {
		t.Fatalf("images=%d err=%v", len(images), err)
	}
	if images[0].ImageID != variantLogical || images[0].Version != 2 || images[0].VersionHighwater != 3 || images[1].ImageID != originalLogical || images[1].Version != 3 {
		t.Fatal("wrong final metadata")
	}
	chat.responses = []*llm.ChatResponse{toolCallResponse("next", imagegenerate.Name, map[string]interface{}{"prompt": "next turn", "image_url": generatedResultID(chat.requests[len(chat.requests)-1].Messages, "5")}), {Message: llm.ChatMessage{Role: "assistant", Content: "Updated."}}}
	response, err = processAgent(a, "next", "discord", "session", "user-1", "User", "edit", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	images, err = store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
	if err != nil || images[0].ImageID != variantLogical || images[0].Version != 4 {
		t.Fatal("cross-turn version highwater reused")
	}
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "No new images."}}}
	response, err = processAgent(a, "no-generation", "discord", "session", "user-1", "User", "describe", nil, nil)
	if err != nil || len(response.Attachments) != 0 {
		t.Fatal("loaded source was automatically delivered")
	}
}

func TestGeneratedImageCapacityRejectsBeforeProvider(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _ := newTestAgent(t, chat, nil, reg)
	policy := testToolPolicy()
	policy.MaxExecutions = 0
	calls := 0
	image := testInputImage(t, 2, 3)
	data, _ := base64.StdEncoding.DecodeString(image.Data)
	if err := registerTestTool(t, reg, testToolSpec{Name: imagegenerate.Name, Description: "generate"}, policy, func(context.Context, map[string]interface{}) (governance.Result, error) {
		calls++
		return governance.Result{Content: `{}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("%d.jpg", calls), MIMEType: image.MimeType, Data: data}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		chat.responses = append(chat.responses, toolCallResponse(fmt.Sprint(i), imagegenerate.Name, map[string]interface{}{"prompt": fmt.Sprint(i)}))
	}
	chat.responses = append(chat.responses, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Four images."}})
	response, err := processAgent(a, "capacity", "discord", "session", "user-1", "User", "generate", nil, nil)
	if err != nil || calls != 4 || len(response.Attachments) != 4 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	result := toolResultByID(chat.requests[len(chat.requests)-1].Messages, "4")
	if result == nil || !strings.Contains(result.Content, "at most four logical images") {
		t.Fatal("missing actionable capacity feedback")
	}
}

func TestGeneratedImagesFeedSuccessiveTextOnlyEdits(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			chat := &fakeChatter{}
			reg := registry.New(config.NewLogger(config.LevelError))
			a, store := newTestAgent(t, chat, nil, reg)
			var logs bytes.Buffer
			level := config.LevelDebug
			if streaming {
				level = config.LevelDebug
			}
			a.log = config.NewLogger(level)
			a.log.SetOutput(&logs)
			var previous string
			count := 0
			handler := func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
				sources := requestctx.InputImagesFromContext(ctx)
				if count > 0 && (len(sources) == 0 || sources[0].Data != previous) {
					t.Fatal("edit did not receive preceding generated bytes")
				}
				image := testInputImage(t, 2+count, 3+count)
				data, err := base64.StdEncoding.DecodeString(image.Data)
				if err != nil {
					t.Fatal(err)
				}
				normalized, err := media.NormalizeInputImageFromBytes(nil, image.MimeType, data, "generated")
				if err != nil {
					t.Fatal(err)
				}
				previous = normalized.Image.Data
				count++
				return governance.Result{Content: `{"status":"generated"}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("image-%d.jpg", count), MIMEType: image.MimeType, Data: data}}}, nil
			}
			policy := testToolPolicy()
			policy.History.Mode = governance.HistoryMetadata
			if err := registerTestTool(t, reg, testToolSpec{Name: imagegenerate.Name}, policy, handler); err != nil {
				t.Fatal(err)
			}
			for i, prompt := range []string{"generate a car", "make it blue", "make it purple"} {
				args := map[string]interface{}{"prompt": prompt}
				if i > 0 {
					assets, err := store.SessionImages(context.Background(), "user-1", "session", 1)
					if err != nil || len(assets) == 0 {
						t.Fatalf("source images=%d err=%v", len(assets), err)
					}
					args["image_url"] = generatedResultID(chat.requests[len(chat.requests)-1].Messages, "generate")
				}
				chat.responses = append(chat.responses, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "generate", Function: llm.ToolFunction{Name: imagegenerate.Name, Arguments: args}}}}}, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Here it is."}})
				var callback func(StreamChunk)
				if streaming {
					callback = func(StreamChunk) {}
				}
				response, err := processAgent(a, fmt.Sprintf("request-%d", i), "discord", "session", "user-1", "User", prompt, nil, callback)
				if err != nil || response.SourceTurnID == 0 {
					t.Fatalf("response persisted=%v err=%v", response != nil, err)
				}
				final := chat.requests[len(chat.requests)-1]
				if !final.Stream {
					t.Fatal("model transport depends on progress callback")
				}
				last := final.Messages[len(final.Messages)-1]
				if len(last.Images) != 1 || last.Images[0].Data != previous || last.Role != "tool" || !strings.Contains(last.Content, "Image generated successfully") {
					t.Fatal("final model call did not see normalized output")
				}
				assets, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
				if err != nil || len(assets) != i+1 || assets[0].Data != previous {
					t.Fatalf("retained images=%d err=%v", len(assets), err)
				}
				turns, err := store.RecentSessionTurns("user-1", "session", response.SessionGeneration, 10)
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(turns)
				if strings.Contains(string(encoded), previous) {
					t.Fatal("base64 leaked into durable turn or tool history")
				}
				if strings.Contains(logs.String(), previous) || strings.Contains(logs.String(), prompt) || strings.Contains(logs.String(), assets[0].ID) || strings.Contains(logs.String(), assets[0].ImageID) {
					t.Fatal("private image data leaked into logs")
				}
			}
			for _, event := range []string{"agent.images.loaded", "agent.images.generated", "agent.images.stored"} {
				if strings.Count(logs.String(), `"event":"`+event+`"`) != 3 {
					t.Fatalf("wrong emission count for %s", event)
				}
			}
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var record map[string]interface{}
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["event"] != "agent.images.generated" {
					continue
				}
				for _, key := range []string{"image_count", "selected_image_count", "catalog_image_count"} {
					if _, ok := record[key].(float64); !ok {
						t.Fatalf("missing numeric %s", key)
					}
				}
			}
		})
	}
}

func TestGeneratedImageContextSurvivesCompaction(t *testing.T) {
	compactor := &fakeForegroundCompactor{artifact: memory.SummaryArtifact{Narrative: "Generated an image."}}
	state := newForegroundCompactionState(compactor, 100, "policy", "", "edit it", nil, nil, []memory.SessionTurn{{ID: 1, UserText: "old", AssistantText: "answer"}}, nil)
	image := requestctx.InputImage{ID: "opaque-id", Path: "/private/image.png", MIMEType: "image/png", Data: "image-payload", Source: "generated"}
	message := llm.ChatMessage{Role: "tool", ToolName: imagegenerate.Name, ToolCallID: "generated", Content: imagegenerate.ResultText(image.Path, nil, false), Images: []llm.InputImage{{MimeType: image.MIMEType, Data: image.Data, Source: "generated"}}}
	assistant := llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "generated", Function: llm.ToolFunction{Name: imagegenerate.Name, Arguments: map[string]interface{}{"prompt": "picture"}}}}}
	messages := []llm.ChatMessage{assistant, message}
	state.imageToolRounds = retainedImageToolRounds(messages)
	rebuilt, stats, err := state.prepare(context.Background(), messages, nil, true)
	if err != nil || !stats.Compacted {
		t.Fatalf("compacted=%v err=%v", stats.Compacted, err)
	}
	last := rebuilt[len(rebuilt)-1]
	if len(last.Images) != 1 || last.Images[0].Data != image.Data || !strings.Contains(last.Content, image.Path) || strings.Contains(last.Content, image.ID) || last.Role != "tool" || last.ToolCallID != "generated" || rebuilt[len(rebuilt)-2].ToolCalls[0].ID != "generated" {
		t.Fatal("compaction lost active image")
	}
	encoded, _ := json.Marshal(compactor.calls)
	if strings.Contains(string(encoded), image.Data) {
		t.Fatal("image bytes entered compaction debt")
	}
}

func TestGeneratedImagesChainWithinBatchAndReachToolsDisabledFinal(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	a.toolPolicy = governance.GlobalPolicy{MaxExecutions: 6, MaxToolIterations: 10}
	current := testInputImage(t, 2, 3)
	previous := current.Data
	count := 0
	policy := testToolPolicy()
	policy.MaxExecutions = 0
	policy.History.Mode = governance.HistoryMetadata
	err := registerTestTool(t, reg, testToolSpec{Name: imagegenerate.Name, Description: "edit"}, policy, func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		sources := requestctx.InputImagesFromContext(ctx)
		if len(sources) == 0 || sources[0].Data != previous {
			t.Fatal("latest generated image missing from context within batch")
		}
		var currentPath string
		for _, source := range sources {
			if source.ID == "current-1" {
				currentPath = source.Path
			}
		}
		if currentPath == "" || args["image_url"] != currentPath {
			t.Fatal("explicit current image selector changed")
		}
		image := testInputImage(t, 3+count, 4+count)
		data, _ := base64.StdEncoding.DecodeString(image.Data)
		normalized, err := media.NormalizeInputImageFromBytes(nil, image.MimeType, data, "generated")
		if err != nil {
			t.Fatal(err)
		}
		previous = normalized.Image.Data
		count++
		return governance.Result{Content: `{"status":"generated"}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("image-%d.jpg", count), MIMEType: image.MimeType, Data: data}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls []llm.ToolCall
	for i := 0; i < 6; i++ {
		args := map[string]interface{}{"prompt": fmt.Sprintf("edit %d", i)}
		calls = append(calls, llm.ToolCall{ID: fmt.Sprint(i), Function: llm.ToolFunction{Name: imagegenerate.Name, Arguments: args}})
	}
	chat.onChat = func(req llm.ChatRequest) {
		if calls[0].Function.Arguments["image_url"] != nil {
			return
		}
		for _, call := range calls {
			for _, message := range req.Messages {
				if paths := attachedImagePaths(message.Content); len(paths) > 0 {
					call.Function.Arguments["image_url"] = paths[0]
				}
			}
		}
	}
	chat.responses = []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", ToolCalls: calls}}, {Message: llm.ChatMessage{Role: "assistant", Content: "Final image."}}}
	response, err := processAgent(a, "batch", "discord", "session", "user-1", "User", "edit repeatedly", []llm.InputImage{current}, nil)
	if err != nil || response == nil || count != 6 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	final := chat.requests[len(chat.requests)-1]
	if len(final.Tools) != 0 {
		t.Fatal("final tools not disabled")
	}
	var previews []llm.InputImage
	for _, message := range final.Messages {
		if message.Role == "tool" && message.ToolName == imagegenerate.Name {
			previews = append(previews, message.Images...)
		}
	}
	if len(previews) != 4 || previews[3].Data != previous {
		t.Fatal("latest four generated outputs not retained in tool results")
	}
	results := 0
	for _, message := range final.Messages {
		if message.Role == "tool" {
			results++
		}
	}
	if results != 6 {
		t.Fatalf("correlated tool results=%d", results)
	}
	assets, err := store.SessionImages(context.Background(), "user-1", "session", response.SessionGeneration)
	if err != nil || len(assets) != 1 || assets[0].Data != previous || assets[0].Version != 6 || len(response.Attachments) != 1 {
		t.Fatalf("assets=%d err=%v", len(assets), err)
	}
}

func TestGeneratedImageCannotSucceedAfterSessionEndBeforePersistence(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{toolCallResponse("generate", imagegenerate.Name, nil), {Message: llm.ChatMessage{Role: "assistant", Content: "Here it is."}}}}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store := newTestAgent(t, chat, nil, reg)
	image := testInputImage(t, 2, 3)
	data, _ := base64.StdEncoding.DecodeString(image.Data)
	if err := registerTestTool(t, reg, testToolSpec{Name: imagegenerate.Name, Description: "generate"}, testToolPolicy(), func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
		if err := store.NewSessionContext(ctx, "user-1", "session"); err != nil {
			t.Fatal(err)
		}
		return governance.Result{Content: `{"status":"generated"}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "output.jpg", MIMEType: image.MimeType, Data: data}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	response, err := processAgent(a, "new-session", "discord", "session", "user-1", "User", "generate", nil, nil)
	if err == nil || response != nil {
		t.Fatal("unpersisted generated image reported as delivered")
	}
	var count int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM state_meta WHERE key LIKE 'oswald:v1:turn:%' AND json_array_length(value,'$.images')>0`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unfenced images=%d err=%v", count, err)
	}
}
