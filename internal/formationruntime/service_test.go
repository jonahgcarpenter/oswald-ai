package formationruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryextractor"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

type fakeExtractor struct {
	candidates []usermemory.MemorySaveItem
	submitted  int
	malformed  int
	err        error
	calls      int
}

type unavailableLowPriorityGate struct{}

func (unavailableLowPriorityGate) TryAcquireLowPriority(context.Context) (context.Context, func(), bool) {
	return nil, nil, false
}

type canceledLowPriorityGate struct{}

func (canceledLowPriorityGate) TryAcquireLowPriority(parent context.Context) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	cancel()
	return ctx, func() {}, true
}

func TestFormationJobLeaseCoversProviderTimeout(t *testing.T) {
	if formationJobLease < 5*time.Minute {
		t.Fatalf("formation lease=%s want at least 5m", formationJobLease)
	}
}

func TestFormationJobLeaseExtendsLongProviderTimeout(t *testing.T) {
	service := NewService(nil, nil, "model", config.NewLogger(config.LevelError), 12*time.Minute)
	if service.jobLease != 12*time.Minute+30*time.Second {
		t.Fatalf("job lease=%s", service.jobLease)
	}
}

func TestFormationWarningRetryAndDeadStatuses(t *testing.T) {
	for _, test := range []struct {
		name   string
		event  string
		status string
	}{
		{name: "retry", event: "user_memory.formation.job.retry", status: "retry"},
		{name: "dead", event: "user_memory.formation.job.dead", status: "degraded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := config.NewLogger(config.LevelDebug)
			logger.SetOutput(&output)
			service := NewService(nil, nil, "model", logger)
			service.warn(test.event, "fixed warning", errors.New("provider failed with token=secret"), config.F("status", test.status))

			var record map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
				t.Fatalf("decode warning log: %v; output=%q", err, output.String())
			}
			if record["event"] != test.event || record["status"] != test.status {
				t.Fatalf("warning record=%#v", record)
			}
			if record["log_type"] != "server" || record["component"] != "user_memory.formation" {
				t.Fatalf("unstable warning scope: %#v", record)
			}
			if strings.Contains(output.String(), "secret") {
				t.Fatalf("warning leaked sanitized error: %s", output.String())
			}
		})
	}
}

func (f *fakeExtractor) Extract(context.Context, usermemory.StoredSessionTurn) (usermemory.MemorySaveBatch, error) {
	f.calls++
	submitted := f.submitted
	if submitted == 0 && len(f.candidates) > 0 {
		submitted = len(f.candidates)
	}
	return usermemory.MemorySaveBatch{Memories: f.candidates, SubmittedCount: submitted, MalformedCount: f.malformed}, f.err
}

