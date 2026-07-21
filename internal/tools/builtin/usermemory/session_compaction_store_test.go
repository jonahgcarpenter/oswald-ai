package usermemory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
)

func TestSessionCompactionRangeAndEnqueueIdempotency(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user-a", "user-b")
	generation := activateCompactionSession(t, store, "user-a", "shared")
	activateCompactionSession(t, store, "user-b", "shared")
	first := appendDeliveredCompactionTurn(t, store, "user-a", "shared", generation, "one")
	second := appendDeliveredCompactionTurn(t, store, "user-a", "shared", generation, "two")
	_ = appendDeliveredCompactionTurn(t, store, "user-b", "shared", generation, "private")

	planned, err := store.DeliveredSessionTurnsAfter(context.Background(), "user-a", "shared", generation, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if planned.TotalCount != 2 || len(planned.Turns) != 1 || planned.Turns[0].ID != first {
		t.Fatalf("planned turns = %+v", planned)
	}
	ranged, err := store.DeliveredSessionTurnsRange(context.Background(), "user-a", "shared", generation, first, second)
	if err != nil || len(ranged) != 2 || ranged[0].ID != first || ranged[1].ID != second {
		t.Fatalf("range = %+v, err = %v", ranged, err)
	}
	firstJob, err := store.EnqueueSessionCompactionJob(context.Background(), "user-a", "shared", generation, first, second)
	if err != nil {
		t.Fatal(err)
	}
	secondJob, err := store.EnqueueSessionCompactionJob(context.Background(), "user-a", "shared", generation, first, second)
	if err != nil || secondJob != firstJob {
		t.Fatalf("idempotent job = %d, want %d, err = %v", secondJob, firstJob, err)
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user-b", "shared", generation, first, second); err == nil {
		t.Fatal("expected cross-tenant range rejection")
	}
}

func TestSessionCompactionDoesNotCrossUndeliveredTurn(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	first := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	middle := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "two")
	last := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "three")
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = NULL WHERE id = ?`, middle); err != nil {
		t.Fatal(err)
	}
	planned, err := store.DeliveredSessionTurnsAfter(context.Background(), "user", "session", generation, 0, 100)
	if err != nil || planned.TotalCount != 1 || len(planned.Turns) != 1 || planned.Turns[0].ID != first {
		t.Fatalf("planned across delivery gap: %+v err=%v", planned, err)
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, last); err == nil {
		t.Fatal("enqueued range across undelivered turn")
	}
	if err := store.MarkSessionTurnDeliveryFailed(context.Background(), "user", middle); err != nil {
		t.Fatal(err)
	}
	planned, err = store.DeliveredSessionTurnsAfter(context.Background(), "user", "session", generation, 0, 100)
	if err != nil || planned.TotalCount != 2 || len(planned.Turns) != 2 || planned.Turns[1].ID != last {
		t.Fatalf("terminal failed delivery still blocked later turns: %+v err=%v", planned, err)
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, last); err != nil {
		t.Fatalf("enqueue across terminal failed delivery: %v", err)
	}
}

func TestSessionCompactionArtifactPublicationAndIncrementalSources(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	first := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	second := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "two")

	jobID, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, second)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute)
	if err != nil || job.ID != jobID {
		t.Fatalf("claim = %+v, err = %v", job, err)
	}
	artifact := SummaryArtifact{Narrative: "First checkpoint", OpenTasks: []string{"ship it"}, Commitments: []string{"follow up"}, Entities: []string{"Atlas"}, Decisions: []string{"use Go"}, TopicTags: []string{"project"}, GenerationModel: "model", GeneratorVersion: "summary-v1"}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, artifact); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, SummaryArtifact{Narrative: "changed", GenerationModel: "model", GeneratorVersion: "summary-v1"}); err == nil {
		t.Fatal("expected immutable artifact mismatch")
	}
	summary, err := store.PublishSessionSummary(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.SourceTurnIDs, []int64{first, second}) || summary.SourceDigest == "" || !reflect.DeepEqual(summary.OpenTasks, []string{"ship it"}) {
		t.Fatalf("published summary = %+v", summary)
	}
	replayed, err := store.PublishSessionSummary(context.Background(), job)
	if err != nil || replayed.ID != summary.ID {
		t.Fatalf("replayed summary = %+v, err = %v", replayed, err)
	}
	if err := store.CompleteSessionCompactionJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET user_text = 'changed' WHERE id = ?`, first); err == nil {
		t.Fatal("updated immutable summary source text")
	}
	if _, err := store.sql.Exec(`DELETE FROM session_turns WHERE id = ?`, first); err == nil {
		t.Fatal("deleted immutable summary source")
	}

	third := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "three")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, third); err != nil {
		t.Fatal(err)
	}
	incrementalJob, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), incrementalJob, SummaryArtifact{Narrative: "Incremental checkpoint", GenerationModel: "model", GeneratorVersion: "summary-v1"}); err != nil {
		t.Fatal(err)
	}
	incremental, err := store.PublishSessionSummary(context.Background(), incrementalJob)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(incremental.SourceTurnIDs, []int64{first, second, third}) {
		t.Fatalf("incremental sources = %v", incremental.SourceTurnIDs)
	}
	latest, err := store.LatestSessionSummary(context.Background(), "user", "session", generation)
	if err != nil || latest.ID != incremental.ID {
		t.Fatalf("latest = %+v, err = %v", latest, err)
	}
	if _, err := store.ResetSession(context.Background(), "user", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM session_summaries WHERE canonical_user_id = 'user' AND session_id = 'session'`, 0)
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = 'user' AND session_id = 'session'`, 0)
}

