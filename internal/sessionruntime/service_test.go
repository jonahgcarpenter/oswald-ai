package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
	preempt  func()
	err      error
	results  []error
}

type unavailableLowPriorityGate struct{}

func (unavailableLowPriorityGate) TryAcquireLowPriority(context.Context) (context.Context, func(), bool) {
	return nil, nil, false
}

type canceledLowPriorityGate struct {
	cancel context.CancelFunc
}

func (g *canceledLowPriorityGate) TryAcquireLowPriority(parent context.Context) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	g.cancel = cancel
	return ctx, func() {}, true
}

func (f *fakeSummaryExtractor) Compact(_ context.Context, previous *usermemory.SessionSummary, turns []usermemory.SessionTurn) (usermemory.SummaryArtifact, error) {
	f.calls++
	f.previous = previous
	f.turns = append([]usermemory.SessionTurn(nil), turns...)
	if f.preempt != nil {
		f.preempt()
	}
	if f.calls <= len(f.results) && f.results[f.calls-1] != nil {
		return usermemory.SummaryArtifact{}, f.results[f.calls-1]
	}
	if f.err != nil {
		return usermemory.SummaryArtifact{}, f.err
	}
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
			ClaimSlot: "project.name", ClaimValue: "Atlas",
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
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, text, "Progress recorded.", 100001, 100000); err != nil {
			t.Fatal(err)
		}
	}
	extractor := &fakeSummaryExtractor{}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("plan job=%d err=%v", jobID, err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if job.Model != "model" || job.GeneratorVersion != SummaryGeneratorVersion {
		t.Fatalf("job model=%q generator version=%q", job.Model, job.GeneratorVersion)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 1 || extractor.previous != nil || len(extractor.turns) != 23 {
		t.Fatalf("extractor calls=%d previous=%+v turns=%d", extractor.calls, extractor.previous, len(extractor.turns))
	}
	summary, err := store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.SourceTurnIDs) != 23 || summary.CoveredThroughTurnID != extractor.turns[len(extractor.turns)-1].ID {
		t.Fatalf("summary=%+v", summary)
	}
	tail, err := store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session-1", profile.Generation, summary.CoveredThroughTurnID, 100)
	if err != nil || len(tail) != maximumRecentTail {
		t.Fatalf("tail=%d err=%v", len(tail), err)
	}
	for i := 26; i <= 42; i++ {
		text := fmt.Sprintf("I am working on Atlas item %d.", i)
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, text, "Progress recorded.", 100001, 100000); err != nil {
			t.Fatal(err)
		}
	}
	jobID, err = service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("incremental plan job=%d err=%v", jobID, err)
	}
	job, err = store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 2 || extractor.previous == nil || len(extractor.previous.Commitments) != 1 || extractor.previous.Commitments[0] != "Report progress" {
		t.Fatalf("previous checkpoint was not supplied: calls=%d previous=%+v", extractor.calls, extractor.previous)
	}
	summary, err = store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.SourceTurnIDs) != 40 || len(summary.Commitments) != 2 || summary.Commitments[0] != "Report progress" || summary.Commitments[1] != "Finish review" {
		t.Fatalf("incremental checkpoint lost continuity: %+v", summary)
	}
	tail, err = store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session-1", profile.Generation, summary.CoveredThroughTurnID, 100)
	if err != nil || len(tail) != maximumRecentTail {
		t.Fatalf("incremental tail=%d err=%v", len(tail), err)
	}
	active, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(active) != 1 || active[0].ClaimSlot != "project.name" {
		t.Fatalf("pre-compaction candidate was not unified and published: %+v err=%v", active, err)
	}
}

func TestEvaluateCompactionCandidateLifecycleThresholdAndSoundness(t *testing.T) {
	turn := usermemory.SessionTurn{ID: 7, UserText: "I work on Atlas."}
	base := usermemory.CompactionCandidateArtifact{
		SourceTurnID: 7, Statement: "The user works on Atlas.", Evidence: "I work on Atlas.",
		Scope: "long_term", Category: "projects", Context: "direct_assertion", Provenance: "user_statement",
		Sensitivity: "low", Importance: 4, ClaimSlot: "project.name", ClaimValue: "Atlas",
	}
	below := base
	below.Confidence = 0.349
	evaluated, err := evaluateCompactionCandidates([]usermemory.SessionTurn{turn}, []usermemory.CompactionCandidateArtifact{below})
	if err != nil || evaluated[0].output.Approval != "proposed" {
		t.Fatalf("below-threshold evaluation=%+v err=%v", evaluated, err)
	}
	atThreshold := base
	atThreshold.Confidence = 0.35
	evaluated, err = evaluateCompactionCandidates([]usermemory.SessionTurn{turn}, []usermemory.CompactionCandidateArtifact{atThreshold})
	if err != nil || evaluated[0].output.Approval != "approved" || evaluated[0].output.ClaimSlot != "project.name" || evaluated[0].output.ClaimValue != "atlas" {
		t.Fatalf("at-threshold evaluation=%+v err=%v", evaluated, err)
	}
	unsound := base
	unsound.Confidence = 0.99
	unsound.Evidence = "My coworker works on Atlas."
	unsound.Statement = "The user works on Atlas."
	turn.UserText = unsound.Evidence
	evaluated, err = evaluateCompactionCandidates([]usermemory.SessionTurn{turn}, []usermemory.CompactionCandidateArtifact{unsound})
	if err != nil || evaluated[0].output.Approval == "approved" {
		t.Fatalf("unsound high-confidence evaluation=%+v err=%v", evaluated, err)
	}
}

