package modelinfo

import (
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestSelectOpenRouterModelExactAndNormalized(t *testing.T) {
	models := []openRouterModel{
		{ID: "a", HuggingFaceID: "org/model-a", TopProvider: openRouterTopProvider{ContextLength: 1024}},
		{ID: "b", HuggingFaceID: " Org/Model-B ", TopProvider: openRouterTopProvider{ContextLength: 2048}},
	}

	selected, ok := selectOpenRouterModel(models, "org/model-a")
	if !ok || selected.ID != "a" {
		t.Fatalf("expected exact model a, got %+v ok=%v", selected, ok)
	}
	selected, ok = selectOpenRouterModel(models, "org/model-b")
	if !ok || selected.ID != "b" {
		t.Fatalf("expected normalized model b, got %+v ok=%v", selected, ok)
	}
}

func TestSelectOpenRouterModelRejectsAmbiguousNormalizedMatch(t *testing.T) {
	models := []openRouterModel{
		{ID: "a", HuggingFaceID: " Org/Model "},
		{ID: "b", HuggingFaceID: "org/model"},
	}
	if _, ok := selectOpenRouterModel(models, "ORG/MODEL"); ok {
		t.Fatal("expected ambiguous normalized match to fail")
	}
}

func TestApplyEnvOverridesAndNormalizeDetails(t *testing.T) {
	details := Details{Name: "model", Provider: "openrouter", ContextWindow: 1000, MaxOutputTokens: 100, Source: "openrouter", Confidence: "high"}
	openRouter := details
	cfg := &config.Config{LLMGatewayModel: "model", ModelContextWindow: 2000, ModelMaxOutputTokens: 300}
	applyEnvOverrides(&details, cfg, openRouter, true, config.NewLogger(config.LevelError))
	details = normalizeDetails(details)

	if details.ContextWindow != 2000 || details.MaxOutputTokens != 300 || details.MaxInputTokens != 1700 {
		t.Fatalf("unexpected normalized details: %+v", details)
	}
	if details.Source != "env+openrouter" || details.Confidence != "high" {
		t.Fatalf("unexpected source/confidence: %+v", details)
	}
}
