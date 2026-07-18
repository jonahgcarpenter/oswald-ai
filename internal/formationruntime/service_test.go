package formationruntime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

type fakeExtractor struct {
	candidates []ExtractedCandidate
	err        error
	calls      int
}

func (f *fakeExtractor) Extract(context.Context, usermemory.StoredSessionTurn) ([]ExtractedCandidate, error) {
	f.calls++
	return f.candidates, f.err
}

func TestServiceProcessesAndReplaysTurnIdempotently(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go for project Atlas")
	extractor := &fakeExtractor{candidates: []ExtractedCandidate{{
		Statement: "The user uses Go for project Atlas.", Evidence: "I use Go for project Atlas",
		Scope: "long_term", Category: "projects", Context: "direct_assertion",
		Provenance: "user_statement", Sensitivity: "low", Confidence: 0.95, Importance: 4,
	}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	source := usermemory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}
	jobID, err := store.EnqueueFormationJob(context.Background(), source, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || job.ID != jobID {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if err := service.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteFormationJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}

	if err := service.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	memories, err = store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("replay memories=%+v err=%v", memories, err)
	}
	if extractor.calls != 1 {
		t.Fatalf("extractor calls=%d want=1 with persisted artifact", extractor.calls)
	}
}

func TestServiceLeavesFailedJobRetryable(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go")
	extractor := &fakeExtractor{err: errors.New("extractor offline")}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	_, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), job); err == nil {
		t.Fatal("expected extraction failure")
	}
	if err := store.RetryFormationJob(context.Background(), job, "extractor", formationMaxAttempts); err != nil {
		t.Fatal(err)
	}
	state, err := store.FormationJobState(context.Background(), "user-1", job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state != "retry" {
		t.Fatalf("job state=%s", state)
	}
}

func TestServicePublishesExplicitToolCandidateAfterTurnPersistence(t *testing.T) {
	store := formationTestStore(t)
	output, err := memoryformation.Evaluate(memoryformation.CandidateInput{
		SourceUserText: "Remember that I prefer tea", Statement: "The user prefers tea.", Evidence: "I prefer tea",
		Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityUserDirect,
		Sensitivity: memoryformation.SensitivityLow, Mode: memoryformation.ModeExplicitRemember,
		Scope: memoryformation.ScopeLongTerm, Category: memoryformation.CategoryDurablePreferences,
		Context: memoryformation.ContextDirectAssertion, Confidence: 0.95, Importance: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err := store.ProposeCandidate(context.Background(), "user-1", usermemory.CandidateProposal{Output: output, IdempotencyKey: "explicit", Source: usermemory.FormationSource{RequestID: "req", SessionID: "session", ToolName: "memory.save"}})
	if err != nil {
		t.Fatal(err)
	}
	turnID := formationTestTurn(t, store, "Remember that I prefer tea")
	_, err = store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &fakeExtractor{}, "model", config.NewLogger(config.LevelError))
	if err := service.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCandidate(context.Background(), "user-1", candidate.ID)
	if err != nil || loaded.SourceTurnID != turnID || loaded.PublishedMemoryID == 0 {
		t.Fatalf("explicit candidate=%+v err=%v", loaded, err)
	}
}

func TestLLMExtractorParsesStrictJSON(t *testing.T) {
	client := fakeExtractionChatter{content: `[{"statement":"The user uses Go.","evidence":"I use Go","scope":"long_term","category":"projects","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":4,"ttl_days":0,"supersedes_statement":""}]`}
	extractor := NewLLMExtractor(client, "model")
	got, err := extractor.Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use Go"})
	if err != nil || len(got) != 1 || got[0].Evidence != "I use Go" {
		t.Fatalf("extracted=%+v err=%v", got, err)
	}
}

type fakeExtractionChatter struct{ content string }

func (f fakeExtractionChatter) Chat(context.Context, llm.ChatRequest, func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: f.content}}, nil
}

func formationTestStore(t *testing.T) *usermemory.Store {
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

func formationTestTurn(t *testing.T, store *usermemory.Store, text string) int64 {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(context.Background(), "session", "user-1", profile.Generation, text, "answer", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return turn.ID
}
