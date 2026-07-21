package usermemory

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
)

func TestCleanupExpiredSessionsIndependentSweep(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	ctx := context.Background()
	expiredProfile, err := store.ResolveSessionProfile(ctx, "user", "expired-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(ctx, "expired-session", "user", expiredProfile.Generation, "bound", "turn", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurn(ctx, "independently-expired", "user", "expired", "turn", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurn(ctx, "subsecond-future", "user", "future", "turn", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveMemory(ctx, "user", SaveRequest{
		Scope: ScopeLongTerm, Category: "identity", Statement: "The user is Ada.", Confidence: 1, Importance: 5,
	}); err != nil {
		t.Fatal(err)
	}
	latestProfile, err := store.ResolveSessionProfile(ctx, "user", "active-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if latestProfile.VersionID == expiredProfile.VersionID {
		t.Fatal("profile mutation did not create a new version")
	}

	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'expired-session'`, formatTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'independently-expired'`, formatTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'subsecond-future'`, formatTime(now.Add(500*time.Millisecond))); err != nil {
		t.Fatal(err)
	}

	counts, err := store.CleanupExpiredSessions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (SessionCleanupCounts{SessionTurnsDeleted: 2, TenantSessionsDeleted: 1}) {
		t.Fatalf("unexpected cleanup counts: %+v", counts)
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'user'`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE session_id = 'subsecond-future'`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM sessions WHERE canonical_user_id = 'user' AND session_id = 'expired-session' AND is_active = 0`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM sessions WHERE canonical_user_id = 'user' AND profile_version = ?`, 1, latestProfile.VersionID)

	next, err := store.ResolveSessionProfile(ctx, "user", "expired-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != expiredProfile.Generation+1 {
		t.Fatalf("next generation = %d, want %d", next.Generation, expiredProfile.Generation+1)
	}
}

func TestCleanupExpiresDurableMemoryAndErasesFormationRetention(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	ctx := context.Background()
	memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeShortTerm, Category: "notes", Statement: "Temporary secret code.", Evidence: "temporary", TTL: time.Nanosecond, Embedding: []float64{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	pendingInput := memoryformation.CandidateInput{SourceUserText: "My phone is 555-0100", Statement: "The user's phone is 555-0100.", Evidence: "My phone is 555-0100", Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityUserDirect, Sensitivity: memoryformation.SensitivityIdentityOrContact, Mode: memoryformation.ModeAutomaticExtraction, Scope: memoryformation.ScopeLongTerm, Category: memoryformation.CategoryIdentity, Context: memoryformation.ContextDirectAssertion, Confidence: 0.9, Importance: 4}
	output, err := memoryformation.Evaluate(pendingInput)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err := store.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, IdempotencyKey: "old-pending"})
	if err != nil {
		t.Fatal(err)
	}
	turnID := seedFormationTurn(t, store, "user", "session", "source")
	jobID, err := store.EnqueueFormationJob(ctx, FormationSource{TurnID: turnID, SessionID: "session", ExtractorVersion: FormationExtractorVersion}, "user")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE memory_candidates SET created_at = ? WHERE id = ?; UPDATE durable_jobs SET state = 'succeeded', completed_at = ? WHERE id = ? AND job_kind = 'memory_formation'`, formatTime(now.Add(-31*24*time.Hour)), candidate.ID, formatTime(now.Add(-8*24*time.Hour)), jobID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	counts, err := store.CleanupExpiredSessions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts.MemoryEntriesExpired != 1 || counts.CandidatesErased != 1 || counts.FormationJobsDeleted != 0 {
		t.Fatalf("cleanup counts=%+v", counts)
	}
	var status, candidateStatement, evidence string
	if err := store.sql.QueryRow(`SELECT status FROM memory_entries WHERE id = ?`, memory.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT statement FROM memory_candidates WHERE id = ?`, candidate.ID).Scan(&candidateStatement); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT content FROM memory_evidence WHERE candidate_id = ?`, candidate.ID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || candidateStatement != "" || evidence != "" {
		t.Fatalf("status=%s candidate=%q evidence=%q", status, candidateStatement, evidence)
	}
	var deleteChanges int
	if err := store.sql.QueryRow(`SELECT count(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND entity_kind = 'memory' AND entity_id = ? AND operation = 'delete'`, memory.ID).Scan(&deleteChanges); err != nil {
		t.Fatal(err)
	}
	if deleteChanges != 1 {
		t.Fatalf("expired memory delete changes=%d", deleteChanges)
	}
}

