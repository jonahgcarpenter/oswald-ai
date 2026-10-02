package compaction

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

type profileTestGate struct {
	allowed            bool
	acquired, released int
}

func (g *profileTestGate) TryAcquireLowPriority(ctx context.Context) (context.Context, func(), bool) {
	if !g.allowed {
		return ctx, func() {}, false
	}
	g.acquired++
	return ctx, func() { g.released++ }, true
}

func TestProfileWorkerCorrectiveRetryUsesFrozenRange(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := memory.NewProfileStore(ctx, root, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := "discord:dm:123"
	if _, err := store.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	appendDelivered := func(text string) memory.StoredSessionTurn {
		turn, err := store.AppendPendingSessionTurn(ctx, memory.SessionTurnWrite{UserID: "alice", SessionID: key, Generation: 1, UserText: text, AssistantText: "synthetic answer", Pressure: memory.SessionPromptPressure{Tokens: 80, Limit: 100, Version: "test-v1"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
			t.Fatal(err)
		}
		return turn
	}
	first := appendDelivered("first synthetic source")
	client := &summarySequenceChatter{outcomes: []summarySequenceOutcome{
		{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "invalid output"}}},
		{response: &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: sessionSummarySaveToolName, Arguments: summaryArguments(t, `{"narrative":"synthetic summary","open_tasks":[],"commitments":[],"entities":[],"decisions":[],"topic_tags":[],"candidates":[]}`)}}}}}},
	}}
	compactor := newSummaryTestCompactor(t, client)
	gate := &profileTestGate{}
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(io.Discard)
	worker := NewProfileService(store, compactor, gate, budget.NewContextBudget(32768), log)
	scope := memory.ActiveSessionScope{UserID: "alice", SessionID: key, Generation: 1}
	worker.runScope(ctx, scope)
	if len(client.requests) != 0 {
		t.Fatal("foreground gate bypassed")
	}
	gate.allowed = true
	worker.runScope(ctx, scope)
	if len(client.requests) != 1 {
		t.Fatal("invalid output did not spend one credit")
	}
	appendDelivered("newer source must not change retry")
	worker.runScope(ctx, scope)
	if len(client.requests) != 2 || gate.acquired != gate.released {
		t.Fatal("retry or permit lifecycle failed")
	}
	summary, err := store.LatestSessionSummary(ctx, "alice", key, 1)
	if err != nil || summary.CoveredThroughTurnID != first.ID {
		t.Fatal("retry changed frozen range", err)
	}
	found := false
	for _, message := range client.requests[1].Messages {
		if strings.Contains(message.Content, "Your previous response omitted the required tool call.") {
			found = true
		}
	}
	if !found {
		t.Fatal("corrective feedback not sent")
	}
	retry, ready, dead, done, expired, err := store.CompressionHealth(ctx)
	if err != nil || retry != 0 || ready != 0 || dead != 0 || done != 1 || expired != 0 {
		t.Fatal("incorrect durable health", err)
	}
}

type preemptedProfileChatter struct{ cancel context.CancelFunc }

func (c preemptedProfileChatter) Chat(ctx context.Context, _ llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	c.cancel()
	return nil, ctx.Err()
}

func TestProfileWorkerPreemptionRefundsProviderCreditAndReleasesLease(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := memory.NewProfileStore(context.Background(), root, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := "discord:dm:123"
	if _, err := store.ResolveSessionContext(context.Background(), "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendPendingSessionTurn(context.Background(), memory.SessionTurnWrite{UserID: "alice", SessionID: key, Generation: 1, UserText: "synthetic source", AssistantText: "synthetic answer", Pressure: memory.SessionPromptPressure{Tokens: 80, Limit: 100, Version: "test-v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(context.Background(), "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(io.Discard)
	compactor, err := NewLLMCompactor(preemptedProfileChatter{cancel}, "synthetic/model", log)
	if err != nil {
		t.Fatal(err)
	}
	gate := &profileTestGate{allowed: true}
	input := budget.NewContextBudget(32768)
	worker := NewProfileService(store, compactor, gate, input, log)
	scope := memory.ActiveSessionScope{UserID: "alice", SessionID: key, Generation: 1}
	worker.runScope(ctx, scope)
	if gate.acquired != 1 || gate.released != 1 {
		t.Fatal("preempted permit was not released")
	}
	turns, _, err := store.CompressionCandidates(context.Background(), scope, 0)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimCompression(context.Background(), scope, turns, memory.CompressionContract("synthetic/model", SummaryGeneratorVersion, input.UsableInputLimit()))
	if err != nil {
		t.Fatal("preempted lease was not released", err)
	}
	for range 4 {
		if err := store.ReserveCompressionSubmission(context.Background(), claim); err != nil {
			t.Fatal("preemption consumed provider credit", err)
		}
	}
	if err := store.ReleaseCompression(context.Background(), claim, false, false); err != nil {
		t.Fatal(err)
	}
}
