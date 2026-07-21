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
		CandidateContentRetention:       time.Hour,
		SuccessfulJobRetention:          24 * time.Hour,
		DeadJobRetention:                48 * time.Hour,
		AccountChallengeGrace:           time.Hour,
		MaintenanceInterval:             time.Hour,
		DatabaseOptimizeInterval:        24 * time.Hour,
		BatchSize:                       100,
	}
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
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = created_at WHERE id = ?; UPDATE memory_entries SET source_turn_id = ? WHERE id = ?`, turn.ID, turn.ID, memory.ID); err != nil {
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
	if err := store.sql.QueryRow(`SELECT status, statement, evidence FROM memory_entries WHERE id = ?`, memory.ID).Scan(&status, &statement, &evidence); err != nil {
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
	if _, err := store.PublishCandidate(ctx, "user", candidate.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE memory_evidence SET created_at = ? WHERE candidate_id = ?`, formatTime(now.Add(-3*time.Hour)), candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaintenanceSweep(ctx, now, maintenanceTestPolicy()); err != nil {
		t.Fatal(err)
	}
	var content, requestID string
	if err := store.sql.QueryRow(`SELECT content, source_request_id FROM memory_evidence WHERE candidate_id = ? ORDER BY id LIMIT 1`, candidate.ID).Scan(&content, &requestID); err != nil {
		t.Fatal(err)
	}
	if content == "" {
		t.Fatalf("active memory evidence was redacted; request_id=%q", requestID)
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
	_, err = store.sql.Exec(`
INSERT INTO memory_formation_audit(canonical_user_id,idempotency_key,event_type,request_id,session_id,actor_type,actor_id,created_at,metadata) VALUES ('user','audit','formed','request','session','system','actor',?, 'sensitive');
INSERT INTO durable_jobs(job_kind,canonical_user_id,idempotency_key,job_type,state,source_request_id,source_session_id,source_session_generation,source_turn_id,extractor_version,extraction_payload,available_at,completed_at,created_at,updated_at) VALUES ('memory_formation','user','job','extract','succeeded','request','session',?,?,'test-v1','payload',?,?,?,?);
INSERT INTO durable_jobs(job_kind,idempotency_key,canonical_user_id,session_id,session_generation,covered_from_turn_id,covered_through_turn_id,state,artifact_payload,available_at,completed_at,last_error_message,created_at,updated_at) VALUES ('session_compaction','compaction-job','user','old-session',?,?,?,'succeeded','artifact',?,?,'message',?,?);
INSERT INTO memory_events(canonical_user_id,event_type,request_id,session_id,created_at,metadata) VALUES ('user','deleted','request','session',?,'metadata');
INSERT INTO account_link_challenges(id,code_hash,initiator_user_id,initiator_gateway,initiator_identifier,created_at,expires_at) VALUES ('challenge','code','user','discord','external',?,?);
INSERT INTO privacy_operations(operation_id,idempotency_key,actor_hash,target_user_id,target_hash,operation_type,target_digest,status,created_at,updated_at,completed_at) VALUES ('operation','operation',?,'user',?,'export_user',?,'completed',?,?,?);
`, old, profile.Generation, turn.ID, old, old, old, old, profile.Generation, turn.ID, turn.ID, old, old, old, old, old, old, formatTime(now.Add(-2*time.Hour)), hash, hash, hash, old, old, old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO memory_formation_audit(canonical_user_id,idempotency_key,event_type,request_id,session_id,actor_type,actor_id,created_at,metadata,content_expires_at) VALUES ('user','future-audit','formed','future-request','future-session','system','actor',?,'future-sensitive',?)`, old, formatTime(now.Add(time.Hour))); err != nil {
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
	if err := store.sql.QueryRow(`SELECT metadata, request_id, actor_id FROM memory_formation_audit WHERE idempotency_key = 'audit'`).Scan(&metadata, &requestID, &actorID); err != nil {
		t.Fatal(err)
	}
	if metadata != "" || requestID != "" || actorID != "" {
		t.Fatalf("audit content retained: %q %q %q", metadata, requestID, actorID)
	}
	if err := store.sql.QueryRow(`SELECT metadata, request_id FROM memory_formation_audit WHERE idempotency_key = 'future-audit'`).Scan(&metadata, &requestID); err != nil {
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
}
