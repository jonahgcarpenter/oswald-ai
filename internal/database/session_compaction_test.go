package database

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestSessionCompactionFreshSchemaAndIdempotency(t *testing.T) {
	db := openTestDB(t)
	for _, table := range []string{"session_summaries", "session_summary_sources", "session_compaction_jobs", "session_turns_fts"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s count=%d err=%v", table, count, err)
		}
	}
	var deliveredColumn int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('session_turns') WHERE name = 'delivered_at'`).Scan(&deliveredColumn); err != nil || deliveredColumn != 1 {
		t.Fatalf("delivered_at column count=%d err=%v", deliveredColumn, err)
	}
	var deliveryFailedColumn int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('session_turns') WHERE name = 'delivery_failed_at'`).Scan(&deliveryFailedColumn); err != nil || deliveryFailedColumn != 1 {
		t.Fatalf("delivery_failed_at column count=%d err=%v", deliveryFailedColumn, err)
	}

	insertSessionTestUser(t, db, "user-a")
	fromID := insertSessionTestTurn(t, db, "user-a", "session-a", 1, "first telescope note", "first answer")
	throughID := insertSessionTestTurn(t, db, "user-a", "session-a", 1, "second telescope note", "second answer")
	summaryID := insertSessionTestSummary(t, db, "user-a", "session-a", 1, fromID, throughID)
	if _, err := db.SQL().Exec(`
INSERT INTO session_summary_sources (summary_id, canonical_user_id, session_id, session_generation, turn_id, ordinal)
VALUES (?, 'user-a', 'session-a', 1, ?, 0), (?, 'user-a', 'session-a', 1, ?, 1)`, summaryID, fromID, summaryID, throughID); err != nil {
		t.Fatalf("insert summary sources: %v", err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO session_compaction_jobs (
	canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	artifact_summary_id, generation_model, generator_version, available_at, created_at, updated_at
) VALUES ('user-a', 'session-a', 1, ?, ?, ?, 'model', 'v1', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, fromID, throughID, summaryID); err != nil {
		t.Fatalf("insert compaction job: %v", err)
	}

	if err := db.migrateSessionCompactionSchema(); err != nil {
		t.Fatalf("repeat compaction migration: %v", err)
	}
	if err := db.initializeSessionTurnsFTS5(); err != nil {
		t.Fatalf("repeat transcript FTS migration: %v", err)
	}
	var summaryCount, sourceCount, jobCount int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM session_summaries`).Scan(&summaryCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM session_summary_sources`).Scan(&sourceCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM session_compaction_jobs`).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if summaryCount != 1 || sourceCount != 2 || jobCount != 1 {
		t.Fatalf("migration changed records: summaries=%d sources=%d jobs=%d", summaryCount, sourceCount, jobCount)
	}
}

func TestSessionCompactionConstraintsAndTenantIsolation(t *testing.T) {
	db := openTestDB(t)
	insertSessionTestUser(t, db, "user-a")
	insertSessionTestUser(t, db, "user-b")
	a1 := insertSessionTestTurn(t, db, "user-a", "session", 1, "a one", "answer")
	a2 := insertSessionTestTurn(t, db, "user-a", "session", 1, "a two", "answer")
	b1 := insertSessionTestTurn(t, db, "user-b", "session", 1, "b one", "answer")
	summaryID := insertSessionTestSummary(t, db, "user-a", "session", 1, a1, a2)

	if _, err := db.SQL().Exec(`UPDATE session_summaries SET narrative = 'changed' WHERE id = ?`, summaryID); err == nil {
		t.Fatal("expected immutable summary update to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO session_summaries (
	canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	narrative, generation_model, generator_version, source_digest, created_at
) VALUES ('user-a', 'session', 1, ?, ?, 'duplicate', 'model', 'v1', 'digest-2', '2026-07-18T12:00:00Z')`, a1, a2); err == nil {
		t.Fatal("expected duplicate summary range to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO session_summaries (
	canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	narrative, open_tasks, generation_model, generator_version, source_digest, created_at
) VALUES ('user-a', 'session', 1, ?, ?, 'invalid json', 'not-json', 'model', 'v1', 'digest-3', '2026-07-18T12:00:00Z')`, a1, a2); err == nil {
		t.Fatal("expected invalid summary JSON to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO session_summary_sources (summary_id, canonical_user_id, session_id, session_generation, turn_id, ordinal)
VALUES (?, 'user-a', 'session', 1, ?, 0)`, summaryID, b1); err == nil {
		t.Fatal("expected cross-tenant summary source to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO session_summary_sources (summary_id, canonical_user_id, session_id, session_generation, turn_id, ordinal)
VALUES (?, 'user-a', 'session', 1, ?, 0)`, summaryID, a1); err != nil {
		t.Fatalf("insert valid summary source: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE session_turns SET canonical_user_id = 'user-b' WHERE id = ?`, a1); err == nil {
		t.Fatal("expected referenced turn tenant update to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO session_compaction_jobs (
	canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	attempt_count, redrive_count, available_at, created_at, updated_at
) VALUES ('user-a', 'session', 1, ?, ?, 4, 0, '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, a1, a2); err == nil {
		t.Fatal("expected excessive attempt count to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO session_compaction_jobs (
	canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	available_at, created_at, updated_at
) VALUES ('user-a', 'session', 1, ?, ?, '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, a1, b1); err == nil {
		t.Fatal("expected cross-tenant compaction range to fail")
	}
}

func TestSessionDeliveredAtBackfillsFormationEligibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
CREATE TABLE account_users (canonical_user_id TEXT PRIMARY KEY, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE session_turns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	canonical_user_id TEXT NOT NULL,
	user_text TEXT NOT NULL,
	assistant_text TEXT NOT NULL,
	tool_names TEXT NOT NULL DEFAULT '',
	importance INTEGER NOT NULL DEFAULT 2,
	topic_tags TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	expires_at TEXT,
	session_generation INTEGER NOT NULL DEFAULT 1,
	source_request_id TEXT NOT NULL DEFAULT '',
	formation_eligible_at TEXT
);
INSERT INTO account_users VALUES ('legacy', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO session_turns (session_id, canonical_user_id, user_text, assistant_text, created_at, formation_eligible_at)
VALUES ('session', 'legacy', 'hello', 'hi', '2026-01-01T00:00:00Z', '2026-01-01T00:00:05Z');`); err != nil {
		raw.Close() // nolint:errcheck
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close() // nolint:errcheck
	var deliveredAt string
	if err := db.SQL().QueryRow(`SELECT delivered_at FROM session_turns`).Scan(&deliveredAt); err != nil {
		t.Fatal(err)
	}
	if deliveredAt != "2026-01-01T00:00:05Z" {
		t.Fatalf("delivered_at=%q", deliveredAt)
	}
}

func TestSessionTurnsFTSInitializationLeavesLegacyTableUnsynchronized(t *testing.T) {
	db := openTestDB(t)
	insertSessionTestUser(t, db, "user-a")
	insertSessionTestUser(t, db, "user-b")

	insertSessionTestTurn(t, db, "user-a", "session-a", 1, "observed a quasar", "recorded the pulsar")
	if err := db.initializeSessionTurnsFTS5(); err != nil {
		t.Fatalf("initialize transcript FTS: %v", err)
	}
	assertSessionFTSMatchCount(t, db, "user-a", "session-a", 1, "quasar", 0)
	assertSessionFTSMatchCount(t, db, "user-b", "session-a", 1, "quasar", 0)
	var triggers int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'session_turns_fts_%'`).Scan(&triggers); err != nil || triggers != 0 {
		t.Fatalf("transcript FTS trigger count=%d err=%v", triggers, err)
	}
}

func insertSessionTestUser(t *testing.T, db *DB, userID string) {
	t.Helper()
	if _, err := db.SQL().Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, userID); err != nil {
		t.Fatalf("insert user %s: %v", userID, err)
	}
}

func insertSessionTestTurn(t *testing.T, db *DB, userID, sessionID string, generation int, userText, assistantText string) int64 {
	t.Helper()
	result, err := db.SQL().Exec(`
INSERT INTO session_turns (canonical_user_id, session_id, session_generation, user_text, assistant_text, created_at, delivered_at)
VALUES (?, ?, ?, ?, ?, '2026-07-18T12:00:00Z', '2026-07-18T12:00:01Z')`, userID, sessionID, generation, userText, assistantText)
	if err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("turn id: %v", err)
	}
	return id
}