func TestCleanupExpiredSessionsRollsBackOnFailure(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	profile, err := store.ResolveSessionProfile(context.Background(), "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session", "user", profile.Generation, "user", "assistant", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user'`, formatTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`CREATE TRIGGER fail_session_cleanup BEFORE UPDATE OF is_active ON sessions BEGIN SELECT RAISE(ABORT, 'cleanup blocked'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.CleanupExpiredSessions(context.Background(), now); err == nil {
		t.Fatal("cleanup unexpectedly succeeded")
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'user'`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM sessions WHERE canonical_user_id = 'user' AND is_active = 1`, 1)
}

func TestCleanupRetainsTranscriptAndSummaryForActiveSessionLifetime(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	ctx := context.Background()
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two"} {
		if err := store.AppendSessionTurnForGeneration(ctx, "session", "user", profile.Generation, text, "answer", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := store.DeliveredSessionTurnsAfter(ctx, "user", "session", profile.Generation, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueSessionCompactionJob(ctx, "user", "session", profile.Generation, turns.Turns[0].ID, turns.Turns[1].ID); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(ctx, "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(ctx, job, SummaryArtifact{Narrative: "summary", GenerationModel: "model", GeneratorVersion: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishSessionSummary(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSessionCompactionJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE session_turns SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'session'`, formatTime(now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	counts, err := store.CleanupExpiredSessions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts.SessionTurnsDeleted != 0 || counts.SessionSummariesDeleted != 0 || counts.CompactionJobsDeleted != 0 {
		t.Fatalf("active session history was removed: %+v", counts)
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'user' AND session_id = 'session'`, 2)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_summaries WHERE canonical_user_id = 'user' AND session_id = 'session'`, 1)

	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'session'`, formatTime(now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	counts, err = store.CleanupExpiredSessions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts.SessionTurnsDeleted != 2 || counts.SessionSummariesDeleted != 1 || counts.CompactionJobsDeleted != 1 || counts.TenantSessionsDeleted != 1 {
		t.Fatalf("inactive session history cleanup counts: %+v", counts)
	}
}

func TestRunSessionCleanupImmediateRepeatsAndContinuesAfterError(t *testing.T) {
	cleaner := &recordingSessionCleaner{failFirst: true, delay: 5 * time.Millisecond, called: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunSessionCleanup(ctx, cleaner, 2*time.Millisecond, nil)
		close(done)
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-cleaner.called:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for cleanup call %d", i+1)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup runner did not stop after cancellation")
	}

	cleaner.mu.Lock()
	defer cleaner.mu.Unlock()
	if cleaner.calls < 3 {
		t.Fatalf("cleanup calls = %d, want at least 3", cleaner.calls)
	}
	if cleaner.maxActive != 1 {
		t.Fatalf("overlapping cleanups = %d", cleaner.maxActive)
	}
}

type recordingSessionCleaner struct {
	mu        sync.Mutex
	calls     int
	active    int
	maxActive int
	failFirst bool
	delay     time.Duration
	called    chan struct{}
}

func (c *recordingSessionCleaner) CleanupExpiredSessions(context.Context, time.Time) (SessionCleanupCounts, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.active++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
	c.mu.Unlock()

	select {
	case c.called <- struct{}{}:
	default:
	}
	time.Sleep(c.delay)

	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	if c.failFirst && call == 1 {
		return SessionCleanupCounts{}, errors.New("transient cleanup failure")
	}
	return SessionCleanupCounts{SessionTurnsDeleted: 1}, nil
}

func assertCleanupRowCount(t *testing.T, store *Store, query string, want int, args ...any) {
	t.Helper()
	var got int
	if err := store.sql.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("row count = %d, want %d for %q", got, want, query)
	}
}
