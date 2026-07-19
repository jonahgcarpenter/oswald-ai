package sessionruntime

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

type summaryFakeChatter struct{ content string }

func (f summaryFakeChatter) Chat(context.Context, llm.ChatRequest, func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: f.content}}, nil
}

func TestLLMExtractorParsesStructuredSummaryAndCandidate(t *testing.T) {
	content := `{"narrative":"Atlas is active.","open_tasks":["ship"],"commitments":[],"entities":["Atlas"],"decisions":[],"topic_tags":["project"],"candidates":[{"source_turn_id":4,"statement":"The user works on Atlas.","evidence":"I work on Atlas.","scope":"long_term","category":"projects","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":4,"ttl_days":0}]}`
	extractor := NewLLMExtractor(summaryFakeChatter{content: content}, "model")
	artifact, err := extractor.Compact(context.Background(), nil, []usermemory.SessionTurn{{ID: 4, UserText: "I work on Atlas.", AssistantText: "Noted."}})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Narrative != "Atlas is active." || artifact.GenerationModel != "model" || artifact.GeneratorVersion != SummaryGeneratorVersion || len(artifact.Candidates) != 1 || artifact.Candidates[0].SourceTurnID != 4 {
		t.Fatalf("artifact=%+v", artifact)
	}
}

func TestLLMExtractorRejectsTrailingJSON(t *testing.T) {
	extractor := NewLLMExtractor(summaryFakeChatter{content: `{"narrative":"x","open_tasks":[],"commitments":[],"entities":[],"decisions":[],"topic_tags":[],"candidates":[]} {}`}, "model")
	if _, err := extractor.Compact(context.Background(), nil, []usermemory.SessionTurn{{ID: 1, UserText: "I work.", AssistantText: "ok"}}); err == nil {
		t.Fatal("expected trailing JSON rejection")
	}
}
