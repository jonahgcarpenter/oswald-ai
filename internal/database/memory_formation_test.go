package database

import (
	"fmt"
	"testing"
)

const formationTestTime = "2026-07-18T12:00:00Z"

func TestMemoryFormationFreshSchema(t *testing.T) {
	db := openTestDB(t)

	for _, table := range []string{
		"memory_candidates",
		"memory_evidence",
		"durable_jobs",
	} {
		assertSchemaObject(t, db, "table", table)
	}
	for _, index := range []string{
		"idx_memory_candidates_state",
		"idx_memory_candidates_statement",
		"idx_memory_evidence_candidate",
		"idx_durable_jobs_ready",
		"idx_memory_events_audit_key",
		"idx_memory_entries_candidate",
	} {
		assertSchemaObject(t, db, "index", index)
	}
	assertSchemaObject(t, db, "view", "memory_formation_audit")
	for _, trigger := range []string{"memory_formation_audit_update", "memory_formation_audit_delete"} {
		assertSchemaObject(t, db, "trigger", trigger)
	}
	for _, column := range []string{
		"candidate_id", "provenance_type", "source_authority", "source_request_id", "source_turn_id",
		"formation_mode", "sensitivity", "approval_state", "approved_at", "approved_by",
		"valid_from", "valid_until", "invalidated_at", "invalidation_reason", "erased_at",
		"erasure_reason", "erasure_request_id",
	} {
		assertTableColumn(t, db, "memory_entries", column)
	}

}

