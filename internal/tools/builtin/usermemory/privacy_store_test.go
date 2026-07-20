package usermemory

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestPrivacyInvalidationOutboxRollbackLeaseRetryAndScrub(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	now := time.Now().UTC()

	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueuePrivacyInvalidationTx(ctx, tx, "rolled-back", []string{"gateway:identity"}, []string{"session"}, false, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM privacy_invalidation_events`, 0)

	tx, err = store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueuePrivacyInvalidationTx(ctx, tx, "operation", []string{"gateway:identity"}, []string{"session"}, false, now); err != nil {
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
	if err := store.RetryPrivacyInvalidation(ctx, retried.ID, now.Add(2*time.Minute), now.Add(time.Minute), "publish_failed"); err != nil {
		t.Fatal(err)
	}
	if early, err := store.ClaimPrivacyInvalidation(ctx, now.Add(90*time.Second), time.Minute); err != nil || early != nil {
		t.Fatalf("early retry claim=%+v err=%v", early, err)
	}
	final, err := store.ClaimPrivacyInvalidation(ctx, now.Add(2*time.Minute), time.Minute)
	if err != nil || final == nil {
		t.Fatalf("final claim=%+v err=%v", final, err)
	}
	if err := store.CompletePrivacyInvalidation(ctx, final.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var external, sessions, state string
	if err := store.sql.QueryRow(`SELECT external_identities, session_ids, state FROM privacy_invalidation_events WHERE id = ?`, final.ID).Scan(&external, &sessions, &state); err != nil {
		t.Fatal(err)
	}
	if external != "[]" || sessions != "[]" || state != "completed" {
		t.Fatalf("completed event retained payload: external=%q sessions=%q state=%q", external, sessions, state)
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
}

func TestPrivacyExportIncludesWebSocketClientMetadataWithoutTokens(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES ('user', ?, ?); INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, display_name, linked_at) VALUES ('websocket', 'external', 'user', 'User', ?); INSERT INTO user_memory_profiles(canonical_user_id, intro, created_at, updated_at) VALUES ('user', 'You are speaking with User.', ?, ?)`, formatTime(now), formatTime(now), formatTime(now), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO websocket_clients(client_id, canonical_user_id, websocket_identifier, client_name, refresh_token_hash, refresh_expires_at, created_at) VALUES ('wsc_client_123456', 'user', 'external', 'Laptop', zeroblob(32), ?, ?)`, formatTime(now.Add(time.Hour)), formatTime(now)); err != nil {
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
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = created_at WHERE id = ?; UPDATE memory_entries SET source_turn_id = ? WHERE id = ?`, turn.ID, turn.ID, memory.ID); err != nil {
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
	if err := store.sql.QueryRow(`SELECT status, statement, evidence FROM memory_entries WHERE id = ?`, memory.ID).Scan(&status, &statement, &evidence); err != nil {
		t.Fatal(err)
	}
	if status != StatusDeleted || statement != "" || evidence != "" {
		t.Fatalf("canonical tombstone retained content: status=%q statement=%q evidence=%q", status, statement, evidence)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM session_turns WHERE id = ?`, 0, turn.ID)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM tenant_sessions WHERE canonical_user_id = ? AND session_id = ? AND generation = ?`, 1, userID, "session", profile.Generation)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM tenant_profile_version_facts WHERE source_memory_id = ?`, 0, memory.ID)
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
	if err := store.sql.QueryRow(`SELECT generation FROM tenant_session_generations WHERE canonical_user_id = ? AND session_id = ?`, userID, "session").Scan(&highWater); err != nil {
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
