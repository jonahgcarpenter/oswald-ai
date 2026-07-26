package usermemory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
)

func maintenanceTestPolicy() config.RetentionPolicy {
	return config.RetentionPolicy{
		ForgottenContentGrace:           time.Hour,
		ContentBearingAuditJobRetention: time.Hour,
		ContentFreeTombstoneRetention:   2 * time.Hour,
		RetiredIndexRetention:           time.Hour,
		SessionInactivity:               24 * time.Hour,
		PendingDeliveryTimeout:          15 * time.Minute,
		CandidateContentRetention:       time.Hour,
		SuccessfulJobRetention:          24 * time.Hour,
		DeadJobRetention:                48 * time.Hour,
		AccountChallengeGrace:           time.Hour,
		MaintenanceInterval:             time.Hour,
		DatabaseOptimizeInterval:        24 * time.Hour,
		BatchSize:                       100,
	}
}

func TestMaintenanceFailsPendingDeliveriesAtBoundaryInBatchesAndUnblocksCompaction(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "user", profile.Generation, "pending one", "answer", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "user", profile.Generation, "pending two", "answer", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "user", profile.Generation, "delivered", "answer", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(ctx, "user", delivered.ID); err != nil {
		t.Fatal(err)
	}
	policy := maintenanceTestPolicy()
	policy.BatchSize = 1
	now := time.Now().UTC()
	created := formatTime(now.Add(-policy.PendingDeliveryTimeout))
	if _, err := store.sql.Exec(`UPDATE session_turns SET created_at = ? WHERE id IN (?, ?)`, created, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	before, err := store.MaintenanceSweep(ctx, now.Add(-time.Second), policy)
	if err != nil || before.PendingDeliveriesFailed != 0 {
		t.Fatalf("pre-boundary counts=%+v err=%v", before, err)
	}
	blocked, err := store.DeliveredSessionTurnsAfter(ctx, "user", "session", profile.Generation, 0, 10)
	if err != nil || blocked.TotalCount != 0 {
		t.Fatalf("pending turn did not block compaction: turns=%+v err=%v", blocked, err)
	}
	for sweep := 0; sweep < 2; sweep++ {
		counts, err := store.MaintenanceSweep(ctx, now, policy)
		if err != nil || counts.PendingDeliveriesFailed != 1 {
			t.Fatalf("sweep %d counts=%+v err=%v", sweep, counts, err)
		}
	}
	unblocked, err := store.DeliveredSessionTurnsAfter(ctx, "user", "session", profile.Generation, 0, 10)
	if err != nil || unblocked.TotalCount != 1 || len(unblocked.Turns) != 1 || unblocked.Turns[0].ID != delivered.ID {
		t.Fatalf("terminal failures did not unblock compaction: turns=%+v err=%v", unblocked, err)
	}
}

func TestMarkFormationEligibleClearsPendingDeliveryTimeoutFailure(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "user", profile.Generation, "late delivery", "answer", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivery_failed_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), turn.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFormationEligible(ctx, "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	var deliveredAt, failedAt any
	if err := store.sql.QueryRow(`SELECT delivered_at, delivery_failed_at FROM session_turns WHERE id = ?`, turn.ID).Scan(&deliveredAt, &failedAt); err != nil {
		t.Fatal(err)
	}
	if deliveredAt == nil || failedAt != nil {
		t.Fatalf("late delivery state: delivered=%v failed=%v", deliveredAt, failedAt)
	}
}

func TestMaintenanceSupersededMemoryLifecyclePreservesReplacement(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	tea := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.8, "preference.drink", "tea")
	old, _, err := store.ProposeCandidate(ctx, "user", CandidateProposal{Output: tea, IdempotencyKey: "retention-tea"})
	if err != nil {
		t.Fatal(err)
	}
	coffee := evaluatedClaimCandidate(t, "I prefer coffee", "The user prefers coffee.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.95, "preference.drink", "coffee")
	replacement, _, err := store.ProposeCandidate(ctx, "user", CandidateProposal{Output: coffee, IdempotencyKey: "retention-coffee"})
	if err != nil {
		t.Fatal(err)
	}
	policy := maintenanceTestPolicy()
	now := time.Now().UTC()
	changed := formatTime(now.Add(-policy.CandidateContentRetention))
	if _, err := store.sql.Exec(`UPDATE memory_entries SET status_changed_at = ? WHERE id = ?`, changed, old.PublishedMemoryID); err != nil {
		t.Fatal(err)
	}
	pre, err := store.MaintenanceSweep(ctx, now.Add(-time.Second), policy)
	if err != nil || pre.SupersededMemoriesRedacted != 0 {
		t.Fatalf("pre-boundary counts=%+v err=%v", pre, err)
	}
	counts, err := store.MaintenanceSweep(ctx, now, policy)
	if err != nil || counts.SupersededMemoriesRedacted != 1 || counts.SupersedesLinksCleared < 1 {
		t.Fatalf("redaction counts=%+v err=%v", counts, err)
	}
	var oldStatement, oldSlot, oldValue, oldEvidence, replacementStatement, replacementEvidence string
	if err := store.sql.QueryRow(`SELECT memory.statement, memory.claim_slot, memory.claim_value, candidate.evidence FROM memory_entries memory JOIN memory_candidates candidate ON candidate.published_memory_id = memory.id WHERE memory.id = ?`, old.PublishedMemoryID).Scan(&oldStatement, &oldSlot, &oldValue, &oldEvidence); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT memory.statement, candidate.evidence FROM memory_entries memory JOIN memory_candidates candidate ON candidate.published_memory_id = memory.id WHERE memory.id = ?`, replacement.PublishedMemoryID).Scan(&replacementStatement, &replacementEvidence); err != nil {
		t.Fatal(err)
	}
	if oldStatement != "" || oldSlot != "" || oldValue != "" || oldEvidence != "" {
		t.Fatalf("superseded content retained: %q %q %q %q", oldStatement, oldSlot, oldValue, oldEvidence)
	}
	if replacementStatement == "" || replacementEvidence == "" {
		t.Fatal("active replacement content was redacted")
	}
	if _, err := store.MaintenanceSweep(ctx, now.Add(policy.ContentFreeTombstoneRetention), policy); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaintenanceSweep(ctx, now.Add(2*policy.ContentFreeTombstoneRetention), policy); err != nil {
		t.Fatal(err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_entries WHERE id = ?`, 0, old.PublishedMemoryID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_entries WHERE id = ? AND status = 'active'`, 1, replacement.PublishedMemoryID)
}

func TestMaintenancePrunesDerivedHistoryAndRetainsLiveReceipt(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: "live receipt"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := formatTime(now.Add(-48 * time.Hour))
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET state = 'succeeded', completed_at = ?, updated_at = ? WHERE job_kind = 'derived_index' AND entity_kind = 'memory' AND entity_id = ?`, old, old, memory.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO durable_jobs(job_kind,idempotency_key,canonical_user_id,state,entity_kind,entity_id,operation,available_at,completed_at,created_at,updated_at) VALUES ('derived_index','older-receipt','user','succeeded','memory',?,'upsert',?,?,?,?)`, memory.ID, old, old, old, old); err != nil {
		t.Fatal(err)
	}
	policy := maintenanceTestPolicy()
	counts, err := store.MaintenanceSweep(ctx, now, policy)
	if err != nil || counts.DerivedIndexJobsDeleted != 1 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND state = 'succeeded' AND entity_kind = 'memory' AND entity_id = ?`, 1, memory.ID)
	if err := store.ReconcileDerivedIndexChanges(ctx); err != nil {
		t.Fatal(err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND entity_kind = 'memory' AND entity_id = ?`, 1, memory.ID)
}

func TestMaintenanceSweepErasesDueForgottenMemoryAtBoundary(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	policy := maintenanceTestPolicy()
	now := time.Now().UTC()
	memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Category: "identity", Statement: "retained secret", Evidence: "secret evidence"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "user", profile.Generation, "retained secret", "ack", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = created_at WHERE id = ?; UPDATE memory_candidates SET source_turn_id = ? WHERE published_memory_id = ?`, turn.ID, turn.ID, memory.ID); err != nil {
		t.Fatal(err)
	}
	memoryRevision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	memoryRecord, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(ctx, memoryRevision, memoryRecord, nil); err != nil {
		t.Fatal(err)
	}
	transcriptRevision, err := store.CreateIndexRevision(ctx, IndexKindTranscriptFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	transcriptRecord, err := store.TranscriptIndexRecordByID(ctx, turn.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTranscriptIndexRecord(ctx, transcriptRevision, transcriptRecord); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ForgetMemory(ctx, "user", hashText("actor"), memory.ID, "request", now, policy); err != nil {
		t.Fatal(err)
	}
	if counts, err := store.MaintenanceSweep(ctx, now.Add(policy.ForgottenContentGrace-time.Nanosecond), policy); err != nil || counts.ForgottenMemories != 0 {
		t.Fatalf("pre-boundary counts=%+v err=%v", counts, err)
	}
	counts, err := store.MaintenanceSweep(ctx, now.Add(policy.ForgottenContentGrace), policy)
	if err != nil || counts.ForgottenMemories != 1 {
		t.Fatalf("boundary counts=%+v err=%v", counts, err)
	}
	var status, statement, evidence string
	if err := store.sql.QueryRow(`SELECT memory.status, memory.statement, COALESCE((SELECT candidate.evidence FROM memory_candidates candidate WHERE candidate.published_memory_id = memory.id LIMIT 1), '') FROM memory_entries memory WHERE memory.id = ?`, memory.ID).Scan(&status, &statement, &evidence); err != nil {
		t.Fatal(err)
	}
	if status != StatusDeleted || statement != "" || evidence != "" {
		t.Fatalf("status=%q statement=%q evidence=%q", status, statement, evidence)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM session_turns WHERE id = ?`, 0, turn.ID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+memoryRevision.TableName+` WHERE rowid = ?`, 0, memory.ID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+transcriptRevision.TableName+` WHERE rowid = ?`, 0, turn.ID)
	if repeated, err := store.MaintenanceSweep(ctx, now.Add(policy.ForgottenContentGrace), policy); err != nil || repeated.ForgottenMemories != 0 {
		t.Fatalf("repeated counts=%+v err=%v", repeated, err)
	}
}

func TestMaintenanceSweepDeletesPrivacyTombstoneGraphForActiveUser(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	output := evaluatedFormationCandidate(t, "I build Atlas.", "I build Atlas.", "The user builds Atlas.", memoryformation.CategoryProjects)
	candidate, _, err := store.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, IdempotencyKey: "privacy-tombstone"})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.PublishedMemoryID == 0 {
		t.Fatal("candidate was not published")
	}
	now := time.Now().UTC()
	if _, err := store.DeleteMemory(ctx, "user", hashText("actor"), candidate.PublishedMemoryID, "delete-memory", now); err != nil {
		t.Fatal(err)
	}

	policy := maintenanceTestPolicy()
	sweepAt := now.Add(policy.ContentFreeTombstoneRetention + time.Hour)
	var deleted MaintenanceCounts
	for range 3 {
		counts, err := store.MaintenanceSweep(ctx, sweepAt, policy)
		if err != nil {
			t.Fatal(err)
		}
		deleted.AuditTombstones += counts.AuditTombstones
		deleted.CandidateTombstones += counts.CandidateTombstones
		deleted.MemoryTombstonesDeleted += counts.MemoryTombstonesDeleted
	}
	if deleted.AuditTombstones == 0 || deleted.CandidateTombstones != 1 || deleted.MemoryTombstonesDeleted != 1 {
		t.Fatalf("unexpected tombstone deletion counts: %+v", deleted)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_events WHERE event_kind = 'formation_audit' AND (candidate_id = ? OR memory_id = ?)`, 0, candidate.ID, candidate.PublishedMemoryID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_candidates WHERE id = ?`, 0, candidate.ID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_entries WHERE id = ?`, 0, candidate.PublishedMemoryID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM account_users WHERE canonical_user_id = 'user' AND lifecycle_state = 'active'`, 1)
}

func TestMaintenanceSweepBoundsLegacyExpiryCategories(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	for _, statement := range []string{"temporary one", "temporary two", "temporary three"} {
		if _, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeShortTerm, Statement: statement, TTL: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	policy := maintenanceTestPolicy()
	policy.BatchSize = 2
	first, err := store.MaintenanceSweep(ctx, time.Now().UTC().Add(2*time.Minute), policy)
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionCleanup.MemoryEntriesExpired != 2 {
		t.Fatalf("first bounded expiry count = %d, want 2", first.SessionCleanup.MemoryEntriesExpired)
	}
	second, err := store.MaintenanceSweep(ctx, time.Now().UTC().Add(2*time.Minute), policy)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionCleanup.MemoryEntriesExpired != 1 {
		t.Fatalf("second bounded expiry count = %d, want 1", second.SessionCleanup.MemoryEntriesExpired)
	}
}

func TestMaintenanceSweepDoesNotStarveLaterCandidateBatches(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	for i, statement := range []string{"candidate one", "candidate two"} {
		output := evaluatedFormationCandidate(t, statement, statement, statement, memoryformation.CategoryProjects)
		candidate, _, err := store.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, IdempotencyKey: fmt.Sprintf("retention-candidate-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.sql.Exec(`UPDATE memory_candidates SET created_at = ?, updated_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-3*time.Hour)), formatTime(time.Now().UTC().Add(-3*time.Hour)), candidate.ID); err != nil {
			t.Fatal(err)
		}
	}
	policy := maintenanceTestPolicy()
	policy.BatchSize = 1
	if _, err := store.MaintenanceSweep(ctx, time.Now().UTC(), policy); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaintenanceSweep(ctx, time.Now().UTC(), policy); err != nil {
		t.Fatal(err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = 'user' AND statement = ''`, 2)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = 'user' AND redacted_at IS NOT NULL AND redaction_reason = 'candidate_retention_expired'`, 2)
}

func TestMaintenancePreservesEvidenceForActivePublishedMemory(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	output := evaluatedFormationCandidate(t, "I use Go for Atlas", "I use Go for Atlas", "The user uses Go for Atlas.", memoryformation.CategoryProjects)
	candidate, _, err := store.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, IdempotencyKey: "active-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.PublicationStatus != "published" || candidate.PublishedMemoryID == 0 {
		t.Fatalf("candidate was not atomically published: %+v", candidate)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE memory_candidates SET created_at = ? WHERE id = ?`, formatTime(now.Add(-3*time.Hour)), candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaintenanceSweep(ctx, now, maintenanceTestPolicy()); err != nil {
		t.Fatal(err)
	}
	var content string
	if err := store.sql.QueryRow(`SELECT evidence FROM memory_candidates WHERE id = ?`, candidate.ID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content == "" {
		t.Fatal("active memory evidence was redacted")
	}
}

func TestMaintenanceSweepBatchProgression(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	policy := maintenanceTestPolicy()
	policy.BatchSize = 1
	now := time.Now().UTC()
	for _, statement := range []string{"first secret", "second secret"} {
		memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: statement})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ForgetMemory(ctx, "user", hashText("actor"), memory.ID, statement, now, policy); err != nil {
			t.Fatal(err)
		}
	}
	for sweep := 0; sweep < 2; sweep++ {
		counts, err := store.MaintenanceSweep(ctx, now.Add(time.Hour), policy)
		if err != nil || counts.ForgottenMemories != 1 {
			t.Fatalf("sweep %d counts=%+v err=%v", sweep, counts, err)
		}
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_entries WHERE status = 'forgotten'`, 0)
}

