package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

type summaryFakeChatter struct {
	arguments map[string]interface{}
	request   llm.ChatRequest
	response  *llm.ChatResponse
	err       error
}

func (f *summaryFakeChatter) Chat(_ context.Context, request llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.request = request
	if f.err != nil {
		return nil, f.err
	}
	if f.response != nil {
		return f.response, nil
	}
	return &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{
		Function: llm.ToolFunction{Name: sessionSummarySaveToolName, Arguments: f.arguments},
	}}}}, nil
}

func TestLLMExtractorParsesStructuredSummaryAndCandidate(t *testing.T) {
	content := `{"narrative":"Atlas is active.","open_tasks":["ship"],"commitments":[],"entities":["Atlas"],"decisions":[],"topic_tags":["project"],"candidates":[{"source_turn_id":4,"statement":"The user works on Atlas.","evidence":"I work on Atlas.","scope":"long_term","category":"projects","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":4,"ttl_days":0,"supersedes":"","claim_slot":"project.name","claim_value":"Atlas"}]}`
	client := &summaryFakeChatter{arguments: summaryArguments(t, content)}
	extractor := NewLLMExtractor(client, "model", 2048)
	history := usermemory.ToolHistory{Version: usermemory.ToolHistoryVersion, Batches: []usermemory.ToolHistoryBatch{{Calls: []usermemory.ToolHistoryCall{{Name: "project.lookup", Status: "succeeded", Result: "Atlas is active", ExecutedAt: "2026-08-28T12:00:00Z"}}}}}
	artifact, err := extractor.Compact(context.Background(), nil, []usermemory.SessionTurn{{ID: 4, UserText: "I work on Atlas.", AssistantText: "Noted.", ToolHistory: history}})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Narrative != "Atlas is active." || artifact.GenerationModel != "model" || artifact.GeneratorVersion != SummaryGeneratorVersion || len(artifact.Candidates) != 1 || artifact.Candidates[0].SourceTurnID != 4 || artifact.Candidates[0].ClaimSlot != "project.name" {
		t.Fatalf("artifact=%+v", artifact)
	}
	request := client.request
	if request.Model != "model" || request.ToolChoice != llm.ToolChoiceRequired {
		t.Fatalf("model=%q tool choice=%q", request.Model, request.ToolChoice)
	}
	if request.ParallelToolCalls == nil || *request.ParallelToolCalls {
		t.Fatalf("parallel tool calls=%v", request.ParallelToolCalls)
	}
	if request.Temperature == nil || *request.Temperature != 0 {
		t.Fatalf("temperature=%v", request.Temperature)
	}
	if request.MaxTokens != 2048 || request.Format != "" {
		t.Fatalf("max tokens=%d format=%q", request.MaxTokens, request.Format)
	}
	if len(request.Tools) != 1 || request.Tools[0].Function.Name != sessionSummarySaveToolName {
		t.Fatalf("tools=%+v", request.Tools)
	}
	if !strings.Contains(request.Messages[1].Content, "project.lookup") || !strings.Contains(request.Messages[1].Content, "Atlas is active") {
		t.Fatalf("compaction request omitted tool history: %+v", request.Messages)
	}
	parameters := request.Tools[0].Function.Parameters
	if parameters.AdditionalProperties == nil || *parameters.AdditionalProperties || len(parameters.Required) != 7 {
		t.Fatalf("summary tool parameters=%+v", parameters)
	}
	candidates := parameters.Properties["candidates"]
	if candidates.MinItems != nil || candidates.MaxItems != nil || candidates.Items == nil || candidates.Items.AdditionalProperties == nil || *candidates.Items.AdditionalProperties || len(candidates.Items.Required) != 14 {
		t.Fatalf("candidate schema=%+v", candidates)
	}
	for _, field := range []string{"open_tasks", "commitments", "entities", "decisions", "topic_tags"} {
		property := parameters.Properties[field]
		if property.MinItems != nil || property.MaxItems != nil || property.Items == nil || property.Items.Type != "string" {
			t.Fatalf("grammar-heavy %s schema=%+v", field, property)
		}
	}
}