func TestServiceCompactionYieldsWhenForegroundWorkIsBusy(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, fmt.Sprintf("I am working on Atlas item %d.", i), "Progress recorded.", 100001, 100000); err != nil {
			t.Fatal(err)
		}
	}
	extractor := &fakeSummaryExtractor{}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(unavailableLowPriorityGate{})
	if _, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); !errors.Is(err, errLowPriorityUnavailable) || extractor.calls != 0 {
		t.Fatalf("process error=%v extractor calls=%d", err, extractor.calls)
	}
	if err := store.DeferSessionCompactionJob(context.Background(), job, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestServiceDiscardsSuccessfulCompactionAfterForegroundPreemption(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, fmt.Sprintf("I am working on Atlas item %d.", i), "Progress recorded.", 100001, 100000); err != nil {
			t.Fatal(err)
		}
	}
	gate := &canceledLowPriorityGate{}
	extractor := &fakeSummaryExtractor{preempt: func() { gate.cancel() }}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(gate)
	if _, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); !errors.Is(err, errProviderPreempted) {
		t.Fatalf("process error=%v", err)
	}
	if _, err := store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation); err == nil {
		t.Fatal("preempted compaction published a summary")
	}
}

func TestServiceBoundsInvalidCompactionOutput(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	extractor := &fakeSummaryExtractor{err: invalidCompactionOutput("missing_tool_call")}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	var state string
	var attempts, invalidRetries int
	if err := db.SQL().QueryRow(`SELECT state, attempt_count, compaction_invalid_output_retry_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &attempts, &invalidRetries); err != nil {
		t.Fatal(err)
	}
	if state != "retry" || attempts != 0 || invalidRetries != 1 || extractor.calls != 1 {
		t.Fatalf("first state=%q attempts=%d invalid_retries=%d calls=%d", state, attempts, invalidRetries, extractor.calls)
	}
	makeCompactionJobReady(t, db, jobID)
	service.drain(context.Background())
	if err := db.SQL().QueryRow(`SELECT state, attempt_count, compaction_invalid_output_retry_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &attempts, &invalidRetries); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || attempts != 1 || invalidRetries != 1 || extractor.calls != 2 {
		t.Fatalf("terminal state=%q attempts=%d invalid_retries=%d calls=%d", state, attempts, invalidRetries, extractor.calls)
	}
}

func TestServiceSkipsPermanentCompactionProviderFailure(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	extractor := &fakeSummaryExtractor{err: &permanentProviderError{statusCode: 400}}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	var state, code string
	if err := db.SQL().QueryRow(`SELECT state, last_error_code FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || code != "provider_request_rejected" || extractor.calls != 1 {
		t.Fatalf("state=%q code=%q calls=%d", state, code, extractor.calls)
	}
}

func TestServiceChargesProviderStartedPreemption(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	gate := &canceledLowPriorityGate{}
	extractor := &fakeSummaryExtractor{preempt: func() { gate.cancel() }}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(gate)
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	var state, code string
	var attempts int
	if err := db.SQL().QueryRow(`SELECT state, attempt_count, last_error_code FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &attempts, &code); err != nil {
		t.Fatal(err)
	}
	if state != "retry" || attempts != 1 || code != "transient_preempted_after_start" || extractor.calls != 1 {
		t.Fatalf("state=%q attempts=%d code=%q calls=%d", state, attempts, code, extractor.calls)
	}
}

