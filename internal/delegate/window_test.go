package delegate

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

func TestWindowForUsesKnownWindow(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	view := agent.View{
		Default: agent.DefaultTarget{Model: "big-model", Provider: "alpha", URL: "https://alpha.example.com/v1", APIKey: "k"},
		Endpoints: map[string]config.ProviderEndpoint{
			"alpha": {URL: "https://alpha.example.com/v1", APIKey: "k"},
		},
	}
	router := agent.NewModelRouter(view, &fakeChatter{}, func(provider string, endpoint config.ProviderEndpoint, virtualKey string) llm.Chatter {
		return &fakeChatter{}
	}, func(ctx context.Context, provider string, endpoint config.ProviderEndpoint, virtualKey string) ([]llm.ModelEntry, error) {
		return []llm.ModelEntry{{ID: "big-model", Window: 1000000}}, nil
	}, nil)
	if _, err := router.ListModels(context.Background(), "alpha"); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	m := NewManager(Deps{Profile: "alice", Log: log, Window: 32768, Router: router})
	if got := m.windowFor("alpha", "big-model"); got != 1000000 {
		t.Fatalf("windowFor = %d, want 1000000", got)
	}
	if got := m.windowFor("alpha", "unknown"); got != 32768 {
		t.Fatalf("windowFor unknown = %d, want fallback 32768", got)
	}
}
