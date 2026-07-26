package memoryextractor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestUserMemorySaveToolMatchesBatchContract(t *testing.T) {
	tool := userMemorySaveTool()
	memories := tool.Function.Parameters.Properties["memories"]
	if tool.Function.Name != userMemorySaveToolName || memories.Items == nil || memories.MaxItems == nil || *memories.MaxItems != maxExtractedMemoryBatch {
		t.Fatalf("private tool schema=%+v", tool)
	}
	if memories.Items.AdditionalProperties == nil || *memories.Items.AdditionalProperties || len(memories.Items.Required) != 13 {
		t.Fatalf("private memory item schema=%+v", memories.Items)
	}
	importance := memories.Items.Properties["importance"]
	if importance.Minimum == nil || importance.Maximum == nil || *importance.Minimum != 1 || *importance.Maximum != 5 {
		t.Fatalf("private importance schema=%+v", importance)
	}
	for field, bounds := range map[string][2]float64{"confidence": {0, 1}, "ttl_days": {0, 30}} {
		property := memories.Items.Properties[field]
		if property.Minimum == nil || property.Maximum == nil || *property.Minimum != bounds[0] || *property.Maximum != bounds[1] {
			t.Fatalf("private %s schema=%+v", field, property)
		}
	}
	for _, field := range usermemory.MemorySaveRequiredFields() {
		if _, ok := memories.Items.Properties[field]; !ok {
			t.Fatalf("private schema missing %q", field)
		}
	}
}

func TestNewLLMExtractorValidatesDependencies(t *testing.T) {
	if _, err := NewLLMExtractor(nil, "model"); err == nil {
		t.Fatal("expected missing client error")
	}
	if _, err := NewLLMExtractor(&fakeChatter{}, " "); err == nil {
		t.Fatal("expected missing model error")
	}
}

func TestLLMExtractorParsesStrictJSON(t *testing.T) {
	client := &fakeChatter{arguments: map[string]interface{}{"memories": []interface{}{validCandidate()}}}
	extractor := newTestExtractor(t, client)
	got, err := extractor.Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use Go"})
	if err != nil || len(got.Memories) != 1 || got.Memories[0].Evidence != "I use Go" || got.Memories[0].ClaimSlot != "project.language" {
		t.Fatalf("extracted=%+v err=%v", got, err)
	}
	if len(client.request.Tools) != 1 || client.request.ToolChoice == nil || client.request.ToolChoice.Function.Name != userMemorySaveToolName {
		t.Fatalf("request did not force the private memory save tool: %+v", client.request)
	}
	for _, required := range []string{"all 13 fields", "Evidence must be the complete user turn", "identity -> identity.*", "identity.name, never identity_name", "topic mention does not establish", "importance must be an integer from 1 to 5", `"category":"communication_preferences"`, `"claim_slot":"communication.reply_style"`, `{"memories":[]}`} {
		if !strings.Contains(client.request.Messages[0].Content, required) {
			t.Fatalf("extractor policy prompt missing %q", required)
		}
	}
}

func TestLLMExtractorRejectsAllMalformedCandidates(t *testing.T) {
	malformed := validCandidate()
	delete(malformed, "claim_slot")
	client := &fakeChatter{arguments: map[string]interface{}{"memories": []interface{}{malformed}}}
	got, err := newTestExtractor(t, client).Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use Go"})
	if !errors.Is(err, ErrPermanentExtraction) || len(got.Memories) != 0 || client.calls != 1 || !strings.Contains(err.Error(), "all 1 submitted candidates were malformed") {
		t.Fatalf("extracted=%+v calls=%d err=%v", got, client.calls, err)
	}
}

func TestLLMExtractorRejectsCandidateMissingCategory(t *testing.T) {
	malformed := validCandidate()
	delete(malformed, "category")
	client := &fakeChatter{arguments: map[string]interface{}{"memories": []interface{}{malformed}}}
	_, err := newTestExtractor(t, client).Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "My name is Jonah"})
	if !errors.Is(err, ErrPermanentExtraction) || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error=%v", err)
	}
}

func TestLLMExtractorPreservesValidCandidatesBesideMalformedCandidates(t *testing.T) {
	malformed := validCandidate()
	delete(malformed, "category")
	client := &fakeChatter{arguments: map[string]interface{}{"memories": []interface{}{malformed, validCandidate()}}}
	got, err := newTestExtractor(t, client).Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use Go"})
	if err != nil || len(got.Memories) != 1 || got.Memories[0].Statement != "The user uses Go." {
		t.Fatalf("extracted=%+v err=%v", got, err)
	}
}

func TestLLMExtractorRejectsMoreThanFiveCandidates(t *testing.T) {
	items := make([]interface{}, maxExtractedMemoryBatch+1)
	for i := range items {
		items[i] = validCandidate()
	}
	client := &fakeChatter{arguments: map[string]interface{}{"memories": items}}
	_, err := newTestExtractor(t, client).Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use several tools"})
	if !errors.Is(err, ErrPermanentExtraction) || !strings.Contains(err.Error(), "maximum is 5") {
		t.Fatalf("error=%v", err)
	}
}

func TestLLMExtractorClassifiesProviderErrors(t *testing.T) {
	permanent := &fakeChatter{err: &llm.ChatHTTPError{StatusCode: 400, Body: "unsupported response format"}}
	_, err := newTestExtractor(t, permanent).Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use Go"})
	if !errors.Is(err, ErrPermanentExtraction) {
		t.Fatalf("permanent error=%v", err)
	}

	transient := &fakeChatter{err: &llm.ChatHTTPError{StatusCode: 503, Body: "unavailable"}}
	_, err = newTestExtractor(t, transient).Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use Go"})
	if err == nil || errors.Is(err, ErrPermanentExtraction) {
		t.Fatalf("transient error=%v", err)
	}
}

type fakeChatter struct {
	arguments map[string]interface{}
	err       error
	request   llm.ChatRequest
	calls     int
}

func (f *fakeChatter) Chat(_ context.Context, request llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.calls++
	f.request = request
	if f.err != nil {
		return nil, f.err
	}
	return &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: userMemorySaveToolName, Arguments: f.arguments}}}}}, nil
}

func newTestExtractor(t *testing.T, client llm.Chatter) *LLMExtractor {
	t.Helper()
	extractor, err := NewLLMExtractor(client, "model")
	if err != nil {
		t.Fatal(err)
	}
	return extractor
}

func validCandidate() map[string]interface{} {
	return map[string]interface{}{
		"statement": "The user uses Go.", "evidence": "I use Go", "scope": "long_term", "category": "projects",
		"context": "direct_assertion", "provenance": "user_statement", "sensitivity": "low", "confidence": 0.9,
		"importance": 4, "ttl_days": 0, "supersedes": "", "claim_slot": "project.language", "claim_value": "go",
	}
}
