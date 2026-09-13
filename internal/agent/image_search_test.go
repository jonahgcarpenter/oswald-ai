package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestSearchReferenceMeasurementsAreSafe(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var output bytes.Buffer
			root := config.NewLogger(level)
			root.SetOutput(&output)
			log := root.Agent("agent", "reference-request", "user-1", "discord", "test-model")
			state := requestctx.NewImageSearchState()
			refs, err := state.AddReferences([]requestctx.ImageSearchReference{{Title: "private-title", SourceURL: "https://private.example", MIMEType: "image/png", Data: "private-bytes"}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := requestctx.WithImageSearchState(context.Background(), state)
			message := searchImageContext(refs)
			messages := []llm.ChatMessage{message}
			markSearchImagesInspected(ctx, messages, messages, log)
			compaction := &foregroundCompactionState{inputLimit: 1, searchContext: &message, log: log}
			fitted := compaction.fitSearchImages(ctx, messages, nil)
			markSearchImagesInspected(ctx, fitted, fitted, log)
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != 2 || strings.Contains(output.String(), "private") || strings.Contains(output.String(), refs[0].ID) {
				t.Fatal("incorrect reference measurement count or private data exposure")
			}
			for i, event := range []string{"agent.images.references.inspected", "agent.images.references.omitted"} {
				var record map[string]interface{}
				if err := json.Unmarshal([]byte(lines[i]), &record); err != nil {
					t.Fatal(err)
				}
				if record["event"] != event || record["level"] != "info" || record["request_id"] != "reference-request" || record["image_count"] != float64(1-i) {
					t.Fatalf("unexpected measurement: %v", record)
				}
			}
		})
	}
}