func TestMemoryFormationStateAndIdempotencyConstraints(t *testing.T) {
	db := openTestDB(t)
	insertFormationUser(t, db, "user-a")
	insertFormationUser(t, db, "user-b")
	turnID := insertFormationTurn(t, db, "user-a", "session-a")

	for i, state := range []string{"proposed", "pending_confirmation", "approved", "rejected"} {
		insertFormationCandidate(t, db, "user-a", fmt.Sprintf("candidate-%d", i), state)
	}
	if _, err := db.SQL().Exec(candidateInsertSQL,
		"user-a", "invalid-candidate", "active", "Invalid candidate", "invalid candidate", formationTestTime, formationTestTime,
	); err == nil {
		t.Fatal("expected invalid candidate state to fail")
	}
	if _, err := db.SQL().Exec(candidateInsertSQL,
		"user-a", "candidate-0", "proposed", "Duplicate candidate", "duplicate candidate", formationTestTime, formationTestTime,
	); err == nil {
		t.Fatal("expected duplicate candidate idempotency key to fail")
	}

	for i, state := range []string{"queued", "running", "retry", "succeeded", "skipped", "dead"} {
		if _, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, job_type, state, source_session_generation, source_turn_id, extractor_version, available_at, created_at, updated_at
) VALUES ('memory_formation', ?, ?, 'post_turn', ?, 1, ?, 'test-v1', ?, ?, ?)`, "user-a", fmt.Sprintf("job-%d", i), state, turnID, formationTestTime, formationTestTime, formationTestTime); err != nil {
			t.Fatalf("insert job state %q: %v", state, err)
		}
	}
	if _, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, job_type, state, source_session_generation, source_turn_id, extractor_version, available_at, created_at, updated_at
) VALUES ('memory_formation', 'user-a', 'invalid-job', 'post_turn', 'failed', 1, ?, 'test-v1', ?, ?, ?)`, turnID, formationTestTime, formationTestTime, formationTestTime); err == nil {
		t.Fatal("expected invalid job state to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, job_type, state, source_session_generation, source_turn_id, extractor_version, available_at, created_at, updated_at
) VALUES ('memory_formation', 'user-a', 'job-0', 'post_turn', 'queued', 1, ?, 'test-v1', ?, ?, ?)`, turnID, formationTestTime, formationTestTime, formationTestTime); err == nil {
		t.Fatal("expected duplicate job idempotency key to fail")
	}
}

func TestMemoryFormationTenantConstraintsAndCascades(t *testing.T) {
	db := openTestDB(t)
	insertFormationUser(t, db, "user-a")
	insertFormationUser(t, db, "user-b")

	turnResult, err := db.SQL().Exec(`
INSERT INTO session_turns (session_id, canonical_user_id, user_text, assistant_text, created_at)
VALUES ('session-a', 'user-a', 'hello', 'hi', ?)`, formationTestTime)
	if err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	turnID, err := turnResult.LastInsertId()
	if err != nil {
		t.Fatalf("turn id: %v", err)
	}
	if _, err := db.SQL().Exec(candidateInsertWithTurnSQL,
		"user-b", "wrong-turn", "proposed", "Wrong turn", "wrong turn", turnID, formationTestTime, formationTestTime,
	); err == nil {
		t.Fatal("expected cross-tenant candidate source turn to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, job_type, state, source_session_generation, source_turn_id, extractor_version, available_at, created_at, updated_at
) VALUES ('memory_formation', 'user-b', 'wrong-turn-job', 'post_turn', 'queued', 1, ?, 'test-v1', ?, ?, ?)`, turnID, formationTestTime, formationTestTime, formationTestTime); err == nil {
		t.Fatal("expected cross-tenant job source turn to fail")
	}

	candidateID := insertFormationCandidate(t, db, "user-a", "candidate-a", "approved")
	memoryResult, err := db.SQL().Exec(`
INSERT INTO memory_entries (
	canonical_user_id, scope, category, statement, statement_key, evidence,
	candidate_id, created_at, updated_at
) VALUES ('user-a', 'long_term', 'projects', 'Builds Oswald.', 'builds oswald.', 'Explicit statement.', ?, ?, ?)`, candidateID, formationTestTime, formationTestTime)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	memoryID, err := memoryResult.LastInsertId()
	if err != nil {
		t.Fatalf("memory id: %v", err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO memory_entries (
	canonical_user_id, scope, category, statement, statement_key, evidence,
	candidate_id, created_at, updated_at
) VALUES ('user-b', 'long_term', 'projects', 'Wrong owner.', 'wrong owner.', 'none', ?, ?, ?)`, candidateID, formationTestTime, formationTestTime); err == nil {
		t.Fatal("expected cross-tenant canonical memory candidate to fail")
	}
	if _, err := db.SQL().Exec(`UPDATE memory_candidates SET published_memory_id = ? WHERE id = ?`, memoryID, candidateID); err != nil {
		t.Fatalf("publish candidate: %v", err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO memory_evidence (
	canonical_user_id, candidate_id, idempotency_key, evidence_type, content, created_at
) VALUES ('user-b', ?, 'cross-tenant-evidence', 'turn_quote', 'wrong tenant', ?)`, candidateID, formationTestTime); err == nil {
		t.Fatal("expected cross-tenant evidence link to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO memory_evidence (
	canonical_user_id, candidate_id, idempotency_key, evidence_type, content, created_at
) VALUES ('user-a', ?, 'evidence-a', 'turn_quote', 'I build Oswald.', ?)`, candidateID, formationTestTime); err != nil {
		t.Fatalf("insert evidence: %v", err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO memory_evidence (
	canonical_user_id, candidate_id, memory_id, idempotency_key, evidence_type, content, created_at
) VALUES ('user-a', ?, ?, 'ambiguous-evidence', 'turn_quote', 'invalid', ?)`, candidateID, memoryID, formationTestTime); err == nil {
		t.Fatal("expected evidence with two owners to fail")
	}

	if _, err := db.SQL().Exec(`DELETE FROM memory_candidates WHERE id = ?`, candidateID); err != nil {
		t.Fatalf("delete evidence candidate: %v", err)
	}
	assertRowCount(t, db, "memory_evidence", 0)
}

