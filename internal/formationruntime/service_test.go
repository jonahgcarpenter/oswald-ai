package formationruntime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

	if err := service.process(context.Background(), job); err == nil {
		t.Fatal("completed formation lease replay succeeded")
	}
	memories, err = store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("replay memories=%+v err=%v", memories, err)
	}
	if extractor.calls != 1 {
		t.Fatalf("extractor calls=%d want=1 with persisted artifact", extractor.calls)
	}
}

func TestServicePublishesPartialDirectNameIntoNewSessionProfile(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "Before we continue, my name is Ada. What should we build?")
	extractor := &fakeExtractor{candidates: []ExtractedCandidate{{
		Statement: "The user's name is Ada.", Evidence: "my name is Ada.",
		Scope: "long_term", Category: "identity", Context: "direct_assertion",
		Provenance: "user_statement", Sensitivity: "identity_or_contact", Confidence: 0.95, Importance: 1,
	}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	_, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "name", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "identity", 10)
	if err != nil || len(memories) != 1 || memories[0].Importance != 3 {
		t.Fatalf("identity memories=%+v err=%v", memories, err)
	}
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "new-session", time.Hour)
	if err != nil || !strings.Contains(profile.Content, "my name is Ada") {
		t.Fatalf("profile=%q err=%v", profile.Content, err)
	}
}

func TestServicePublishesPacmanInferenceAsUncertainMemory(t *testing.T) {
	store := formationTestStore(t)
	text := "what is the best pacman package for file management?"
	turnID := formationTestTurn(t, store, text)
	extractor := &fakeExtractor{candidates: []ExtractedCandidate{{
		Statement: "The user may use or be evaluating a pacman-based Linux environment.", Evidence: text,
		Scope: "long_term", Category: "environment", Context: "direct_assertion",
		Provenance: "model_inference", Sensitivity: "low", Confidence: 0.55, Importance: 3,
		ClaimSlot: "environment.linux_distribution", ClaimValue: "arch_family",
	}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	source := usermemory.FormationSource{RequestID: "pacman", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}
	if _, err := store.EnqueueFormationJob(context.Background(), source, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
	memory := memories[0]
	if memory.Confidence != 0.55 || memory.ProvenanceType != "model_inference" || memory.SourceAuthority != "model" || memory.ClaimKey != "environment.linux_distribution=arch_family" || memory.EvidenceCount != 1 {
		t.Fatalf("uncertain memory=%+v", memory)
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
	client := &fakeExtractionChatter{content: `{"candidates":[{"statement":"The user uses Go.","evidence":"I use Go","scope":"long_term","category":"projects","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":4,"ttl_days":0,"supersedes_statement":"","claim_slot":"projects.language","claim_value":"go"}]}`}
	extractor := NewLLMExtractor(client, "model")
	got, err := extractor.Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use Go"})
	if err != nil || len(got) != 1 || got[0].Evidence != "I use Go" || got[0].ClaimSlot != "projects.language" {
		t.Fatalf("extracted=%+v err=%v", got, err)
	}
	if client.request.Format != "json" {
		t.Fatalf("request format = %q, want json", client.request.Format)
	}
	prompt := client.request.Messages[0].Content
	for _, required := range []string{"smallest complete span", "part of a longer user prompt", "Inference evidence must be the complete user turn", "identity facts, including names, must have importance at least 3", `"I am your creator" is about the user`} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("extractor policy prompt missing %q", required)
		}
	}
}

func TestLLMExtractorDiscardsMalformedCandidateWithoutFailingArtifact(t *testing.T) {
	client := &fakeExtractionChatter{content: `{"candidates":[{"statement":"The AI is running version 3.2.0.","evidence":"You actually are on v3.2.0 not 1.0","scope":"short_term","category":"software_version","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":0.4,"ttl_days":7,"supersedes_statement":null}]}`}
	extractor := NewLLMExtractor(client, "model")
	got, err := extractor.Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "You actually are on v3.2.0 not 1.0"})
	if err != nil || len(got) != 0 || client.calls != 1 {
		t.Fatalf("extracted=%+v calls=%d err=%v", got, client.calls, err)
	}
}

func TestServiceCompletesMalformedCandidateArtifactWithoutRetry(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "You actually are on v3.2.0 not 1.0")
	client := &fakeExtractionChatter{content: `{"candidates":[{"statement":"The AI is running version 3.2.0.","evidence":"You actually are on v3.2.0 not 1.0","scope":"short_term","category":"software_version","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":0.4,"ttl_days":7,"supersedes_statement":null}]}`}
	service := NewService(store, NewLLMExtractor(client, "model"), "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "succeeded" || client.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, client.calls, err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestLLMExtractorPreservesValidCandidatesBesideMalformedCandidates(t *testing.T) {
	client := &fakeExtractionChatter{content: `{"candidates":[{"statement":"bad","evidence":"bad","importance":0.4},{"statement":"The user uses Go.","evidence":"I use Go","scope":"long_term","category":"projects","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":4,"ttl_days":0,"supersedes_statement":""}]}`}
	extractor := NewLLMExtractor(client, "model")
	got, err := extractor.Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use Go"})
	if err != nil || len(got) != 1 || got[0].Statement != "The user uses Go." {
		t.Fatalf("extracted=%+v err=%v", got, err)
	}
}

func TestServiceSkipsPermanentExtractionFailureWithoutRetry(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "Nothing to retain")
	client := &fakeExtractionChatter{content: `[{"statement":"wrong top-level shape"}]`}
	service := NewService(store, NewLLMExtractor(client, "model"), "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "skipped" || client.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, client.calls, err)
	}
}

func TestLLMExtractorRejectsPermanentProviderRequestError(t *testing.T) {
	client := &fakeExtractionChatter{err: &llm.ChatHTTPError{StatusCode: 400, Body: "unsupported response format"}}
	_, err := NewLLMExtractor(client, "model").Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use Go"})
	if !errors.Is(err, errPermanentExtraction) {
		t.Fatalf("error = %v, want permanent extraction failure", err)
	}
}

func TestLLMExtractorLeavesTransientProviderErrorRetryable(t *testing.T) {
	client := &fakeExtractionChatter{err: &llm.ChatHTTPError{StatusCode: 503, Body: "unavailable"}}
	_, err := NewLLMExtractor(client, "model").Extract(context.Background(), usermemory.StoredSessionTurn{UserText: "I use Go"})
	if err == nil || errors.Is(err, errPermanentExtraction) || errorCode(err) != "transient_provider" {
		t.Fatalf("error = %v, code = %q", err, errorCode(err))
	}
}

type fakeExtractionChatter struct {
	content string
	err     error
	request llm.ChatRequest
	calls   int
}

func (f *fakeExtractionChatter) Chat(_ context.Context, request llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.calls++
	f.request = request
	if f.err != nil {
		return nil, f.err
	}
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