func TestLLMExtractorClassifiesInvalidToolOutput(t *testing.T) {
	tests := []struct {
		name     string
		client   *summaryFakeChatter
		wantCode string
	}{
		{name: "missing", client: &summaryFakeChatter{response: summaryToolResponse()}, wantCode: "missing_tool_call"},
		{name: "multiple", client: &summaryFakeChatter{response: summaryToolResponse(sessionSummarySaveToolName, sessionSummarySaveToolName)}, wantCode: "multiple_tool_calls"},
		{name: "unexpected", client: &summaryFakeChatter{response: summaryToolResponse("other")}, wantCode: "unexpected_tool_call"},
		{name: "malformed", client: &summaryFakeChatter{arguments: map[string]interface{}{"_raw": "bad"}}, wantCode: "malformed_tool_arguments"},
		{name: "missing field", client: &summaryFakeChatter{arguments: map[string]interface{}{"narrative": "x"}}, wantCode: "missing_required_field"},
		{name: "duplicate field", client: &summaryFakeChatter{response: summaryRawToolResponse(sessionSummarySaveToolName, `{"narrative":"x","narrative":"y","open_tasks":[],"commitments":[],"entities":[],"decisions":[],"topic_tags":[],"candidates":[]}`)}, wantCode: "duplicate_argument_field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewLLMExtractor(test.client, "model", 2048).Compact(context.Background(), nil, []usermemory.SessionTurn{{ID: 1, UserText: "I work.", AssistantText: "ok"}})
			var invalid *invalidCompactionOutputError
			if !errors.As(err, &invalid) || invalid.code != test.wantCode {
				t.Fatalf("error=%v code=%q", err, compactionErrorCode(err))
			}
		})
	}
}

func summaryToolResponse(names ...string) *llm.ChatResponse {
	calls := make([]llm.ToolCall, 0, len(names))
	for _, name := range names {
		calls = append(calls, llm.ToolCall{Function: llm.ToolFunction{Name: name}})
	}
	return &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: calls}}
}

func summaryRawToolResponse(name, raw string) *llm.ChatResponse {
	return &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: name, RawArguments: raw}}}}}
}

func TestLLMExtractorClassifiesProviderErrors(t *testing.T) {
	for _, test := range []struct {
		status    int
		permanent bool
	}{{http.StatusBadRequest, true}, {http.StatusUnauthorized, true}, {http.StatusRequestTimeout, false}, {http.StatusTooManyRequests, false}, {http.StatusServiceUnavailable, false}} {
		client := &summaryFakeChatter{err: &llm.ChatHTTPError{StatusCode: test.status, Body: "secret reflected content"}}
		_, err := NewLLMExtractor(client, "model", 2048).Compact(context.Background(), nil, []usermemory.SessionTurn{{ID: 1, UserText: "I work.", AssistantText: "ok"}})
		if errors.Is(err, errPermanentProvider) != test.permanent {
			t.Fatalf("status=%d permanent=%v error=%v", test.status, test.permanent, err)
		}
	}
}

func TestLLMExtractorPreservesExactSourceTurnID(t *testing.T) {
	const sourceTurnID int64 = 9007199254740993
	raw := `{"narrative":"x","open_tasks":[],"commitments":[],"entities":[],"decisions":[],"topic_tags":[],"candidates":[{"source_turn_id":9007199254740993,"statement":"The user works.","evidence":"I work.","scope":"long_term","category":"projects","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":4,"ttl_days":0,"supersedes":"","claim_slot":"project.fact","claim_value":"works"}]}`
	client := &summaryFakeChatter{response: summaryRawToolResponse(sessionSummarySaveToolName, raw)}
	artifact, err := NewLLMExtractor(client, "model", 2048).Compact(context.Background(), nil, []usermemory.SessionTurn{{ID: sourceTurnID, UserText: "I work.", AssistantText: "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Candidates) != 1 || artifact.Candidates[0].SourceTurnID != sourceTurnID {
		t.Fatalf("artifact=%+v", artifact)
	}
}

func TestLLMExtractorRejectsTrailingJSON(t *testing.T) {
	client := &summaryFakeChatter{arguments: map[string]interface{}{"_raw": `{"narrative":"x","open_tasks":[],"commitments":[],"entities":[],"decisions":[],"topic_tags":[],"candidates":[]} {}`}}
	extractor := NewLLMExtractor(client, "model", 2048)
	if _, err := extractor.Compact(context.Background(), nil, []usermemory.SessionTurn{{ID: 1, UserText: "I work.", AssistantText: "ok"}}); err == nil {
		t.Fatal("expected trailing JSON rejection")
	}
}

func summaryArguments(t *testing.T, content string) map[string]interface{} {
	t.Helper()
	var arguments map[string]interface{}
	if err := json.Unmarshal([]byte(content), &arguments); err != nil {
		t.Fatal(err)
	}
	return arguments
}