func TestMemoryFormationNullableSourceCascades(t *testing.T) {
	db := openTestDB(t)
	insertFormationUser(t, db, "user-a")
	turnResult, err := db.SQL().Exec(`
INSERT INTO session_turns (session_id, canonical_user_id, user_text, assistant_text, created_at)
VALUES ('session-a', 'user-a', 'hello', 'hi', ?)`, formationTestTime)
	if err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	turnID, err := turnResult.LastInsertId()
	if err != nil {
		t.Fatalf("turn id: %v", err)
	}
	candidateResult, err := db.SQL().Exec(candidateInsertWithTurnSQL,
		"user-a", "candidate-a", "approved", "Statement", "statement", turnID, formationTestTime, formationTestTime,
	)
	if err != nil {
		t.Fatalf("insert sourced candidate: %v", err)
	}
	candidateID, err := candidateResult.LastInsertId()
	if err != nil {
		t.Fatalf("candidate id: %v", err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, job_type, state, source_session_generation, source_turn_id, extractor_version, available_at, created_at, updated_at
) VALUES ('memory_formation', 'user-a', 'job-a', 'post_turn', 'succeeded', 1, ?, 'test-v1', ?, ?, ?)`, turnID, formationTestTime, formationTestTime, formationTestTime); err != nil {
		t.Fatalf("insert sourced job: %v", err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO memory_evidence (
	canonical_user_id, candidate_id, idempotency_key, evidence_type, content, source_turn_id, created_at
) VALUES ('user-a', ?, 'evidence-a', 'turn_quote', 'hello', ?, ?)`, candidateID, turnID, formationTestTime); err != nil {
		t.Fatalf("insert sourced evidence: %v", err)
	}

	if _, err := db.SQL().Exec(`DELETE FROM session_turns WHERE id = ?`, turnID); err != nil {
		t.Fatalf("delete source turn: %v", err)
	}
	for _, table := range []string{"memory_candidates", "memory_evidence", "durable_jobs"} {
		var count, withSource int
		query := `SELECT COUNT(*), COUNT(source_turn_id) FROM ` + table
		if table == "durable_jobs" {
			query += ` WHERE job_kind = 'memory_formation'`
		}
		if err := db.SQL().QueryRow(query).Scan(&count, &withSource); err != nil {
			t.Fatalf("inspect %s source: %v", table, err)
		}
		if count != 1 || withSource != 0 {
			t.Fatalf("%s count=%d source_count=%d, want 1 and 0", table, count, withSource)
		}
	}
}

func TestMemoryFormationAuditIsAppendOnlyAndTenantCascadeDeletes(t *testing.T) {
	db := openTestDB(t)
	insertFormationUser(t, db, "user-a")
	insertFormationUser(t, db, "user-b")
	insertFormationCandidate(t, db, "user-a", "candidate-a", "proposed")
	turnID := insertFormationTurn(t, db, "user-a", "session-a")
	if _, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, job_type, state, source_session_generation, source_turn_id, extractor_version, available_at, created_at, updated_at
) VALUES ('memory_formation', 'user-a', 'job-a', 'post_turn', 'queued', 1, ?, 'test-v1', ?, ?, ?)`, turnID, formationTestTime, formationTestTime, formationTestTime); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO memory_formation_audit (
	canonical_user_id, idempotency_key, event_type, actor_type, created_at
) VALUES ('user-a', 'audit-a', 'candidate.proposed', 'agent', ?)`, formationTestTime); err != nil {
		t.Fatalf("insert audit: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE memory_formation_audit SET event_type = 'changed' WHERE idempotency_key = 'audit-a'`); err == nil {
		t.Fatal("expected audit update to fail")
	}
	if _, err := db.SQL().Exec(`UPDATE memory_formation_audit SET canonical_user_id = 'user-b' WHERE idempotency_key = 'audit-a'`); err == nil {
		t.Fatal("expected audit ownership update to fail")
	}
	if _, err := db.SQL().Exec(`DELETE FROM memory_formation_audit WHERE idempotency_key = 'audit-a'`); err == nil {
		t.Fatal("expected direct audit delete to fail")
	}

	if _, err := db.SQL().Exec(`DELETE FROM account_users WHERE canonical_user_id = 'user-a'`); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	for _, table := range []string{"memory_candidates", "durable_jobs", "memory_formation_audit"} {
		assertRowCount(t, db, table, 0)
	}
}

