package usermemory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestForgetMemoryImmediatelySuppressesSourceExchangeServingAndWork(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user", "other")
	userProfile, err := store.ResolveSessionProfile(ctx, "user", "shared", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	otherProfile, err := store.ResolveSessionProfile(ctx, "other", "shared", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Category: "identity", Statement: "my private quasar", Evidence: "my private quasar", Confidence: 1, Importance: 5})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "shared", "user", userProfile.Generation, "my private quasar", "ack quasar", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(ctx, "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendSessionTurnForGenerationResult(ctx, "shared", "user", userProfile.Generation, "second exchange", "second ack", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(ctx, "user", second.ID); err != nil {
		t.Fatal(err)
	}
	otherTurn, err := store.AppendSessionTurnForGenerationResult(ctx, "shared", "other", otherProfile.Generation, "other quasar", "other ack", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(ctx, "other", otherTurn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE memory_candidates SET source_turn_id = ? WHERE canonical_user_id = 'user' AND published_memory_id = ?`, turn.ID, memory.ID); err != nil {
		t.Fatal(err)
	}
	formationJob, err := store.EnqueueFormationJob(ctx, FormationSource{TurnID: turn.ID}, "user")
	if err != nil {
		t.Fatal(err)
	}
	summaryJobID, err := store.EnqueueSessionCompactionJob(ctx, "user", "shared", userProfile.Generation, turn.ID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	summaryJob, err := store.ClaimSessionCompactionJob(ctx, "summary-worker", time.Minute)
	if err != nil || summaryJob.ID != summaryJobID {
		t.Fatalf("summary job=%+v err=%v", summaryJob, err)
	}
	if err := store.SaveSessionCompactionArtifact(ctx, summaryJob, SummaryArtifact{Narrative: "private summary", GenerationModel: "model", GeneratorVersion: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishSessionSummary(ctx, summaryJob); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSessionCompactionJob(ctx, summaryJob, false); err != nil {
		t.Fatal(err)
	}
	compactionJob, err := store.EnqueueSessionCompactionJob(ctx, "user", "shared", userProfile.Generation, turn.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	liveTranscript, err := store.LiveIndexRevision(ctx, IndexKindTranscriptFTS)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	policy := maintenanceTestPolicy()
	if status, err := store.ForgetMemory(ctx, "user", hashText("actor"), memory.ID, "forget-request", now, policy); err != nil || status != "forgotten" {
		t.Fatalf("forget status=%q err=%v", status, err)
	}
	var suppressedAt, retainedText string
	if err := store.sql.QueryRow(`SELECT privacy_suppressed_at, user_text FROM session_turns WHERE id = ?`, turn.ID).Scan(&suppressedAt, &retainedText); err != nil {
		t.Fatal(err)
	}
	if suppressedAt != formatTime(now) || retainedText != "my private quasar" {
		t.Fatalf("suppressed_at=%q retained_text=%q", suppressedAt, retainedText)
	}
	recent, err := store.RecentCompletedExchanges(ctx, "user", "shared", userProfile.Generation, 10)
	if err != nil || len(recent) != 1 || recent[0].ID != second.ID {
		t.Fatalf("recent=%+v err=%v", recent, err)
	}
	results, err := store.SearchTranscript(ctx, "user", "shared", userProfile.Generation, "quasar", 10)
	if err != nil || len(results) != 0 {
		t.Fatalf("suppressed transcript results=%+v err=%v", results, err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+liveTranscript.TableName+` WHERE rowid = ?`, 0, turn.ID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM session_summaries WHERE canonical_user_id = 'user'`, 0)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM durable_jobs WHERE id IN (?, ?) AND state = 'skipped'`, 2, formationJob, compactionJob)
	if _, err := store.TranscriptIndexRecordByID(ctx, turn.ID, "user"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("suppressed index record error=%v", err)
	}
	planned, err := store.DeliveredSessionTurnsAfter(ctx, "user", "shared", userProfile.Generation, 0, 10)
	if err != nil || len(planned.Turns) != 1 || planned.Turns[0].ID != second.ID {
		t.Fatalf("planned=%+v err=%v", planned, err)
	}
	if _, err := store.EnqueueSessionCompactionJob(ctx, "user", "shared", userProfile.Generation, turn.ID, second.ID); err == nil {
		t.Fatal("suppressed range was accepted for compaction")
	}
	if _, err := store.EnqueueFormationJob(ctx, FormationSource{TurnID: turn.ID}, "user"); err == nil {
		t.Fatal("suppressed turn was accepted for formation")
	}
	otherResults, err := store.SearchTranscript(ctx, "other", "shared", otherProfile.Generation, "quasar", 10)
	if err != nil || len(otherResults) != 1 || otherResults[0].TurnID != otherTurn.ID {
		t.Fatalf("other tenant results=%+v err=%v", otherResults, err)
	}
	exported, err := store.ExportPrivacy(ctx, "user", now)
	if err != nil || !strings.Contains(string(exported), `"privacy_suppressed_at": "`+suppressedAt+`"`) {
		t.Fatalf("export missing suppression timestamp: err=%v payload=%s", err, exported)
	}

	// A repeated forget repairs stale physical serving rows and resurrected work.
	if _, err := store.sql.Exec(`INSERT INTO `+liveTranscript.TableName+`(rowid, canonical_user_id, session_id, session_generation, user_text, assistant_text) VALUES (?, 'user', 'shared', ?, 'stale quasar', 'stale'); UPDATE durable_jobs SET state = 'retry', completed_at = NULL, last_error_code = '' WHERE id = ?`, turn.ID, userProfile.Generation, compactionJob); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ForgetMemory(ctx, "user", hashText("actor"), memory.ID, "forget-repair", now.Add(time.Minute), policy); err != nil {
		t.Fatal(err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+liveTranscript.TableName+` WHERE rowid = ?`, 0, turn.ID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM durable_jobs WHERE id = ? AND state = 'skipped'`, 1, compactionJob)

	if counts, err := store.MaintenanceSweep(ctx, now.Add(policy.ForgottenContentGrace), policy); err != nil || counts.ForgottenMemories != 1 {
		t.Fatalf("grace deletion counts=%+v err=%v", counts, err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM session_turns WHERE id = ?`, 0, turn.ID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM session_turns WHERE id = ?`, 1, otherTurn.ID)
}

func TestPrivacyInvalidationOutboxRollbackLeaseRetryAndScrub(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	now := time.Now().UTC()

	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueuePrivacyInvalidationTx(ctx, tx, "user", "rolled-back", []string{"gateway:identity"}, []string{"session"}, false, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'privacy_invalidation'`, 0)

	tx, err = store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueuePrivacyInvalidationTx(ctx, tx, "user", "operation", []string{"gateway:identity"}, []string{"session"}, false, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	event, err := store.ClaimPrivacyInvalidation(ctx, now, time.Minute)
	if err != nil || event == nil || event.Attempts != 1 {
		t.Fatalf("first claim=%+v err=%v", event, err)
	}
	if _, err := store.ClaimPrivacyInvalidation(ctx, now.Add(30*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	retried, err := store.ClaimPrivacyInvalidation(ctx, now.Add(time.Minute), time.Minute)
	if err != nil || retried == nil || retried.ID != event.ID || retried.Attempts != 2 {
		t.Fatalf("expired lease claim=%+v err=%v", retried, err)
	}
	if err := store.RetryPrivacyInvalidation(ctx, *retried, now.Add(2*time.Minute), now.Add(time.Minute), "publish_failed"); err != nil {
		t.Fatal(err)
	}
	if early, err := store.ClaimPrivacyInvalidation(ctx, now.Add(90*time.Second), time.Minute); err != nil || early != nil {
		t.Fatalf("early retry claim=%+v err=%v", early, err)
	}
	final, err := store.ClaimPrivacyInvalidation(ctx, now.Add(2*time.Minute), time.Minute)
	if err != nil || final == nil {
		t.Fatalf("final claim=%+v err=%v", final, err)
	}
	if err := store.CompletePrivacyInvalidation(ctx, *final, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var external, sessions, state string
	if err := store.sql.QueryRow(`SELECT external_identities, session_ids, state FROM durable_jobs WHERE id = ? AND job_kind = 'privacy_invalidation'`, final.ID).Scan(&external, &sessions, &state); err != nil {
		t.Fatal(err)
	}
	if external != "[]" || sessions != "[]" || state != "succeeded" {
		t.Fatalf("completed event retained payload: external=%q sessions=%q state=%q", external, sessions, state)
	}
}

func TestReclaimedPrivacyInvalidationLeaseRejectsStaleDispatcher(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	now := time.Now().UTC()
	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueuePrivacyInvalidationTx(ctx, tx, "user", "stale-operation", []string{"gateway:identity"}, []string{"session"}, false, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stale, err := store.ClaimPrivacyInvalidation(ctx, now, time.Minute)
	if err != nil || stale == nil {
		t.Fatalf("stale claim=%+v err=%v", stale, err)
	}
	current, err := store.ClaimPrivacyInvalidation(ctx, now.Add(time.Minute), time.Minute)
	if err != nil || current == nil {
		t.Fatalf("current claim=%+v err=%v", current, err)
	}
	if stale.LeaseOwner == current.LeaseOwner || current.LeaseOwner == "" {
		t.Fatalf("stale owner=%q current owner=%q", stale.LeaseOwner, current.LeaseOwner)
	}
	if err := store.CompletePrivacyInvalidation(ctx, *stale, now.Add(time.Minute)); !errors.Is(err, ErrStalePrivacyInvalidationLease) {
		t.Fatalf("stale complete error=%v", err)
	}
	if err := store.RetryPrivacyInvalidation(ctx, *stale, now.Add(2*time.Minute), now.Add(time.Minute), "stale"); !errors.Is(err, ErrStalePrivacyInvalidationLease) {
		t.Fatalf("stale retry error=%v", err)
	}
	if err := store.CompletePrivacyInvalidation(ctx, *current, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestUserErasureInvalidationSurvivesAccountCascade(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES ('user', ?, ?); INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, display_name, linked_at) VALUES ('websocket', 'external', 'user', '', ?)`, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO websocket_clients(client_id, canonical_user_id, websocket_identifier, client_name, created_at) VALUES ('wsc_client_123456', 'user', 'external', 'Laptop', ?)`, formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	invalidation, err := store.EraseUserWithInvalidationTx(ctx, tx, "user", "delete-user-operation", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(invalidation.ExternalIdentities) != 2 || len(invalidation.SessionIDs) != 1 {
		t.Fatalf("invalidation=%+v", invalidation)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM account_users WHERE canonical_user_id = 'user'`, 0)
	if early, err := store.ClaimPrivacyInvalidation(ctx, now, time.Minute); err != nil || early != nil {
		t.Fatalf("early account-erasure event=%+v err=%v", early, err)
	}
	event, err := store.ClaimPrivacyInvalidation(ctx, now.Add(privacyCloseInvalidationDelay), time.Minute)
	if err != nil || event == nil || !event.CloseConnections || len(event.ExternalIdentities) != 2 || len(event.SessionIDs) != 1 {
		t.Fatalf("surviving event=%+v err=%v", event, err)
	}
	if event.SubjectUserID != "user" {
		t.Fatalf("missing durable subject fence: %+v", event)
	}
}

func TestUserErasureDelayedInvalidationDoesNotTargetRecreatedTenant(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES ('old-user', ?, ?); INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, display_name, linked_at) VALUES ('websocket', 'recreated', 'old-user', '', ?)`, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveSessionProfile(ctx, "old-user", "shared-session", time.Hour); err != nil {
		t.Fatal(err)
	}
	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EraseUserWithInvalidationTx(ctx, tx, "old-user", "erase-recreate", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES ('new-user', ?, ?); INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, display_name, linked_at) VALUES ('websocket', 'recreated', 'new-user', '', ?)`, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveSessionProfile(ctx, "new-user", "shared-session", time.Hour); err != nil {
		t.Fatal(err)
	}
	event, err := store.ClaimPrivacyInvalidation(ctx, now.Add(privacyCloseInvalidationDelay), time.Minute)
	if err != nil || event == nil {
		t.Fatalf("claim delayed erasure: event=%+v err=%v", event, err)
	}
	if len(event.ExternalIdentities) != 0 || len(event.SessionIDs) != 0 {
		t.Fatalf("erasure replay retained recreated tenant scope: %+v", event)
	}
}

