package startup

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

type fakeSummarizerRotator struct {
	mu      sync.Mutex
	rotated []summarizerTarget
}

func (f *fakeSummarizerRotator) Rotate(next summarizerTarget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotated = append(f.rotated, next)
}

func (f *fakeSummarizerRotator) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rotated)
}

type fakeDelegateSwapper struct {
	mu      sync.Mutex
	configs []*config.Config
}

func (f *fakeDelegateSwapper) SwapConfig(next *config.Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs = append(f.configs, next)
}

func (f *fakeDelegateSwapper) last() *config.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.configs) == 0 {
		return nil
	}
	return f.configs[len(f.configs)-1]
}

func routingConfig(name, model string) *config.Config {
	return &config.Config{
		ProfileName:        name,
		LLMGatewayModel:    model,
		LLMGatewayProvider: "fake",
		LLMGatewayURL:      "http://localhost:9999/v1",
		LLMGatewayAPIKey:   "synthetic-key",
	}
}

func newTestModelRouter(view agent.View) *agent.ModelRouter {
	return agent.NewModelRouter(view, &profileFakeModel{}, func(url, apiKey, virtualKey string) llm.Chatter {
		return &profileFakeModel{}
	}, nil, nil)
}

func TestModelApplierRotatesChangedProfileOnly(t *testing.T) {
	old := routingConfig("default", "fake/default")
	old.Profiles = map[string]*config.Config{"alice": routingConfig("alice", "fake/alice")}
	next := routingConfig("default", "fake/default")
	next.Profiles = map[string]*config.Config{"alice": routingConfig("alice", "fake/alice-next")}

	defaultRouter := newTestModelRouter(modelViewFor(old))
	aliceRouter := newTestModelRouter(modelViewFor(old.Profiles["alice"]))
	defaultRotator := &fakeSummarizerRotator{}
	aliceRotator := &fakeSummarizerRotator{}
	defaultDelegate := &fakeDelegateSwapper{}
	aliceDelegate := &fakeDelegateSwapper{}
	handles := map[string]modelRoutingHandles{
		"default": {router: defaultRouter, summarizer: defaultRotator, delegate: defaultDelegate},
		"alice":   {router: aliceRouter, summarizer: aliceRotator, delegate: aliceDelegate},
	}
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&strings.Builder{})

	buildModelApplier(handles, log)(old, next)

	if defaultRotator.count() != 0 || defaultDelegate.last() != nil {
		t.Fatal("unchanged profile was rotated")
	}
	target, _ := defaultRouter.Resolve(context.Background(), "", "", "", "")
	if target.Model != "fake/default" {
		t.Fatalf("unchanged router model = %q", target.Model)
	}

	if aliceRotator.count() != 1 {
		t.Fatalf("changed profile rotations = %d, want 1", aliceRotator.count())
	}
	if got := aliceDelegate.last(); got != next.Profiles["alice"] {
		t.Fatalf("delegate config = %+v, want the next profile", got)
	}
	target, _ = aliceRouter.Resolve(context.Background(), "", "", "", "")
	if target.Model != "fake/alice-next" {
		t.Fatalf("changed router model = %q", target.Model)
	}
}

func TestModelApplierWarnsAndContinuesAfterRotationFailure(t *testing.T) {
	old := routingConfig("default", "fake/default")
	old.Profiles = map[string]*config.Config{
		"alice": routingConfig("alice", "fake/alice"),
		"bob":   routingConfig("bob", "fake/bob"),
	}
	next := routingConfig("default", "fake/default")
	next.Profiles = map[string]*config.Config{
		"alice": routingConfig("alice", ""),
		"bob":   routingConfig("bob", "fake/bob-next"),
	}

	defaultRouter := newTestModelRouter(modelViewFor(old))
	aliceRouter := newTestModelRouter(modelViewFor(old.Profiles["alice"]))
	bobRouter := newTestModelRouter(modelViewFor(old.Profiles["bob"]))
	defaultRotator := &fakeSummarizerRotator{}
	aliceRotator := &fakeSummarizerRotator{}
	bobRotator := &fakeSummarizerRotator{}
	defaultDelegate := &fakeDelegateSwapper{}
	aliceDelegate := &fakeDelegateSwapper{}
	bobDelegate := &fakeDelegateSwapper{}
	handles := map[string]modelRoutingHandles{
		"default": {router: defaultRouter, summarizer: defaultRotator, delegate: defaultDelegate},
		"alice":   {router: aliceRouter, summarizer: aliceRotator, delegate: aliceDelegate},
		"bob":     {router: bobRouter, summarizer: bobRotator, delegate: bobDelegate},
	}
	log := config.NewLogger(config.LevelDebug)
	var output strings.Builder
	log.SetOutput(&output)

	buildModelApplier(handles, log)(old, next)

	if aliceRotator.count() != 0 || aliceDelegate.last() != nil {
		t.Fatal("failed rotation reached the summarizer or delegate")
	}
	if target, _ := aliceRouter.Resolve(context.Background(), "", "", "", ""); target.Model != "fake/alice" {
		t.Fatalf("failed rotation swapped the router to %q", target.Model)
	}
	if !strings.Contains(output.String(), `"event":"config.reload.apply_failed"`) {
		t.Fatalf("apply_failed event missing: %s", output.String())
	}
	if !strings.Contains(output.String(), `"profile":"alice"`) {
		t.Fatalf("apply_failed profile missing: %s", output.String())
	}
	if bobRotator.count() != 1 {
		t.Fatalf("bob rotations = %d, want 1", bobRotator.count())
	}
	if got := bobDelegate.last(); got != next.Profiles["bob"] {
		t.Fatalf("bob delegate config = %+v", got)
	}
	target, _ := bobRouter.Resolve(context.Background(), "", "", "", "")
	if target.Model != "fake/bob-next" {
		t.Fatalf("bob router model = %q", target.Model)
	}
}