func TestMemoryEventsRejectInvalidTenantReferences(t *testing.T) {
	db := openTestDB(t)
	insertFormationUser(t, db, "user-a")
	insertFormationUser(t, db, "user-b")
	turnA := insertFormationTurn(t, db, "user-a", "session-a")
	turnB := insertFormationTurn(t, db, "user-b", "session-b")
	candidateA := insertFormationCandidate(t, db, "user-a", "candidate-a", "approved")
	candidateB := insertFormationCandidate(t, db, "user-b", "candidate-b", "approved")
	memoryA := insertFormationMemory(t, db, "user-a", "memory-a")
	memoryB := insertFormationMemory(t, db, "user-b", "memory-b")
	jobA := insertFormationJob(t, db, "user-a", "job-a", turnA)
	jobB := insertFormationJob(t, db, "user-b", "job-b", turnB)
	derivedJob := insertDerivedIndexJob(t, db, "user-a", "derived-a", memoryA)

	validResult, err := db.SQL().Exec(`
INSERT INTO memory_events (
	canonical_user_id, memory_id, event_type, created_at, candidate_id, job_id, turn_id
) VALUES ('user-a', ?, 'formation.valid', ?, ?, ?, ?)`, memoryA, formationTestTime, candidateA, jobA, turnA)
	if err != nil {
		t.Fatalf("insert valid memory event: %v", err)
	}
	validID, err := validResult.LastInsertId()
	if err != nil {
		t.Fatalf("valid memory event id: %v", err)
	}

	invalid := []struct {
		name   string
		column string
		value  int64
	}{
		{name: "memory owner", column: "memory_id", value: memoryB},
		{name: "candidate owner", column: "candidate_id", value: candidateB},
		{name: "job owner", column: "job_id", value: jobB},
		{name: "job kind", column: "job_id", value: derivedJob},
		{name: "turn owner", column: "turn_id", value: turnB},
	}
	for i, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			insertSQL := fmt.Sprintf(`INSERT INTO memory_events (canonical_user_id, event_type, created_at, %s) VALUES ('user-a', ?, ?, ?)`, test.column)
			if _, err := db.SQL().Exec(insertSQL, fmt.Sprintf("invalid.insert.%d", i), formationTestTime, test.value); err == nil {
				t.Fatal("expected invalid memory event insert to fail")
			}
			updateSQL := fmt.Sprintf(`UPDATE memory_events SET %s = ? WHERE id = ?`, test.column)
			if _, err := db.SQL().Exec(updateSQL, test.value, validID); err == nil {
				t.Fatal("expected invalid memory event update to fail")
			}
		})
	}

	if _, err := db.SQL().Exec(`
INSERT INTO memory_formation_audit (
	canonical_user_id, idempotency_key, event_type, candidate_id, actor_type, created_at
) VALUES ('user-a', 'invalid-view-reference', 'candidate.invalid', ?, 'agent', ?)`, candidateB, formationTestTime); err == nil {
		t.Fatal("expected audit view insert with cross-tenant reference to fail")
	}
}

const candidateInsertSQL = `
INSERT INTO memory_candidates (
	canonical_user_id, idempotency_key, state, scope, category, statement, statement_key,
	provenance_type, formation_mode, created_at, updated_at
) VALUES (?, ?, ?, 'long_term', 'projects', ?, ?, 'explicit_user', 'automatic', ?, ?)`