func TestMaintenanceSweepForeignKeyFailurePreventsRetentionMutation(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	store.sql.SetMaxOpenConns(1)
	seedAccountUsers(t, store, "user")
	policy := maintenanceTestPolicy()
	now := time.Now().UTC()
	memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: "must remain until a valid sweep"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ForgetMemory(ctx, "user", hashText("actor"), memory.ID, "request", now, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO memory_events(canonical_user_id,event_type,created_at) VALUES ('missing-user','invalid',?)`, formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaintenanceSweep(ctx, now.Add(time.Hour), policy); err == nil {
		t.Fatal("maintenance accepted a foreign key violation")
	}
	var status string
	if err := store.sql.QueryRow(`SELECT status FROM memory_entries WHERE id = ?`, memory.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "forgotten" {
		t.Fatalf("retention mutated before integrity check: status=%q", status)
	}
}

func TestMaintenanceSweepSchedulesOptimizeByPolicy(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	policy := maintenanceTestPolicy()
	now := time.Now().UTC()
	first, err := store.MaintenanceSweep(ctx, now, policy)
	if err != nil || !first.OptimizeRun {
		t.Fatalf("first sweep=%+v err=%v", first, err)
	}
	second, err := store.MaintenanceSweep(ctx, now.Add(policy.DatabaseOptimizeInterval-time.Nanosecond), policy)
	if err != nil || second.OptimizeRun {
		t.Fatalf("early sweep=%+v err=%v", second, err)
	}
	third, err := store.MaintenanceSweep(ctx, now.Add(policy.DatabaseOptimizeInterval+time.Second), policy)
	if err != nil || !third.OptimizeRun {
		t.Fatalf("boundary sweep=%+v err=%v", third, err)
	}
}

func TestMaintenanceSweepRedactsAndPrunesRetentionArtifacts(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	policy := maintenanceTestPolicy()
	now := time.Now().UTC()
	old := formatTime(now.Add(-3 * time.Hour))
	hash := strings.Repeat("a", 64)
	profile, err := store.ResolveSessionProfile(ctx, "user", "old-session", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "old-session", "user", profile.Generation, "source", "answer", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFormationEligible(ctx, "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	_, err = store.sql.Exec(`
INSERT INTO memory_events(canonical_user_id,event_kind,idempotency_key,event_type,request_id,session_id,actor_type,actor_id,created_at,metadata) VALUES ('user','formation_audit','audit','formed','request','session','system','actor',?, 'sensitive');
INSERT INTO durable_jobs(job_kind,canonical_user_id,idempotency_key,state,source_request_id,source_session_id,source_session_generation,source_turn_id,extractor_version,extraction_payload,available_at,completed_at,created_at,updated_at) VALUES ('memory_formation','user','job','succeeded','','old-session',?,?,'test-v1','payload',?,?,?,?);
INSERT INTO durable_jobs(job_kind,idempotency_key,canonical_user_id,session_id,session_generation,covered_from_turn_id,covered_through_turn_id,state,artifact_payload,available_at,completed_at,created_at,updated_at) VALUES ('session_compaction','compaction-job','user','old-session',?,?,?,'succeeded','artifact',?,?,?,?);
INSERT INTO memory_events(canonical_user_id,event_type,request_id,session_id,created_at,metadata) VALUES ('user','deleted','request','session',?,'metadata');
INSERT INTO account_link_challenges(id,code_hash,initiator_user_id,initiator_gateway,initiator_identifier,created_at,expires_at) VALUES ('challenge','code','user','discord','external',?,?);
INSERT INTO privacy_operations(operation_id,idempotency_key,actor_hash,target_user_id,target_hash,operation_type,target_digest,status,created_at,updated_at,completed_at) VALUES ('operation','operation',?,'user',?,'export_user',?,'completed',?,?,?);
`, old, profile.Generation, turn.ID, old, old, old, old, profile.Generation, turn.ID, turn.ID, old, old, old, old, old, old, formatTime(now.Add(-2*time.Hour)), hash, hash, hash, old, old, old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO memory_events(canonical_user_id,event_kind,idempotency_key,event_type,request_id,session_id,actor_type,actor_id,created_at,metadata) VALUES ('user','formation_audit','future-audit','formed','future-request','future-session','system','actor',?,'future-sensitive')`, formatTime(now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintenanceSweep(ctx, now, policy)
	if err != nil {
		t.Fatal(err)
	}
	if counts.AuditRowsRedacted != 1 || counts.FormationJobsRedacted != 1 || counts.CompactionJobsRedacted != 1 || counts.EventsRedacted != 1 || counts.EventTombstones != 0 || counts.PrivacyTombstones != 1 || counts.ChallengesDeleted != 1 {
		t.Fatalf("counts=%+v", counts)
	}
	var metadata, requestID, actorID string
	if err := store.sql.QueryRow(`SELECT metadata, request_id, actor_id FROM memory_events WHERE event_kind = 'formation_audit' AND idempotency_key = 'audit'`).Scan(&metadata, &requestID, &actorID); err != nil {
		t.Fatal(err)
	}
	if metadata != "" || requestID != "" || actorID != "" {
		t.Fatalf("audit content retained: %q %q %q", metadata, requestID, actorID)
	}
	if err := store.sql.QueryRow(`SELECT metadata, request_id FROM memory_events WHERE event_kind = 'formation_audit' AND idempotency_key = 'future-audit'`).Scan(&metadata, &requestID); err != nil {
		t.Fatal(err)
	}
	if metadata != "future-sensitive" || requestID != "future-request" {
		t.Fatalf("future audit was redacted early: %q %q", metadata, requestID)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM account_link_challenges WHERE id = 'challenge'`, 0)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM privacy_operations WHERE operation_id = 'operation'`, 0)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_events WHERE event_type = 'deleted' AND metadata = ''`, 1)
	if _, err := store.MaintenanceSweep(ctx, now.Add(3*time.Hour), policy); err != nil {
		t.Fatal(err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_events WHERE event_type = 'deleted'`, 0)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_events WHERE event_kind = 'formation_audit' AND idempotency_key = 'audit'`, 0)
}