func TestPrivacyInvalidationForSameOwnerStillDispatches(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES ('user', ?, ?); INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, display_name, linked_at) VALUES ('discord', 'owner', 'user', '', ?)`, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: "same-owner fact"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveSessionProfile(ctx, "user", "owner-session", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ForgetMemory(ctx, "user", hashText("actor"), memory.ID, "forget-owner", now, config.RetentionPolicy{ForgottenContentGrace: time.Hour}); err != nil {
		t.Fatal(err)
	}
	event, err := store.ClaimPrivacyInvalidation(ctx, now, time.Minute)
	if err != nil || event == nil {
		t.Fatalf("claim same-owner invalidation: event=%+v err=%v", event, err)
	}
	if strings.Join(event.ExternalIdentities, ",") != "discord:owner" || strings.Join(event.SessionIDs, ",") != "owner-session" {
		t.Fatalf("same-owner scope was filtered: %+v", event)
	}
}

func TestPrivacyExportIncludesWebSocketClientMetadataWithoutTokens(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at, speaker_intro) VALUES ('user', ?, ?, 'You are speaking with User.'); INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, display_name, linked_at) VALUES ('websocket', 'external', 'user', 'User', ?)`, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO websocket_clients(client_id, canonical_user_id, websocket_identifier, client_name, refresh_token_hash, refresh_expires_at, created_at) VALUES ('wsc_client_123456', 'user', 'external', 'Laptop', zeroblob(32), ?, ?)`, formatTime(now.Add(time.Hour)), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueuePrivacyInvalidationTx(ctx, tx, "user", "export-hidden-invalidation", []string{"websocket:external"}, nil, true, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	exported, err := store.ExportPrivacy(ctx, "user", now)
	if err != nil {
		t.Fatal(err)
	}
	text := string(exported)
	if !strings.Contains(text, `"websocket_clients"`) || !strings.Contains(text, `"wsc_client_123456"`) || strings.Contains(text, "refresh_token_hash") {
		t.Fatalf("unexpected websocket client export: %s", text)
	}
	if strings.Contains(text, "subject_canonical_user_id") || strings.Contains(text, "privacy_invalidation") {
		t.Fatalf("privacy invalidation fence leaked into export: %s", text)
	}
}

func TestPrivacyHardDeletePurgesCanonicalProfileTranscriptAndRevisions(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	userID := "privacy-user"
	if _, err := store.sql.Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, userID, formatTime(time.Now()), formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	memory, err := store.SaveMemory(ctx, userID, SaveRequest{Scope: ScopeLongTerm, Category: "identity", Statement: "secret statement", Evidence: "secret evidence", Confidence: 1, Importance: 5})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.ResolveSessionProfile(ctx, userID, "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "session", userID, profile.Generation, "secret statement", "ack", nil, time.Hour)
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
	memoryRecord, err := store.MemoryIndexRecordByID(ctx, memory.ID, userID)
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
	transcriptRecord, err := store.TranscriptIndexRecordByID(ctx, turn.ID, userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTranscriptIndexRecord(ctx, transcriptRevision, transcriptRecord); err != nil {
		t.Fatal(err)
	}
	state, err := store.DeleteMemory(ctx, userID, hashText("actor"), memory.ID, "privacy-request", time.Now().UTC())
	if err != nil || state != StatusDeleted {
		t.Fatalf("state=%q err=%v", state, err)
	}
	var status, statement, evidence string
	if err := store.sql.QueryRow(`SELECT memory.status, memory.statement, COALESCE((SELECT candidate.evidence FROM memory_candidates candidate WHERE candidate.published_memory_id = memory.id LIMIT 1), '') FROM memory_entries memory WHERE memory.id = ?`, memory.ID).Scan(&status, &statement, &evidence); err != nil {
		t.Fatal(err)
	}
	if status != StatusDeleted || statement != "" || evidence != "" {
		t.Fatalf("canonical tombstone retained content: status=%q statement=%q evidence=%q", status, statement, evidence)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM session_turns WHERE id = ?`, 0, turn.ID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM sessions WHERE canonical_user_id = ? AND session_id = ? AND generation = ?`, 1, userID, "session", profile.Generation)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM sessions, json_each(sessions.source_memory_ids) source WHERE CAST(source.value AS INTEGER) = ?`, 0, memory.ID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+memoryRevision.TableName+` WHERE rowid = ?`, 0, memory.ID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+transcriptRevision.TableName+` WHERE rowid = ?`, 0, turn.ID)

	// Repeating hard deletion is idempotent and also repairs stale derived rows.
	state, err = store.DeleteMemory(ctx, userID, hashText("actor"), memory.ID, "privacy-request-2", time.Now().UTC())
	if err != nil || state != StatusDeleted {
		t.Fatalf("repeat state=%q err=%v", state, err)
	}
}