func insertSessionTestSummary(t *testing.T, db *DB, userID, sessionID string, generation int, fromID, throughID int64) int64 {
	t.Helper()
	result, err := db.SQL().Exec(`
INSERT INTO session_summaries (
	canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	narrative, open_tasks, commitments, entities, decisions, topic_tags,
	generation_model, generator_version, source_digest, created_at
) VALUES (?, ?, ?, ?, ?, 'A summary.', '["task"]', '["commitment"]', '["entity"]', '["decision"]', '["topic"]', 'model', 'v1', 'digest', '2026-07-18T12:00:00Z')`, userID, sessionID, generation, fromID, throughID)
	if err != nil {
		t.Fatalf("insert summary: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("summary id: %v", err)
	}
	return id
}

func assertSessionFTSMatchCount(t *testing.T, db *DB, userID, sessionID string, generation int, query string, want int) {
	t.Helper()
	var got int
	if err := db.SQL().QueryRow(`
SELECT COUNT(*) FROM session_turns_fts
WHERE session_turns_fts MATCH ? AND canonical_user_id = ? AND session_id = ? AND session_generation = ?`, query, userID, sessionID, generation).Scan(&got); err != nil {
		t.Fatalf("query transcript FTS: %v", err)
	}
	if got != want {
		t.Fatalf("transcript FTS count user=%s session=%s generation=%d query=%s got=%d want=%d", userID, sessionID, generation, query, got, want)
	}
}
