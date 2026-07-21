package database

import (
	"fmt"
	"testing"
)

func TestSessionCompactionFreshSchemaAndIdempotency(t *testing.T) {
	db := openTestDB(t)
	for _, table := range []string{"session_summaries", "durable_jobs", "session_turns_fts"} {
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
	if _, err := db.SQL().Exec(`UPDATE session_summaries SET source_turn_ids = json_array(?, ?) WHERE id = ?`, fromID, throughID, summaryID); err == nil {
		t.Fatal("expected immutable summary source update to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	artifact_summary_id, generation_model, generator_version, available_at, created_at, updated_at
) VALUES ('session_compaction', 'job-a', 'user-a', 'session-a', 1, ?, ?, ?, 'model', 'v1', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, fromID, throughID, summaryID); err != nil {
		t.Fatalf("insert compaction job: %v", err)
	}

	var summaryCount, jobCount int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM session_summaries`).Scan(&summaryCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction'`).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if summaryCount != 1 || jobCount != 1 {
		t.Fatalf("baseline changed records: summaries=%d jobs=%d", summaryCount, jobCount)
	}
}

func TestSessionCompactionConstraintsAndTenantIsolation(t *testing.T) {
	db := openTestDB(t)
	insertSessionTestUser(t, db, "user-a")
	insertSessionTestUser(t, db, "user-b")
	a1 := insertSessionTestTurn(t, db, "user-a", "session", 1, "a one", "answer")
	a2 := insertSessionTestTurn(t, db, "user-a", "session", 1, "a two", "answer")
	b1 := insertSessionTestTurn(t, db, "user-b", "session", 1, "b one", "answer")
	otherSession := insertSessionTestTurn(t, db, "user-a", "other-session", 1, "other session", "answer")
	otherGeneration := insertSessionTestTurn(t, db, "user-a", "session", 2, "other generation", "answer")
	summaryID := insertSessionTestSummary(t, db, "user-a", "session", 1, a1, a2)

	if _, err := db.SQL().Exec(`UPDATE session_summaries SET narrative = 'changed' WHERE id = ?`, summaryID); err == nil {
		t.Fatal("expected immutable summary update to fail")
	}
	if _, err := db.SQL().Exec(`
	INSERT INTO session_summaries (
		canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
		narrative, generation_model, generator_version, source_digest, created_at, source_turn_ids
	) VALUES ('user-a', 'session', 1, ?, ?, 'duplicate', 'model', 'v1', 'digest-2', '2026-07-18T12:00:00Z', json_array(?, ?))`, a1, a2, a1, a2); err == nil {
		t.Fatal("expected duplicate summary range to fail")
	}
	if _, err := db.SQL().Exec(`
	INSERT INTO session_summaries (
		canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
		narrative, open_tasks, generation_model, generator_version, source_digest, created_at, source_turn_ids
	) VALUES ('user-a', 'session', 1, ?, ?, 'invalid json', 'not-json', 'model', 'v1', 'digest-3', '2026-07-18T12:00:00Z', json_array(?, ?))`, a1, a2, a1, a2); err == nil {
		t.Fatal("expected invalid summary JSON to fail")
	}
	for name, sourceIDs := range map[string]string{
		"malformed":        `not-json`,
		"noninteger":       fmt.Sprintf(`[%d,"%d"]`, a1, a2),
		"duplicate":        fmt.Sprintf(`[%d,%d,%d]`, a1, a1, a2),
		"unordered":        fmt.Sprintf(`[%d,%d]`, a2, a1),
		"missing endpoint": fmt.Sprintf(`[%d]`, a1),
		"cross tenant":     fmt.Sprintf(`[%d,%d,%d]`, a1, b1, a2),
		"cross session":    fmt.Sprintf(`[%d,%d,%d]`, a1, otherSession, a2),
		"cross generation": fmt.Sprintf(`[%d,%d,%d]`, a1, otherGeneration, a2),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.SQL().Exec(`
INSERT INTO session_summaries (
	canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	narrative, generation_model, generator_version, source_digest, created_at, source_turn_ids
) VALUES ('user-a', 'session', 1, ?, ?, ?, 'model', 'v1', ?, '2026-07-18T12:00:00Z', ?)`, a1, a2, name, "digest-"+name, sourceIDs); err == nil {
				t.Fatalf("expected %s source turn array to fail", name)
			}
		})
	}
	if _, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	attempt_count, redrive_count, available_at, created_at, updated_at
) VALUES ('session_compaction', 'too-many-attempts', 'user-a', 'session', 1, ?, ?, 4, 0, '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, a1, a2); err == nil {
		t.Fatal("expected excessive attempt count to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	available_at, created_at, updated_at
) VALUES ('session_compaction', 'cross-tenant', 'user-a', 'session', 1, ?, ?, '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, a1, b1); err == nil {
		t.Fatal("expected cross-tenant compaction range to fail")
	}
}

func TestSessionTurnsFTSInitializationLeavesLegacyTableUnsynchronized(t *testing.T) {
	db := openTestDB(t)
	insertSessionTestUser(t, db, "user-a")
	insertSessionTestUser(t, db, "user-b")

	insertSessionTestTurn(t, db, "user-a", "session-a", 1, "observed a quasar", "recorded the pulsar")
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
	generation_model, generator_version, source_digest, created_at, source_turn_ids
) VALUES (?, ?, ?, ?, ?, 'A summary.', '["task"]', '["commitment"]', '["entity"]', '["decision"]', '["topic"]', 'model', 'v1', 'digest', '2026-07-18T12:00:00Z', json_array(?, ?))`, userID, sessionID, generation, fromID, throughID, fromID, throughID)
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
