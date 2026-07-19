package sessionruntime

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

type fakeSummaryExtractor struct {
	calls    int
	previous *usermemory.SessionSummary
	turns    []usermemory.SessionTurn
}

func (f *fakeSummaryExtractor) Compact(_ context.Context, previous *usermemory.SessionSummary, turns []usermemory.SessionTurn) (usermemory.SummaryArtifact, error) {
	f.calls++
	f.previous = previous
	f.turns = append([]usermemory.SessionTurn(nil), turns...)
	first := turns[0]
	commitments := []string{"Report progress"}
	if previous != nil {
		commitments = append(append([]string(nil), previous.Commitments...), "Finish review")
	}
	return usermemory.SummaryArtifact{
		Narrative: "The user is progressing through Atlas work.",
		OpenTasks: []string{"Continue Atlas"}, Commitments: commitments,
		Entities: []string{"Atlas"}, Decisions: []string{"Work sequentially"}, TopicTags: []string{"project"},
		Candidates: []usermemory.CompactionCandidateArtifact{{
			SourceTurnID: first.ID, Statement: "The user is working on Atlas.", Evidence: first.UserText,
			Scope: "long_term", Category: "projects", Context: "direct_assertion",
			Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4,
		}},
	}, nil
}

func TestServicePlansCompactsAndPreservesRecentTail(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		text := fmt.Sprintf("I am working on Atlas item %d.", i)
		if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, text, "Progress recorded.", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	extractor := &fakeSummaryExtractor{}
	service := NewService(store, extractor, "summary-model", promptbudget.ContextBudget{PromptLimit: 100000}, time.Minute, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("plan job=%d err=%v", jobID, err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 1 || extractor.previous != nil || len(extractor.turns) != 17 {
		t.Fatalf("extractor calls=%d previous=%+v turns=%d", extractor.calls, extractor.previous, len(extractor.turns))
	}
	summary, err := store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.SourceTurnIDs) != 17 || summary.CoveredThroughTurnID != extractor.turns[len(extractor.turns)-1].ID || summary.ExpiresAt.IsZero() {
		t.Fatalf("summary=%+v", summary)
	}
	tail, err := store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session-1", profile.Generation, summary.CoveredThroughTurnID, 100)
	if err != nil || len(tail) != minimumRecentTail {
		t.Fatalf("tail=%d err=%v", len(tail), err)
	}
	for i := 26; i <= 42; i++ {
		text := fmt.Sprintf("I am working on Atlas item %d.", i)
		if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, text, "Progress recorded.", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	jobID, err = service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("incremental plan job=%d err=%v", jobID, err)
	}
	job, err = store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 2 || extractor.previous == nil || len(extractor.previous.Commitments) != 1 || extractor.previous.Commitments[0] != "Report progress" {
		t.Fatalf("previous checkpoint was not supplied: calls=%d previous=%+v", extractor.calls, extractor.previous)
	}
	summary, err = store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.SourceTurnIDs) != 34 || len(summary.Commitments) != 2 || summary.Commitments[0] != "Report progress" || summary.Commitments[1] != "Finish review" {
		t.Fatalf("incremental checkpoint lost continuity: %+v", summary)
	}
	tail, err = store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session-1", profile.Generation, summary.CoveredThroughTurnID, 100)
	if err != nil || len(tail) != minimumRecentTail {
		t.Fatalf("incremental tail=%d err=%v", len(tail), err)
	}
	active, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(active) != 0 {
		t.Fatalf("pre-compaction candidate became active: %+v err=%v", active, err)
	}
}

func TestServicePlannerWaitsBelowThreshold(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < minimumRecentTail+1; i++ {
		if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, "short", "short", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(store, &fakeSummaryExtractor{}, "model", promptbudget.ContextBudget{PromptLimit: 100000}, time.Minute, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID != 0 {
		t.Fatalf("unexpected plan job=%d err=%v", jobID, err)
	}
}

func newSessionRuntimeStore(t *testing.T) *usermemory.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	db, err := database.Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES ('user-1', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	db.Close() // nolint:errcheck
	store := usermemory.NewStore(path, log)
	t.Cleanup(func() { store.Close() })
	return store
}