func TestPrivacySessionDeletePreservesGenerationHighWater(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	userID := "privacy-user"
	if _, err := store.sql.Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, userID, formatTime(time.Now()), formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	profile, err := store.ResolveSessionProfile(ctx, userID, "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(ctx, "session", userID, profile.Generation, "hello", "world", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteSessionPrivacy(ctx, userID, hashText("actor"), "session", "request", time.Now().UTC())
	if err != nil || deleted != profile.Generation {
		t.Fatalf("generation=%d err=%v", deleted, err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = ? AND session_id = ?`, 0, userID, "session")
	var highWater int
	if err := store.sql.QueryRow(`SELECT generation FROM sessions WHERE canonical_user_id = ? AND session_id = ?`, userID, "session").Scan(&highWater); err != nil {
		t.Fatal(err)
	}
	if highWater <= profile.Generation {
		t.Fatalf("generation high-water=%d, deleted=%d", highWater, profile.Generation)
	}
	if repeated, err := store.DeleteSessionPrivacy(ctx, userID, hashText("actor"), "session", "request-2", time.Now().UTC()); err != nil || repeated != 0 {
		t.Fatalf("repeat generation=%d err=%v", repeated, err)
	}
}

func assertPrivacyCount(t *testing.T, db *sql.DB, query string, want int, args ...any) {
	t.Helper()
	var got int
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("count=%d want %d for %s", got, want, query)
	}
}