const candidateInsertWithTurnSQL = `
INSERT INTO memory_candidates (
	canonical_user_id, idempotency_key, state, scope, category, statement, statement_key,
	provenance_type, source_turn_id, formation_mode, created_at, updated_at
) VALUES (?, ?, ?, 'long_term', 'projects', ?, ?, 'explicit_user', ?, 'automatic', ?, ?)`

func insertFormationUser(t *testing.T, db *DB, userID string) {
	t.Helper()
	if _, err := db.SQL().Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, userID, formationTestTime, formationTestTime); err != nil {
		t.Fatalf("insert user %q: %v", userID, err)
	}
}

func insertFormationCandidate(t *testing.T, db *DB, userID, key, state string) int64 {
	t.Helper()
	result, err := db.SQL().Exec(candidateInsertSQL,
		userID, key, state, "Statement "+key, "statement "+key, formationTestTime, formationTestTime,
	)
	if err != nil {
		t.Fatalf("insert candidate %q: %v", key, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("candidate %q id: %v", key, err)
	}
	return id
}

func insertFormationTurn(t *testing.T, db *DB, userID, sessionID string) int64 {
	t.Helper()
	result, err := db.SQL().Exec(`INSERT INTO session_turns (session_id, canonical_user_id, session_generation, user_text, assistant_text, created_at) VALUES (?, ?, 1, 'hello', 'hi', ?)`, sessionID, userID, formationTestTime)
	if err != nil {
		t.Fatalf("insert formation turn: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("formation turn id: %v", err)
	}
	return id
}

func insertFormationMemory(t *testing.T, db *DB, userID, key string) int64 {
	t.Helper()
	result, err := db.SQL().Exec(`
INSERT INTO memory_entries (
	canonical_user_id, scope, category, statement, statement_key, evidence, created_at, updated_at
) VALUES (?, 'long_term', 'notes', ?, ?, 'evidence', ?, ?)`, userID, "Statement "+key, key, formationTestTime, formationTestTime)
	if err != nil {
		t.Fatalf("insert memory %q: %v", key, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("memory %q id: %v", key, err)
	}
	return id
}

func insertFormationJob(t *testing.T, db *DB, userID, key string, turnID int64) int64 {
	t.Helper()
	result, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, job_type, source_session_generation,
	source_turn_id, extractor_version, available_at, created_at, updated_at
) VALUES ('memory_formation', ?, ?, 'post_turn', 1, ?, 'test-v1', ?, ?, ?)`, userID, key, turnID, formationTestTime, formationTestTime, formationTestTime)
	if err != nil {
		t.Fatalf("insert formation job %q: %v", key, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("formation job %q id: %v", key, err)
	}
	return id
}

func insertDerivedIndexJob(t *testing.T, db *DB, userID, key string, memoryID int64) int64 {
	t.Helper()
	result, err := db.SQL().Exec(`
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, entity_kind, entity_id, operation,
	available_at, created_at, updated_at
) VALUES ('derived_index', ?, ?, 'memory', ?, 'upsert', ?, ?, ?)`, userID, key, memoryID, formationTestTime, formationTestTime, formationTestTime)
	if err != nil {
		t.Fatalf("insert derived-index job %q: %v", key, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("derived-index job %q id: %v", key, err)
	}
	return id
}

func assertSchemaObject(t *testing.T, db *DB, objectType, name string) {
	t.Helper()
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, objectType, name).Scan(&count); err != nil {
		t.Fatalf("inspect %s %s: %v", objectType, name, err)
	}
	if count != 1 {
		t.Fatalf("%s %s count = %d, want 1", objectType, name, count)
	}
}

func assertTableColumn(t *testing.T, db *DB, table, column string) {
	t.Helper()
	rows, err := db.SQL().Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("inspect %s columns: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan %s columns: %v", table, err)
		}
		if name == column {
			return
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s columns: %v", table, err)
	}
	t.Fatalf("column %s.%s not found", table, column)
}

func assertRowCount(t *testing.T, db *DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s row count = %d, want %d", table, got, want)
	}
}
