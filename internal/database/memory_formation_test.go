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
		"memory_relations",
		"memory_formation_jobs",
		"memory_formation_audit",
	} {
		assertSchemaObject(t, db, "table", table)
	}
	for _, index := range []string{
		"idx_memory_candidates_state",
		"idx_memory_candidates_statement",
		"idx_memory_evidence_candidate",
		"idx_memory_relations_target_memory",
		"idx_memory_formation_jobs_ready",
		"idx_memory_formation_audit_tenant_time",
		"idx_memory_entries_candidate",
	} {
		assertSchemaObject(t, db, "index", index)
	}
	for _, trigger := range []string{"memory_formation_audit_no_update", "memory_formation_audit_no_delete"} {
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

	var migrationCount int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, memoryFormationMigration).Scan(&migrationCount); err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if migrationCount != 1 {
		t.Fatalf("migration count = %d, want 1", migrationCount)
	}
}

func TestMemoryFormationStateAndIdempotencyConstraints(t *testing.T) {
	db := openTestDB(t)
	insertFormationUser(t, db, "user-a")
	insertFormationUser(t, db, "user-b")

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
INSERT INTO memory_formation_jobs (
	canonical_user_id, idempotency_key, job_type, state, available_at, created_at, updated_at
) VALUES (?, ?, 'post_turn', ?, ?, ?, ?)`, "user-a", fmt.Sprintf("job-%d", i), state, formationTestTime, formationTestTime, formationTestTime); err != nil {
			t.Fatalf("insert job state %q: %v", state, err)
		}
	}
	if _, err := db.SQL().Exec(`
INSERT INTO memory_formation_jobs (
	canonical_user_id, idempotency_key, job_type, state, available_at, created_at, updated_at
) VALUES ('user-a', 'invalid-job', 'post_turn', 'failed', ?, ?, ?)`, formationTestTime, formationTestTime, formationTestTime); err == nil {
		t.Fatal("expected invalid job state to fail")
	}
	if _, err := db.SQL().Exec(`
INSERT INTO memory_formation_jobs (
	canonical_user_id, idempotency_key, job_type, state, available_at, created_at, updated_at
) VALUES ('user-a', 'job-0', 'post_turn', 'queued', ?, ?, ?)`, formationTestTime, formationTestTime, formationTestTime); err == nil {
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
INSERT INTO memory_formation_jobs (
	canonical_user_id, idempotency_key, job_type, state, source_turn_id, available_at, created_at, updated_at
) VALUES ('user-b', 'wrong-turn-job', 'post_turn', 'queued', ?, ?, ?, ?)`, turnID, formationTestTime, formationTestTime, formationTestTime); err == nil {
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

	otherCandidateID := insertFormationCandidate(t, db, "user-a", "candidate-b", "proposed")
	if _, err := db.SQL().Exec(`
INSERT INTO memory_relations (
	canonical_user_id, idempotency_key, relation_type, source_candidate_id, target_memory_id, created_at
) VALUES ('user-a', 'relation-a', 'contradicts', ?, ?, ?)`, otherCandidateID, memoryID, formationTestTime); err != nil {
		t.Fatalf("insert relation: %v", err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM memory_candidates WHERE id = ?`, otherCandidateID); err != nil {
		t.Fatalf("delete source candidate: %v", err)
	}
	assertRowCount(t, db, "memory_relations", 0)

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
INSERT INTO memory_formation_jobs (
	canonical_user_id, idempotency_key, job_type, state, source_turn_id, available_at, created_at, updated_at
) VALUES ('user-a', 'job-a', 'post_turn', 'succeeded', ?, ?, ?, ?)`, turnID, formationTestTime, formationTestTime, formationTestTime); err != nil {
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
	for _, table := range []string{"memory_candidates", "memory_evidence", "memory_formation_jobs"} {
		var count, withSource int
		if err := db.SQL().QueryRow(`SELECT COUNT(*), COUNT(source_turn_id) FROM `+table).Scan(&count, &withSource); err != nil {
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
	if _, err := db.SQL().Exec(`
INSERT INTO memory_formation_jobs (
	canonical_user_id, idempotency_key, job_type, state, available_at, created_at, updated_at
) VALUES ('user-a', 'job-a', 'post_turn', 'queued', ?, ?, ?)`, formationTestTime, formationTestTime, formationTestTime); err != nil {
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
	for _, table := range []string{"memory_candidates", "memory_formation_jobs", "memory_formation_audit"} {
		assertRowCount(t, db, table, 0)
	}
}

func TestMemoryFormationMigrationIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	insertFormationUser(t, db, "user-a")
	candidateID := insertFormationCandidate(t, db, "user-a", "candidate-a", "proposed")

	if err := db.migrateMemoryFormationSchema(); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if err := db.initializeUserMemory(); err != nil {
		t.Fatalf("repeat user memory initialization: %v", err)
	}

	var migrationCount, candidateCount int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, memoryFormationMigration).Scan(&migrationCount); err != nil {
		t.Fatalf("count migration: %v", err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE id = ?`, candidateID).Scan(&candidateCount); err != nil {
		t.Fatalf("count candidate: %v", err)
	}
	if migrationCount != 1 || candidateCount != 1 {
		t.Fatalf("migration count=%d candidate count=%d", migrationCount, candidateCount)
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