func TestServiceProcessesAndReplaysTurnIdempotently(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go for project Atlas", "req")
	extractor := &fakeExtractor{candidates: []usermemory.MemorySaveItem{{
		Statement: "The user uses Go for project Atlas.", Evidence: "I use Go for project Atlas",
		Scope: "long_term", Category: "projects", Context: "direct_assertion",
		Provenance: "user_statement", Sensitivity: "low", Confidence: 0.95, Importance: 4,
		ClaimSlot: "project.atlas_language", ClaimValue: "go",
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
	turnID := formationTestTurn(t, store, "Before we continue, my name is Ada. What should we build?", "name")
	extractor := &fakeExtractor{candidates: []usermemory.MemorySaveItem{{
		Statement: "The user's name is Ada.", Evidence: "my name is Ada.",
		Scope: "long_term", Category: "identity", Context: "direct_assertion",
		Provenance: "user_statement", Sensitivity: "identity_or_contact", Confidence: 0.95, Importance: 1,
		ClaimSlot: "identity.name", ClaimValue: "ada",
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
	if err != nil || !strings.Contains(profile.Content, "The user's name is Ada.") {
		t.Fatalf("profile=%q err=%v", profile.Content, err)
	}
}

func TestServicePublishesIndependentFactsFromLongTurn(t *testing.T) {
	store := formationTestStore(t)
	text := "My name is Ada. I use Fedora on my workstation. I prefer concise replies."
	turnID := formationTestTurn(t, store, text, "long")
	extractor := &fakeExtractor{candidates: []usermemory.MemorySaveItem{
		{Statement: "The user's name is Ada.", Evidence: "My name is Ada.", Scope: "long_term", Category: "identity", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "identity_or_contact", Confidence: 0.95, Importance: 3, ClaimSlot: "identity.name", ClaimValue: "Ada"},
		{Statement: "The user uses Fedora on their workstation.", Evidence: "I use Fedora on my workstation.", Scope: "long_term", Category: "environment", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.95, Importance: 4, ClaimSlot: "environment.workstation_os", ClaimValue: "Fedora"},
		{Statement: "The user prefers concise replies.", Evidence: "I prefer concise replies.", Scope: "long_term", Category: "communication_preferences", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4, ClaimSlot: "communication.reply_style", ClaimValue: "concise"},
	}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	if _, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "long", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1"); err != nil {
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
	if err != nil || len(memories) != 3 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServicePublishesPacmanInferenceAsUncertainMemory(t *testing.T) {
	store := formationTestStore(t)
	text := "Considering pacman packages for file management."
	turnID := formationTestTurn(t, store, text, "pacman")
	extractor := &fakeExtractor{candidates: []usermemory.MemorySaveItem{{
		Statement: "The user may use or be evaluating a pacman-based Arch-family Linux environment.", Evidence: text,
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
	if memory.Confidence != 0.55 || memory.ProvenanceType != "model_inference" || memory.SourceAuthority != "model" || memory.ClaimSlot != "environment.linux_distribution" || memory.ClaimValue != "arch_family" || memory.EvidenceCount != 1 {
		t.Fatalf("uncertain memory=%+v", memory)
	}
}

func TestServiceCompletesBlockedConflictWithoutPublicationRetry(t *testing.T) {
	store := formationTestStore(t)
	activeOutput, err := memoryformation.Evaluate(memoryformation.CandidateInput{
		SourceUserText: "I prefer tea.", Statement: "The user prefers tea.", Evidence: "I prefer tea.",
		Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityUserDirect,
		Sensitivity: memoryformation.SensitivityLow, Mode: memoryformation.ModeAutomaticExtraction,
		Scope: memoryformation.ScopeLongTerm, Category: memoryformation.CategoryDurablePreferences,
		Context: memoryformation.ContextDirectAssertion, Confidence: 0.95, Importance: 4,
		ClaimSlot: "preference.drink", ClaimValue: "tea",
	})
	if err != nil {
		t.Fatal(err)
	}
	activeCandidate, _, err := store.ProposeCandidate(context.Background(), "user-1", usermemory.CandidateProposal{Output: activeOutput, IdempotencyKey: "active-tea"})
	if err != nil {
		t.Fatal(err)
	}
	if activeCandidate.PublishedMemoryID == 0 {
		t.Fatalf("active candidate was not atomically published: %+v", activeCandidate)
	}

	turnID := formationTestTurn(t, store, "I prefer coffee.", "blocked")
	extractor := &fakeExtractor{candidates: []usermemory.MemorySaveItem{{
		Statement: "The user prefers coffee.", Evidence: "I prefer coffee.", Scope: "long_term",
		Category: "durable_preferences", Context: "direct_assertion", Provenance: "user_statement",
		Sensitivity: "low", Confidence: 0.9, Importance: 4,
		ClaimSlot: "preference.drink", ClaimValue: "coffee",
	}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "blocked", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "succeeded" || extractor.calls != 1 {
		t.Fatalf("state=%q extractor_calls=%d err=%v", state, extractor.calls, err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 || memories[0].ClaimValue != "tea" {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServiceLeavesFailedJobRetryable(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go", "req")
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

func TestServiceDefersExtractionWhileForegroundWorkIsBusy(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go", "busy")
	extractor := &fakeExtractor{}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(unavailableLowPriorityGate{})
	jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "busy", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" || extractor.calls != 0 {
		t.Fatalf("state=%q calls=%d err=%v", state, extractor.calls, err)
	}
}

func TestServiceDiscardsSuccessfulExtractionAfterForegroundPreemption(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go", "preempt")
	extractor := &fakeExtractor{candidates: []usermemory.MemorySaveItem{{Statement: "The user uses Go.", Evidence: "I use Go", Scope: "long_term", Category: "projects", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4, ClaimSlot: "project.language", ClaimValue: "Go"}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(canceledLowPriorityGate{})
	if _, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "preempt", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), job); !errors.Is(err, errBackgroundPreempted) {
		t.Fatalf("process error=%v", err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("preempted memories=%+v err=%v", memories, err)
	}
}

func TestServiceRetriesAllMalformedCandidatesOnceThenSkips(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, "You actually are on v3.2.0 not 1.0", "req")
	client := &fakeExtractionChatter{content: `{"candidates":[{"statement":"The AI is running version 3.2.0.","evidence":"You actually are on v3.2.0 not 1.0","scope":"short_term","category":"software_version","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":0.4,"ttl_days":7,"supersedes_statement":null}]}`}
	service := NewService(store, newTestLLMExtractor(t, client), "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" || client.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, client.calls, err)
	}
	makeFormationJobReady(t, db, jobID)
	service.drain(context.Background())
	state, err = store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "skipped" || client.calls != 2 {
		t.Fatalf("terminal state=%q calls=%d err=%v", state, client.calls, err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServicePublishesWhenInvalidOutputRetryRecovers(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, "I use Go", "retry-recovers")
	extractor := &fakeExtractor{err: &memoryextractor.InvalidOutputError{Code: "missing_tool_call"}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "retry-recovers", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" || extractor.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, extractor.calls, err)
	}
	extractor.err = nil
	extractor.candidates = []usermemory.MemorySaveItem{{Statement: "The user uses Go.", Evidence: "I use Go", Scope: "long_term", Category: "projects", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4, ClaimSlot: "project.language", ClaimValue: "go"}}
	makeFormationJobReady(t, db, jobID)
	service.drain(context.Background())
	state, err = store.FormationJobState(context.Background(), "user-1", jobID)
	memories, listErr := store.ListMemories("user-1", "", "", 10)
	if err != nil || listErr != nil || state != "succeeded" || extractor.calls != 2 || len(memories) != 1 {
		t.Fatalf("state=%q calls=%d memories=%+v state_err=%v list_err=%v", state, extractor.calls, memories, err, listErr)
	}
}

func TestServiceKeepsInvalidOutputRetryIndependentFromOperationalAttempts(t *testing.T) {
	valid := []usermemory.MemorySaveItem{{Statement: "The user uses Go.", Evidence: "I use Go", Scope: "long_term", Category: "projects", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4, ClaimSlot: "project.language", ClaimValue: "go"}}
	for _, test := range []struct {
		name   string
		errors []error
	}{
		{name: "transient then invalid", errors: []error{errors.New("provider unavailable"), &memoryextractor.InvalidOutputError{Code: "missing_tool_call"}, nil}},
		{name: "invalid then transient", errors: []error{&memoryextractor.InvalidOutputError{Code: "missing_tool_call"}, errors.New("provider unavailable"), nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, db := formationTestStoreWithDB(t)
			turnID := formationTestTurn(t, store, "I use Go", test.name)
			extractor := &fakeExtractor{}
			service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
			jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: test.name, SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
			if err != nil {
				t.Fatal(err)
			}
			for index, extractErr := range test.errors {
				extractor.err = extractErr
				extractor.candidates = nil
				if extractErr == nil {
					extractor.candidates = valid
				}
				service.drain(context.Background())
				if index < len(test.errors)-1 {
					state, err := store.FormationJobState(context.Background(), "user-1", jobID)
					if err != nil || state != "retry" {
						t.Fatalf("step=%d state=%q err=%v", index, state, err)
					}
					makeFormationJobReady(t, db, jobID)
				}
			}
			state, err := store.FormationJobState(context.Background(), "user-1", jobID)
			memories, listErr := store.ListMemories("user-1", "", "", 10)
			var attemptCount, invalidRetryCount int
			counterErr := db.SQL().QueryRow(`SELECT attempt_count, invalid_output_retry_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&attemptCount, &invalidRetryCount)
			if err != nil || listErr != nil || counterErr != nil || state != "succeeded" || extractor.calls != 3 || len(memories) != 1 || attemptCount != 2 || invalidRetryCount != 1 {
				t.Fatalf("state=%q calls=%d memories=%+v attempts=%d invalid_retries=%d state_err=%v list_err=%v counter_err=%v", state, extractor.calls, memories, attemptCount, invalidRetryCount, err, listErr, counterErr)
			}
		})
	}
}

func TestServiceDoesNotRetryValidEmptyBatch(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "Nothing durable here", "empty")
	extractor := &fakeExtractor{}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "empty", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "succeeded" || extractor.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, extractor.calls, err)
	}
}

func TestServiceRetriesCandidateMissingCategory(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "My name is Jonah", "missing-category")
	client := &fakeExtractionChatter{content: `{"memories":[{"statement":"The user's name is Jonah.","evidence":"My name is Jonah","scope":"long_term","context":"direct_assertion","provenance":"user_statement","sensitivity":"identity_or_contact","confidence":1,"importance":5,"ttl_days":0,"supersedes":"","claim_slot":"identity.name","claim_value":"Jonah"}]}`}
	service := NewService(store, newTestLLMExtractor(t, client), "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "missing-category", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" || client.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, client.calls, err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServiceDoesNotRetryPolicyRejectedCandidate(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "My name is Jonah", "rejected")
	client := &fakeExtractionChatter{content: `{"candidates":[{"statement":"The user's name is Jonah.","evidence":"My name is Jonah","scope":"long_term","category":"identity","context":"direct_assertion","provenance":"user_statement","sensitivity":"identity_or_contact","confidence":1,"importance":3,"ttl_days":0,"supersedes_statement":"","claim_slot":"identity_name","claim_value":"jonah"}]}`}
	service := NewService(store, newTestLLMExtractor(t, client), "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "rejected", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
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

func TestServiceDropsMalformedPersistedArtifactBesideValidCandidate(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go.", "mixed-artifact")
	source := usermemory.FormationSource{RequestID: "mixed-artifact", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}
	if _, err := store.EnqueueFormationJob(context.Background(), source, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	artifact := `{"memories":[{"statement":"Malformed sibling.","evidence":"I use Go.","scope":"long_term","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":4,"ttl_days":0,"supersedes":"","claim_slot":"project.language","claim_value":"go"},{"statement":"The user uses Go.","evidence":"I use Go.","scope":"long_term","category":"projects","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":4,"ttl_days":0,"supersedes":"","claim_slot":"project.language","claim_value":"go"}]}`
	if err := store.SaveFormationJobArtifact(context.Background(), job, artifact); err != nil {
		t.Fatal(err)
	}
	if err := NewService(store, nil, "model", config.NewLogger(config.LevelError)).process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 || memories[0].ClaimSlot != "project.language" || memories[0].ClaimValue != "go" || memories[0].EvidenceCount != 1 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServicePersistsAndReplaysExtractionCounts(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go.", "artifact-counts")
	extractor := &fakeExtractor{candidates: []usermemory.MemorySaveItem{{Statement: "The user uses Go.", Evidence: "I use Go.", Scope: "long_term", Category: "projects", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4, ClaimSlot: "project.language", ClaimValue: "go"}}, submitted: 2, malformed: 1}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	if _, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "artifact-counts", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.FormationJobArtifact(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	batch, itemErrors, err := usermemory.DecodeMemorySaveBatchJSON([]byte(artifact))
	if err != nil || len(itemErrors) != 0 || batch.SubmittedCount != 2 || batch.MalformedCount != 1 || len(batch.Memories) != 1 {
		t.Fatalf("artifact=%s batch=%+v item_errors=%+v err=%v", artifact, batch, itemErrors, err)
	}
	if err := service.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 1 {
		t.Fatalf("extractor calls=%d want 1", extractor.calls)
	}
}

func TestServiceRetriesMalformedOuterArguments(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "Nothing to retain", "req")
	client := &fakeExtractionChatter{content: `[{"statement":"wrong top-level shape"}]`}
	service := NewService(store, newTestLLMExtractor(t, client), "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), usermemory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: usermemory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" || client.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, client.calls, err)
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
	arguments := map[string]interface{}{}
	var artifact map[string]interface{}
	if err := json.Unmarshal([]byte(f.content), &artifact); err != nil {
		arguments["_raw"] = f.content
	} else if candidates, ok := artifact["candidates"].([]interface{}); ok {
		for _, raw := range candidates {
			if candidate, ok := raw.(map[string]interface{}); ok {
				if value, exists := candidate["supersedes_statement"]; exists {
					candidate["supersedes"] = value
					delete(candidate, "supersedes_statement")
				}
			}
		}
		arguments["memories"] = candidates
	} else {
		arguments = artifact
	}
	return &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: "user_memory_save", Arguments: arguments}}}}}, nil
}

func newTestLLMExtractor(t *testing.T, client llm.Chatter) *memoryextractor.LLMExtractor {
	t.Helper()
	extractor, err := memoryextractor.NewLLMExtractor(client, "model")
	if err != nil {
		t.Fatal(err)
	}
	return extractor
}

func formationTestStore(t *testing.T) *usermemory.Store {
	t.Helper()
	store, _ := formationTestStoreWithDB(t)
	return store
}

func formationTestStoreWithDB(t *testing.T) (*usermemory.Store, *database.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	db, err := database.Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id) VALUES ('user-1')`); err != nil {
		t.Fatal(err)
	}
	store := usermemory.NewStore(path, log)
	t.Cleanup(func() {
		store.Close() // nolint:errcheck
		db.Close()    // nolint:errcheck
	})
	return store, db
}

func makeFormationJobReady(t *testing.T, db *database.DB, jobID int64) {
	t.Helper()
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
}

func formationTestTurn(t *testing.T, store *usermemory.Store, text, requestID string) int64 {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: requestID})
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "user-1", profile.Generation, text, "answer", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFormationEligible(context.Background(), "user-1", turn.ID); err != nil {
		t.Fatal(err)
	}
	return turn.ID
}
