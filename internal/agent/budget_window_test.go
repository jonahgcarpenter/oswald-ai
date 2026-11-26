package agent

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	tokenbudget "github.com/jonahgcarpenter/oswald-ai/internal/session/compaction/budget"
)

func TestInputLimitForTargetUsesKnownWindow(t *testing.T) {
	ctx := context.Background()
	fake := newRouterListingFake()
	fake.models[routerListingAlphaURL] = []llm.ModelEntry{{ID: "big-model", Window: 1000000}}
	router := NewModelRouter(routerListingView(), routerTestDefaultClient(), newRouterClientFactory().newClient, fake.list, newRouterPinStore())
	if _, err := router.ListModels(ctx, "alpha-provider"); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	a := &Agent{budget: tokenbudget.NewContextBudget(32768), router: router}
	target := Target{Model: "big-model", Provider: "alpha-provider", Client: routerTestDefaultClient(), BillingBaseURL: routerListingAlphaURL}
	if got := a.inputLimitForTarget(ctx, target); got != 1000000-256 {
		t.Fatalf("inputLimit = %d, want %d", got, 1000000-256)
	}
}

func TestInputLimitForTargetFallsBackWithoutWindow(t *testing.T) {
	ctx := context.Background()
	router := NewModelRouter(routerTestView(), routerTestDefaultClient(), newRouterClientFactory().newClient, nil, newRouterPinStore())
	a := &Agent{budget: tokenbudget.NewContextBudget(32768), router: router}
	target := Target{Model: "unknown-model", Provider: "default-provider", Client: routerTestDefaultClient()}
	want := tokenbudget.NewContextBudget(32768).UsableInputLimit()
	if got := a.inputLimitForTarget(ctx, target); got != want {
		t.Fatalf("inputLimit = %d, want fallback %d", got, want)
	}
}

func TestInputLimitForTargetHandlesLargeToSmallSwap(t *testing.T) {
	ctx := context.Background()
	fake := newRouterListingFake()
	fake.models[routerListingAlphaURL] = []llm.ModelEntry{
		{ID: "big-model", Window: 1000000},
		{ID: "small-model", Window: 32768},
	}
	router := NewModelRouter(routerListingView(), routerTestDefaultClient(), newRouterClientFactory().newClient, fake.list, newRouterPinStore())
	if _, err := router.ListModels(ctx, "alpha-provider"); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	a := &Agent{budget: tokenbudget.NewContextBudget(32768), router: router}
	big := Target{Model: "big-model", Provider: "alpha-provider", Client: routerTestDefaultClient()}
	small := Target{Model: "small-model", Provider: "alpha-provider", Client: routerTestDefaultClient()}
	bigLimit := a.inputLimitForTarget(ctx, big)
	smallLimit := a.inputLimitForTarget(ctx, small)
	if bigLimit <= smallLimit {
		t.Fatalf("big limit %d should exceed small limit %d", bigLimit, smallLimit)
	}
	if smallLimit != 32768-256 {
		t.Fatalf("small limit = %d, want %d", smallLimit, 32768-256)
	}
}