func TestExpiredSessionGenerationCannotCompact(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turn := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'session'`, formatTime(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(context.Background(), "user", turn); err == nil {
		t.Fatal("expired generation accepted delivery mark")
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turn, turn); err == nil {
		t.Fatal("expired generation accepted compaction job")
	}
}

func TestPreCompactionCandidateRequiresLiveJobLease(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turn := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "I work on Atlas.")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turn, turn); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET lease_until = ? WHERE id = ? AND job_kind = 'session_compaction'`, formatTime(time.Now().Add(-time.Minute)), job.ID); err != nil {
		t.Fatal(err)
	}
	output, err := memoryformation.Evaluate(memoryformation.CandidateInput{SourceUserText: "I work on Atlas.", Statement: "The user works on Atlas.", Evidence: "I work on Atlas.", Scope: memoryformation.ScopeLongTerm, Category: memoryformation.CategoryProjects, Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityModel, Sensitivity: memoryformation.SensitivityLow, Mode: memoryformation.ModePreCompactionExtraction, Context: memoryformation.ContextDirectAssertion, Confidence: 0.9, Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, Source: FormationSource{RequestID: "session-compaction:1", SessionID: "session", SessionGeneration: generation, TurnID: turn, Model: "model", ExtractorVersion: "v1"}, IdempotencyKey: "stale-lease", CompactionJob: &job}); err == nil {
		t.Fatal("stale lease staged pre-compaction candidate")
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM memory_candidates WHERE idempotency_key = 'stale-lease'`, 0)
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET lease_until = ? WHERE id = ? AND job_kind = 'session_compaction'`, formatTime(time.Now().Add(time.Minute)), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, Source: FormationSource{RequestID: "session-compaction:1", SessionID: "other-session", SessionGeneration: generation, TurnID: turn, Model: "model", ExtractorVersion: "v1"}, IdempotencyKey: "wrong-scope", CompactionJob: &job}); err == nil {
		t.Fatal("mismatched session staged pre-compaction candidate")
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM memory_candidates WHERE idempotency_key = 'wrong-scope'`, 0)
}

func TestSessionCompactionPublicationRollsBackAndRejectsStaleGeneration(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	first := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	second := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "two")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, second); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, SummaryArtifact{Narrative: "checkpoint", GenerationModel: "model", GeneratorVersion: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = NULL WHERE id = ?`, second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishSessionSummary(context.Background(), job); err == nil {
		t.Fatal("expected incomplete source range rejection")
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM session_summaries`, 0)
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction' AND artifact_summary_id IS NOT NULL`, 0)

	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetSession(context.Background(), "user", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishSessionSummary(context.Background(), job); err == nil {
		t.Fatal("expected stale generation rejection")
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM session_summaries`, 0)
	changed, err := store.ReconcileSessionCompactionJobs(context.Background())
	if err != nil || changed != 0 {
		t.Fatalf("reconciled = %d, err = %v", changed, err)
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction'`, 0)
}

func TestSessionCompactionLeaseRetryDeadAndRedrive(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turnID := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turnID, turnID); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= maxSessionCompactionAttempts; attempt++ {
		job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute)
		if err != nil || job.AttemptCount != attempt {
			t.Fatalf("attempt %d job = %+v, err = %v", attempt, job, err)
		}
		if err := store.RetrySessionCompactionJob(context.Background(), job, "model failed", "temporary failure"); err != nil {
			t.Fatal(err)
		}
		if attempt < maxSessionCompactionAttempts {
			if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ? AND job_kind = 'session_compaction'`, formatTime(time.Now().Add(-time.Second)), job.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claim dead job error = %v", err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET updated_at = ? WHERE job_kind = 'session_compaction'`, formatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	redriven, err := store.RedriveDeadSessionCompactionJobs(context.Background(), time.Minute)
	if err != nil || redriven != 1 {
		t.Fatalf("redriven = %d, err = %v", redriven, err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker-2", time.Minute)
	if err != nil || job.AttemptCount != 1 || job.RedriveCount != 1 {
		t.Fatalf("redriven job = %+v, err = %v", job, err)
	}
}

func newSessionCompactionTestStore(t *testing.T) *Store {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func activateCompactionSession(t *testing.T, store *Store, userID, sessionID string) int {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), userID, sessionID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return profile.Generation
}

func appendDeliveredCompactionTurn(t *testing.T, store *Store, userID, sessionID string, generation int, text string) int64 {
	t.Helper()
	turn, err := store.AppendSessionTurnForGenerationResult(context.Background(), sessionID, userID, generation, text, "answer "+text, nil, time.Hour)
	if err != nil || turn.ID == 0 {
		t.Fatalf("append turn = %+v, err = %v", turn, err)
	}
	if err := store.MarkSessionTurnDelivered(context.Background(), userID, turn.ID); err != nil {
		t.Fatal(err)
	}
	return turn.ID
}

func assertCompactionCount(t *testing.T, store *Store, query string, want int) {
	t.Helper()
	var got int
	if err := store.sql.QueryRow(query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query %q count = %d, want %d", query, got, want)
	}
}