func TestServiceCompactionProviderCallsHaveAbsoluteBound(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	extractor := &fakeSummaryExtractor{results: []error{invalidCompactionOutput("missing_tool_call")}, err: errors.New("provider unavailable")}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	for {
		service.drain(context.Background())
		var state string
		var redrives int
		if err := db.SQL().QueryRow(`SELECT state, redrive_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &redrives); err != nil {
			t.Fatal(err)
		}
		switch state {
		case "retry":
			makeCompactionJobReady(t, db, jobID)
		case "dead":
			if redrives == 3 {
				var attempts, invalidRetries int
				if err := db.SQL().QueryRow(`SELECT attempt_count, compaction_invalid_output_retry_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&attempts, &invalidRetries); err != nil {
					t.Fatal(err)
				}
				if extractor.calls != 13 || attempts != 3 || invalidRetries != 1 {
					t.Fatalf("calls=%d attempts=%d invalid_retries=%d", extractor.calls, attempts, invalidRetries)
				}
				return
			}
			if _, err := db.SQL().Exec(`UPDATE durable_jobs SET updated_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), jobID); err != nil {
				t.Fatal(err)
			}
			count, err := store.RedriveDeadSessionCompactionJobs(context.Background(), time.Second)
			if err != nil || count != 1 {
				t.Fatalf("redrive count=%d err=%v", count, err)
			}
			makeCompactionJobReady(t, db, jobID)
		default:
			t.Fatalf("unexpected state=%q", state)
		}
	}
}

func TestValidateCompactionCandidatesKeepsStoreFailuresTransient(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &fakeSummaryExtractor{}, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	if _, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = service.validateCandidates(ctx, job, usermemory.SummaryArtifact{})
	if err == nil || errors.Is(err, errInvalidCompactionOutput) {
		t.Fatalf("validation error=%v", err)
	}
}

func TestServicePlannerWaitsBelowThreshold(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maximumRecentTail+1; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, "short", "short", 99999, 100000); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(store, &fakeSummaryExtractor{}, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID != 0 {
		t.Fatalf("unexpected plan job=%d err=%v", jobID, err)
	}
}

func TestServicePlannerTriggersAtPressureBoundary(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, fmt.Sprintf("turn %d", i), "answer", 100000, 100000); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(store, &fakeSummaryExtractor{}, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("boundary plan job=%d err=%v", jobID, err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if job.CoveredThroughTurnID != job.TargetTurnID {
		t.Fatalf("single-head campaign job=%+v", job)
	}
}

func TestPreservedRecentTailIsAtMostTwoCompleteExchanges(t *testing.T) {
	small := usermemory.SessionTurn{UserText: "short", AssistantText: "short"}
	if got := preservedRecentTailCount([]usermemory.SessionTurn{small, small, small}, 100000); got != 2 {
		t.Fatalf("small tail count=%d", got)
	}
	oversized := usermemory.SessionTurn{UserText: strings.Repeat("x", 9000), AssistantText: strings.Repeat("y", 9000)}
	if got := preservedRecentTailCount([]usermemory.SessionTurn{small, oversized}, 4000); got != 0 {
		t.Fatalf("oversized newest tail count=%d", got)
	}
}

func TestServiceCampaignContinuesAfterFirstChunk(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", maximumCompactionRange+6)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &fakeSummaryExtractor{}, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	if _, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if job.CoveredThroughTurnID >= job.TargetTurnID {
		t.Fatalf("first chunk unexpectedly covered campaign: %+v", job)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	nextID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || nextID == 0 || nextID == job.ID {
		t.Fatalf("continuation job=%d first=%d err=%v", nextID, job.ID, err)
	}
	next, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if next.TargetTurnID != job.TargetTurnID || next.CoveredThroughTurnID != job.TargetTurnID {
		t.Fatalf("continuation target changed: first=%+v next=%+v", job, next)
	}
}

func TestServiceRecordsUncompactableCompleteExchangeWithoutProviderCall(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, strings.Repeat("large ", 1000), "answer", 100, 100); err != nil {
			t.Fatal(err)
		}
	}
	extractor := &fakeSummaryExtractor{}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("receipt job=%d err=%v", jobID, err)
	}
	var state, code string
	if err := db.SQL().QueryRow(`SELECT state, last_error_code FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || code != "uncompactable_complete_exchange" || extractor.calls != 0 {
		t.Fatalf("state=%q code=%q extractor_calls=%d", state, code, extractor.calls)
	}
}

func newSessionRuntimeStore(t *testing.T) *usermemory.Store {
	t.Helper()
	store, _ := newSessionRuntimeStoreWithDB(t)
	return store
}

func newSessionRuntimeStoreWithDB(t *testing.T) (*usermemory.Store, *database.DB) {
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

func seedCompactionRuntimeTurns(t *testing.T, store *usermemory.Store, sessionID string, count int) (usermemory.SessionProfile, error) {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", sessionID, time.Hour)
	if err != nil {
		return usermemory.SessionProfile{}, err
	}
	for i := 1; i <= count; i++ {
		if err := appendDeliveredPressureTurn(store, sessionID, "user-1", profile.Generation, fmt.Sprintf("I am working on Atlas item %d.", i), "Progress recorded.", 100001, 100000); err != nil {
			return usermemory.SessionProfile{}, err
		}
	}
	return profile, nil
}

func appendDeliveredPressureTurn(store *usermemory.Store, sessionID, userID string, generation int, userText, assistantText string, tokens, limit int) error {
	turn, err := store.AppendSessionTurnForGenerationResultWithPressure(context.Background(), sessionID, userID, generation, userText, assistantText, nil, time.Hour, usermemory.SessionPromptPressure{Tokens: tokens, Limit: limit, Version: promptPressureVersion("model", limit)})
	if err != nil {
		return err
	}
	return store.MarkSessionTurnDelivered(context.Background(), userID, turn.ID)
}

func makeCompactionJobReady(t *testing.T, db *database.DB, jobID int64) {
	t.Helper()
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
}
