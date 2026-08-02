package database

import "testing"

const formationTestTime = "2026-07-18T12:00:00Z"

func TestNormalizedUserMemorySchema(t *testing.T) {
	db := openTestDB(t)
	for _, object := range []struct{ kind, name string }{
		{"table", "memory_candidates"}, {"table", "memory_entries"}, {"table", "durable_jobs"},
		{"index", "idx_memory_candidates_published"},
		{"index", "idx_memory_entries_active_serving"}, {"index", "idx_memory_entries_claim_identity"},
	} {
		assertSchemaObject(t, db, object.kind, object.name)
	}
	for _, removed := range []struct{ kind, name string }{
		{"table", "memory_evidence"},
	} {
		assertNoSchemaObject(t, db, removed.kind, removed.name)
	}
	for _, column := range []string{"evidence", "source_turn_id", "published_memory_id", "updated_at"} {
		assertTableColumn(t, db, "memory_candidates", column)
	}
	for _, removed := range []struct{ table, column string }{
		{"memory_candidates", "evidence_summary"}, {"memory_candidates", "policy_decision"},
		{"memory_candidates", "lifecycle_state"}, {"memory_candidates", "lifecycle_reason"},
		{"memory_candidates", "source_session_id"}, {"memory_candidates", "formation_eligible_at"},
		{"memory_entries", "evidence"}, {"memory_entries", "candidate_id"},
		{"memory_entries", "approval_state"}, {"memory_entries", "profile_approved"},
		{"memory_candidates", "statement_key"}, {"memory_candidates", "claim_key"}, {"memory_candidates", "source_authority"},
		{"memory_entries", "statement_key"}, {"memory_entries", "claim_key"}, {"memory_entries", "source_authority"},
		{"memory_candidates", "publication_status"}, {"memory_entries", "last_used_at"},
		{"memory_entries", "status_changed_at"}, {"memory_entries", "status_reason"},
		{"session_turns", "formation_eligible_at"}, {"session_turns", "topic_tags"},
		{"durable_jobs", "job_type"}, {"durable_jobs", "last_error_message"},
		{"memory_candidates", "redacted_at"}, {"memory_candidates", "redaction_reason"},
		{"memory_entries", "lifecycle_request_id"}, {"memory_entries", "hard_delete_after"},
		{"session_turns", "privacy_suppressed_at"},
	} {
		assertNoTableColumn(t, db, removed.table, removed.column)
	}
}

func TestCandidatePublicationConsistencyAndTenantFences(t *testing.T) {
	db := openTestDB(t)
	insertFormationUser(t, db, "user-a")
	insertFormationUser(t, db, "user-b")
	turnA := insertFormationTurn(t, db, "user-a", "session-a")
	memoryA := insertFormationMemory(t, db, "user-a", "Memory A")

	if _, err := db.SQL().Exec(candidateInsertSQL, "user-a", "approved-none", "approved", "Candidate", "candidate", "evidence", turnA, nil); err != nil {
		t.Fatalf("insert approved candidate: %v", err)
	}
	if _, err := db.SQL().Exec(candidateInsertSQL, "user-a", "published", "approved", "Candidate", "candidate-4", "evidence", turnA, memoryA); err != nil {
		t.Fatalf("insert published candidate: %v", err)
	}
	if _, err := db.SQL().Exec(candidateInsertSQL, "user-a", "rejected-link", "rejected", "Candidate", "candidate-3", "evidence", turnA, memoryA); err == nil {
		t.Fatal("rejected candidate accepted with published memory")
	}
	if _, err := db.SQL().Exec(candidateInsertSQL, "user-b", "cross-turn", "approved", "Candidate", "candidate-5", "evidence", turnA, nil); err == nil {
		t.Fatal("cross-tenant candidate source turn accepted")
	}
	if _, err := db.SQL().Exec(candidateInsertSQL, "user-b", "cross-memory", "approved", "Candidate", "candidate-6", "evidence", nil, memoryA); err == nil {
		t.Fatal("cross-tenant candidate publication accepted")
	}
}

func insertFormationUser(t *testing.T, db *DB, userID string) {
	t.Helper()
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id) VALUES (?)`, userID); err != nil {
		t.Fatal(err)
	}
}

func insertFormationTurn(t *testing.T, db *DB, userID, sessionID string) int64 {
	t.Helper()
	result, err := db.SQL().Exec(`INSERT INTO session_turns(session_id,canonical_user_id,user_text,assistant_text,created_at,session_generation,delivered_at) VALUES (?,?,'user','assistant',?,1,?)`, sessionID, userID, formationTestTime, formationTestTime)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return id
}

func insertFormationMemory(t *testing.T, db *DB, userID, statement string) int64 {
	t.Helper()
	result, err := db.SQL().Exec(`INSERT INTO memory_entries(canonical_user_id,scope,category,statement,confidence,importance,status,created_at,updated_at,claim_slot,claim_value) VALUES (?,'long_term','notes',?,0.9,3,'active',?,?,'notes.fact',?)`, userID, statement, formationTestTime, formationTestTime, statement)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return id
}

const candidateInsertSQL = `INSERT INTO memory_candidates(canonical_user_id,idempotency_key,state,scope,category,statement,claim_value,evidence,source_turn_id,formation_mode,provenance_type,sensitivity,extractor_version,published_memory_id,claim_slot,created_at,updated_at) VALUES (?,?,?,'long_term','notes',?,?,?,?,'automatic_extraction','user_statement','low','test-v1',?,'notes.fact','2026-07-18T12:00:00Z','2026-07-18T12:00:00Z')`

func assertSchemaObject(t *testing.T, db *DB, objectType, name string) {
	t.Helper()
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, objectType, name).Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing %s %s: count=%d err=%v", objectType, name, count, err)
	}
}

func assertNoSchemaObject(t *testing.T, db *DB, objectType, name string) {
	t.Helper()
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, objectType, name).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected %s %s: count=%d err=%v", objectType, name, count, err)
	}
}

func assertTableColumn(t *testing.T, db *DB, table, column string) {
	t.Helper()
	if !tableHasColumn(t, db, table, column) {
		t.Fatalf("missing %s.%s", table, column)
	}
}

func assertNoTableColumn(t *testing.T, db *DB, table, column string) {
	t.Helper()
	if tableHasColumn(t, db, table, column) {
		t.Fatalf("unexpected %s.%s", table, column)
	}
}

func tableHasColumn(t *testing.T, db *DB, table, column string) bool {
	t.Helper()
	rows, err := db.SQL().Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}