func newImageSearchTestAgent(t *testing.T, chat *fakeChatter, reg *registry.Registry) (*Agent, *agentMemoryFixture, requestctx.ImageSearchReference) {
	t.Helper()
	image := testInputImage(t, 64, 64)
	ref := requestctx.ImageSearchReference{Title: "Synthetic bird", SourceURL: "https://example.org/bird", MIMEType: image.MimeType, Data: image.Data}
	policy := governance.ToolPolicy{BlockDuplicates: true, History: governance.HistoryPolicy{Mode: governance.HistoryMetadata}}
	if err := registerTestTool(t, reg, registry.Spec{Name: toolnames.WebImageSearch}, policy, func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
		state := requestctx.ImageSearchStateFromContext(ctx)
		if err := state.ReserveSearch(); err != nil {
			return governance.Result{}, err
		}
		refs, err := state.AddReferences([]requestctx.ImageSearchReference{ref})
		if err != nil {
			return governance.Result{}, err
		}
		return productiveResult(`{"result_id":"` + refs[0].ID + `","title":"Synthetic bird"}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registerTestTool(t, reg, registry.Spec{Name: toolnames.WebImageSelect}, policy, registry.Handler(websearch.NewImageSelectHandler())); err != nil {
		t.Fatal(err)
	}
	a, store := newTestAgent(t, chat, nil, reg)
	return a, store, ref
}

func searchReferenceImages(req llm.ChatRequest) []llm.InputImage {
	var images []llm.InputImage
	for _, message := range req.Messages {
		for _, image := range message.Images {
			if strings.HasPrefix(image.Source, "search-reference:") {
				images = append(images, image)
			}
		}
	}
	return images
}

func TestImageSearchUserSourcePrefixCannotAuthorizeOmittedReference(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("search", toolnames.WebImageSearch, map[string]interface{}{"query": "bird"}),
		toolCallResponse("select", toolnames.WebImageSelect, map[string]interface{}{"result_id": "search-1"}),
		{Message: llm.ChatMessage{Role: "assistant", Content: "No inspected preview."}},
	}}
	a, _, ref := newImageSearchTestAgent(t, chat, registry.New(config.NewLogger(config.LevelError)))
	a.budget.PromptLimit = 1
	attached := testInputImage(t, 9, 6)
	attached.Source = "search-reference:search-1"
	if attached.Data == ref.Data {
		t.Fatal("spoofed source must have different bytes from the catalog")
	}
	response, err := processAgent(a, "spoof", "discord", "session", "user-1", "User", "find a bird", []llm.InputImage{attached}, nil)
	if err != nil || response == nil || response.Error != "" || len(response.Attachments) != 0 || len(chat.requests) != 3 {
		t.Fatalf("response=%+v requests=%d err=%v", response, len(chat.requests), err)
	}
	for _, req := range chat.requests[1:] {
		images := searchReferenceImages(req)
		if len(images) != 1 || images[0].Data != attached.Data || images[0].MimeType != attached.MimeType {
			t.Fatal("budget must omit the catalog preview while retaining the unrelated user image")
		}
	}
	if !messagesContain(chat.requests[2].Messages, "select an image result inspected") {
		t.Fatal("user-controlled source prefix authorized an uninspected catalog image")
	}
}

func TestImageSearchRetrySelectsSuccessfulResizedBytes(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{response: toolCallResponse("search", toolnames.WebImageSearch, map[string]interface{}{"query": "bird"})},
		{err: &llm.ChatHTTPError{StatusCode: 500, Body: "model runner has unexpectedly stopped"}},
		{response: toolCallResponse("select", toolnames.WebImageSelect, map[string]interface{}{"result_id": "search-1"})},
		{err: &llm.ChatHTTPError{StatusCode: 500, Body: "model runner has unexpectedly stopped"}},
		{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "A bird."}}},
	}}
	a, _, ref := newImageSearchTestAgent(t, chat, registry.New(config.NewLogger(config.LevelError)))
	response, err := processAgent(a, "resize", "discord", "session", "user-1", "User", "find a bird", nil, nil)
	if err != nil || response == nil || response.Error != "" || len(response.Attachments) != 1 || len(chat.requests) != 5 {
		t.Fatalf("response=%+v requests=%d err=%v", response, len(chat.requests), err)
	}
	failed := searchReferenceImages(chat.requests[1])
	successful := searchReferenceImages(chat.requests[2])
	later := searchReferenceImages(chat.requests[4])
	if len(failed) != 1 || len(successful) != 1 || len(later) != 1 || failed[0].Data != ref.Data || successful[0].Data == ref.Data || later[0].Data == successful[0].Data {
		t.Fatal("expected original failed attempt and distinct resized successes before and after selection")
	}
	want, err := base64.StdEncoding.DecodeString(successful[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response.Attachments[0].Data, want) || response.Attachments[0].MIMEType != successful[0].MimeType {
		t.Fatal("selection did not freeze the exact successful resized input across later inspection")
	}
}

func TestImageSearchCompactionRetainsReferencesAndCorrectiveRetryFitsWithoutDebt(t *testing.T) {
	// First measure the compacted request, then leave room for its preview but
	// not the extra corrective prompt. Both runs exercise the production loop.
	limit := 5000
	for _, narrow := range []bool{false, true} {
		name := "compaction_retains_preview"
		if narrow {
			name = "corrective_omits_preview_without_new_compaction"
		}
		t.Run(name, func(t *testing.T) {
			chat := &fakeChatter{}
			reg := registry.New(config.NewLogger(config.LevelError))
			a, _, ref := newImageSearchTestAgent(t, chat, reg)
			largeResult := strings.Repeat("synthetic research detail ", 20000)
			if err := registerTestTool(t, reg, registry.Spec{Name: "test.large"}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
				return productiveResult(largeResult), nil
			}); err != nil {
				t.Fatal(err)
			}
			batch := toolCallResponse("search", toolnames.WebImageSearch, map[string]interface{}{"query": "bird"})
			batch.Message.ToolCalls = append(batch.Message.ToolCalls, toolCallResponse("large", "test.large", nil).Message.ToolCalls...)
			chat.responses = []*llm.ChatResponse{batch,
				{Message: llm.ChatMessage{Role: "assistant"}},
				{Message: llm.ChatMessage{Role: "assistant", Content: "A bird."}},
			}
			a.budget.PromptLimit = limit
			a.toolPolicy = governance.GlobalPolicy{MaxExecutions: 2, MaxToolIterations: 10}
			compactor := &fakeForegroundCompactor{artifact: memory.SummaryArtifact{Narrative: "Research completed."}}
			a.SetForegroundCompactor(compactor)
			response, err := processAgent(a, "fit", "discord", "session", "user-1", "User", "find a bird", nil, nil)
			if err != nil || response == nil || response.Error != "" || len(chat.requests) != 3 || len(compactor.calls) != 1 {
				t.Fatalf("response=%+v requests=%d compactions=%d err=%v", response, len(chat.requests), len(compactor.calls), err)
			}
			compacted := chat.requests[1]
			refs := searchReferenceImages(compacted)
			if !messagesContain(compacted.Messages, "active_turn_summary") || len(refs) != 1 || refs[0].Data != ref.Data || budget.EstimateRequest(compacted.Messages, compacted.Tools) > limit {
				t.Fatal("compaction did not retain the newly affordable optional reference")
			}
			if budget.EstimateRequest([]llm.ChatMessage{{Role: "tool", Content: largeResult}}, nil) <= limit {
				t.Fatal("fixture did not exceed the precompaction budget")
			}
			corrective := chat.requests[2]
			if len(corrective.Tools) != 0 || !messagesContain(corrective.Messages, emptyResponseRetryPrompt) {
				t.Fatal("missing tools-disabled empty-response corrective call")
			}
			if narrow {
				unfitted := append(append([]llm.ChatMessage(nil), compacted.Messages...), llm.ChatMessage{Role: "user", Content: emptyResponseRetryPrompt})
				if budget.EstimateRequest(unfitted, nil) <= limit {
					t.Fatal("corrective prompt did not cross capacity")
				}
				if len(searchReferenceImages(corrective)) != 0 || budget.EstimateRequest(corrective.Messages, nil) > limit {
					t.Fatal("no-debt corrective call discarded fitted messages when Compacted was false")
				}
			} else {
				limit = budget.EstimateRequest(compacted.Messages, compacted.Tools)
			}
		})
	}
}

func TestImageSearchInspectionDeliveryAndRequestIsolation(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, store, ref := newImageSearchTestAgent(t, chat, reg)
	probeCalls := 0
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.sources"}, testToolPolicy(), func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
		probeCalls++
		if len(requestctx.InputImagesFromContext(ctx)) != 0 {
			t.Fatal("found preview became an automatic edit source")
		}
		state := requestctx.ImageSearchStateFromContext(ctx)
		if state == nil || (probeCalls == 2 && (len(state.ActiveReferences()) != 0 || len(state.SelectedReferences()) != 0)) {
			t.Fatal("next Process did not create an empty image search state")
		}
		return productiveResult("no edit sources"), nil
	}); err != nil {
		t.Fatal(err)
	}
	search := toolCallResponse("search", toolnames.WebImageSearch, map[string]interface{}{"query": "bird"})
	search.Message.ToolCalls = append(search.Message.ToolCalls, toolCallResponse("premature", toolnames.WebImageSelect, map[string]interface{}{"result_id": "search-1"}).Message.ToolCalls...)
	selectImage := toolCallResponse("select", toolnames.WebImageSelect, map[string]interface{}{"result_id": "search-1"})
	selectImage.Message.ToolCalls = append(selectImage.Message.ToolCalls, toolCallResponse("probe", "test.sources", nil).Message.ToolCalls...)
	chat.responses = []*llm.ChatResponse{search, selectImage, {Message: llm.ChatMessage{Role: "assistant", Content: "A bird."}}}
	response, err := processAgent(a, "search", "discord", "session", "user-1", "User", "find a bird", nil, nil)
	if err != nil || response == nil || response.Error != "" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if len(chat.requests) != 3 || len(searchReferenceImages(chat.requests[0])) != 0 {
		t.Fatal("unexpected request count or premature visual reference")
	}
	for _, req := range chat.requests[1:] {
		images := searchReferenceImages(req)
		if len(images) != 1 || images[0].Data != ref.Data || images[0].MimeType != ref.MIMEType {
			t.Fatal("next model round lost normalized preview")
		}
	}
	// The reference must follow every result in the model's correlated batch.
	results := map[string]int{}
	previewIndex := -1
	for i, message := range chat.requests[1].Messages {
		if message.Role == "tool" {
			results[message.ToolCallID] = i
			if message.ToolCallID == "premature" && !strings.Contains(message.Content, "inspected") {
				t.Fatalf("same-batch select was not rejected: %s", message.Content)
			}
		}
		if len(message.Images) > 0 {
			previewIndex = i
		}
	}
	_, hasSearch := results["search"]
	_, hasPremature := results["premature"]
	if len(results) != 2 || !hasSearch || !hasPremature || previewIndex <= results["search"] || previewIndex <= results["premature"] {
		t.Fatalf("reference interleaved with tool results: results=%v preview=%d", results, previewIndex)
	}
	data, err := base64.StdEncoding.DecodeString(ref.Data)
	if err != nil {
		t.Fatal(err)
	}
	ext := ".jpg"
	if ref.MIMEType == "image/png" {
		ext = ".png"
	}
	filename := "found-preview-search-1" + ext
	want := "A bird."
	if response.Response != want || len(response.Attachments) != 1 || response.Attachments[0].Filename != filename || response.Attachments[0].MIMEType != ref.MIMEType || !bytes.Equal(response.Attachments[0].Data, data) {
		t.Fatal("selection changed normalized bytes or appended text to the model response")
	}
	turns, err := store.RecentSessionTurns("user-1", "session", response.SessionGeneration, 10)
	if err != nil || len(turns) != 1 || turns[0].AssistantText != want {
		t.Fatalf("delivered turn missing: count=%d err=%v", len(turns), err)
	}
	history, err := json.Marshal(turns[0].ToolHistory)
	if err != nil || bytes.Contains(history, []byte(ref.Data)) || bytes.Contains(history, []byte("Synthetic bird")) || bytes.Contains(history, []byte("sourced_thumbnail_preview")) || !bytes.Contains(history, []byte(toolnames.WebImageSelect)) {
		t.Fatalf("metadata-only history contract violated: %s err=%v", history, err)
	}
	var count int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM session_images`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("found previews persisted as session images: count=%d err=%v", count, err)
	}
	chat.requests = nil
	chat.responses = []*llm.ChatResponse{selectImage, {Message: llm.ChatMessage{Role: "assistant", Content: "Search again."}}}
	response, err = processAgent(a, "next", "discord", "session", "user-1", "User", "send it again", nil, nil)
	if err != nil || response == nil || len(response.Attachments) != 0 || probeCalls != 2 {
		t.Fatalf("next request reused selection: response=%+v probes=%d err=%v", response, probeCalls, err)
	}
	for _, req := range chat.requests {
		if len(searchReferenceImages(req)) != 0 {
			t.Fatal("next Process reused visual catalog")
		}
	}
	if !messagesContain(chat.requests[1].Messages, "select an image result inspected") {
		t.Fatal("stale catalog ID was not rejected")
	}
}

func TestImageSearchModelPaths(t *testing.T) {
	for _, mode := range []string{"research", "homeassistant", "parser_retry", "tiny_budget", "tools_disabled", "corrective", "failure", "canceled", "corrective_failure", "tools_disabled_failure"} {
		t.Run(mode, func(t *testing.T) {
			chat := &fakeChatter{}
			reg := registry.New(config.NewLogger(config.LevelError))
			a, store, ref := newImageSearchTestAgent(t, chat, reg)
			chat.outcomes = []fakeChatOutcome{{response: toolCallResponse("search", toolnames.WebImageSearch, map[string]interface{}{"query": "bird"})}}
			if mode == "parser_retry" {
				chat.outcomes = append(chat.outcomes, fakeChatOutcome{err: &llm.ChatHTTPError{StatusCode: 500, Body: "XML syntax error on line 7: unexpected EOF"}})
			}
			selects := mode != "research" && mode != "homeassistant" && mode != "tools_disabled" && mode != "corrective"
			if selects {
				chat.outcomes = append(chat.outcomes, fakeChatOutcome{response: toolCallResponse("select", toolnames.WebImageSelect, map[string]interface{}{"result_id": "search-1"})})
			}
			if mode == "tiny_budget" {
				a.budget.PromptLimit = 1
			}
			if strings.HasPrefix(mode, "tools_disabled") {
				limit := 1
				if selects {
					limit = 2
				}
				a.toolPolicy = governance.GlobalPolicy{MaxExecutions: limit, MaxToolIterations: 10}
			}
			if strings.HasPrefix(mode, "corrective") {
				chat.outcomes = append(chat.outcomes, fakeChatOutcome{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant"}}})
			}
			failure := mode == "failure" || strings.HasSuffix(mode, "_failure")
			switch {
			case failure:
				chat.outcomes = append(chat.outcomes, fakeChatOutcome{err: errors.New("synthetic model failure")})
			case mode == "canceled":
				chat.outcomes = append(chat.outcomes, fakeChatOutcome{err: context.Canceled})
			default:
				chat.outcomes = append(chat.outcomes, fakeChatOutcome{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Finished."}}})
			}
			gateway := "discord"
			if mode == "homeassistant" {
				gateway = mode
			}
			response, err := processAgent(a, mode, gateway, "session", "user-1", "User", "research a bird", nil, nil)
			if mode == "canceled" {
				var count int
				if !errors.Is(err, context.Canceled) || response != nil {
					t.Fatalf("canceled response=%+v err=%v", response, err)
				}
				if err := store.sql.QueryRow(`SELECT COUNT(*) FROM session_turns`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("cancellation published turn: count=%d err=%v", count, err)
				}
				return
			}
			if err != nil || response == nil || response.Error != "" || len(chat.outcomes) != 0 {
				t.Fatalf("response=%+v remaining=%d err=%v", response, len(chat.outcomes), err)
			}
			wantAttachment := selects && mode != "tiny_budget"
			if wantAttachment {
				data, _ := base64.StdEncoding.DecodeString(ref.Data)
				if len(response.Attachments) != 1 || !bytes.Equal(response.Attachments[0].Data, data) {
					t.Fatal("selected preview lost")
				}
			} else if len(response.Attachments) != 0 || strings.Contains(response.Response, "Found thumbnail preview") {
				t.Fatal("unselected research preview delivered")
			}
			if !failure && response.Response != "Finished." {
				t.Fatal("appended text to the model response")
			}
			if failure && (response.Kind != "image_partial" || response.Response != foundImagePartialResponse || response.SourceTurnID == 0 || response.PersistenceStatus != "pending") {
				t.Fatalf("selected preview failure not finalized: %+v", response)
			}
			for _, req := range chat.requests[1:] {
				images := searchReferenceImages(req)
				if mode == "tiny_budget" {
					if len(images) != 0 {
						t.Fatal("optional preview exceeded tiny budget")
					}
				} else if len(images) != 1 || images[0].Data != ref.Data {
					t.Fatal("model continuation lost search reference")
				}
			}
			if mode == "tiny_budget" && !messagesContain(chat.requests[2].Messages, "select an image result inspected") {
				t.Fatal("omitted preview authorized selection")
			}
			if mode == "homeassistant" {
				for _, req := range chat.requests {
					if !requestHasTool(req, toolnames.WebImageSearch) || requestHasTool(req, toolnames.WebImageSelect) {
						t.Fatal("Home Assistant image-tool exposure is wrong")
					}
				}
			}
			if strings.HasPrefix(mode, "tools_disabled") || strings.HasPrefix(mode, "corrective") {
				if len(chat.requests[len(chat.requests)-1].Tools) != 0 {
					t.Fatal("final/corrective call still advertises tools")
				}
			}
			if mode == "parser_retry" && !reflect.DeepEqual(chat.requests[1], chat.requests[2]) {
				t.Fatal("parser retry changed inspection input")
			}
		})
	}
}

func TestImageSearchForcedCompactionPreservesSeparateReferences(t *testing.T) {
	chat := &fakeChatter{}
	reg := registry.New(config.NewLogger(config.LevelError))
	a, _, ref := newImageSearchTestAgent(t, chat, reg)
	generated := testInputImage(t, 9, 6)
	data, err := base64.StdEncoding.DecodeString(generated.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerTestTool(t, reg, registry.Spec{Name: toolnames.ComfyUITextToImage}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: `{}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "generated.jpg", MIMEType: generated.MimeType, Data: data}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	batch := toolCallResponse("search", toolnames.WebImageSearch, map[string]interface{}{"query": "bird"})
	batch.Message.ToolCalls = append(batch.Message.ToolCalls, toolCallResponse("generate", toolnames.ComfyUITextToImage, map[string]interface{}{"prompt": "bird"}).Message.ToolCalls...)
	chat.outcomes = []fakeChatOutcome{
		{response: batch},
		{err: &llm.ChatHTTPError{StatusCode: 400, Body: "maximum context length exceeded"}},
		{response: toolCallResponse("select", toolnames.WebImageSelect, map[string]interface{}{"result_id": "search-1"})},
		{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Found and generated."}}},
	}
	compactor := &fakeForegroundCompactor{artifact: memory.SummaryArtifact{Narrative: "Searched and generated a bird."}}
	a.SetForegroundCompactor(compactor)
	response, err := processAgent(a, "compact", "discord", "session", "user-1", "User", "find and generate a bird", nil, nil)
	if err != nil || response == nil || response.Error != "" || len(response.Attachments) != 2 || len(compactor.calls) != 1 || len(chat.requests) != 4 {
		t.Fatalf("response=%+v compactions=%d requests=%d err=%v", response, len(compactor.calls), len(chat.requests), err)
	}
	var generatedReference llm.InputImage
	for _, message := range chat.requests[1].Messages {
		for _, image := range message.Images {
			if !strings.HasPrefix(image.Source, "search-reference:") {
				generatedReference = image
			}
		}
	}
	if generatedReference.Data == "" {
		t.Fatal("generated reference missing before compaction")
	}
	for _, req := range chat.requests[2:] {
		if !messagesContain(req.Messages, "active_turn_summary") {
			t.Fatal("forced compaction did not install checkpoint")
		}
		refs := searchReferenceImages(req)
		if len(refs) != 1 || refs[0].Data != ref.Data {
			t.Fatal("compaction lost search reference")
		}
		generatedMessages := 0
		for _, message := range req.Messages {
			for _, image := range message.Images {
				if !strings.HasPrefix(image.Source, "search-reference:") {
					generatedMessages++
					if message.Role != "user" || len(message.Images) != 1 || !reflect.DeepEqual(image, generatedReference) {
						t.Fatal("generated and search reference contexts were conflated")
					}
				}
			}
		}
		if generatedMessages != 1 {
			t.Fatalf("generated reference count=%d", generatedMessages)
		}
	}
}
