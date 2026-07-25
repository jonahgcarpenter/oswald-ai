package database

import "testing"

const formationTestTime = "2026-07-18T12:00:00Z"

func TestNormalizedUserMemorySchema(t *testing.T) {
	db := openTestDB(t)
	for _, object := range []struct{ kind, name string }{
		{"table", "memory_candidates"}, {"table", "memory_entries"}, {"table", "memory_events"}, {"table", "durable_jobs"},
		{"index", "idx_memory_candidates_publication"}, {"index", "idx_memory_candidates_published"}, {"index", "idx_memory_candidates_claim_identity"},
		{"index", "idx_memory_entries_active_serving"}, {"index", "idx_memory_entries_claim_identity"}, {"index", "idx_memory_events_audit_key"},
		{"trigger", "memory_events_formation_identity_immutable"}, {"trigger", "memory_events_formation_no_delete"},
	} {
		assertSchemaObject(t, db, object.kind, object.name)
	}
	for _, removed := range []struct{ kind, name string }{
		{"table", "memory_evidence"}, {"view", "memory_formation_audit"},
	} {
		assertNoSchemaObject(t, db, removed.kind, removed.name)
	}
	for _, column := range []string{"evidence", "source_turn_id", "published_memory_id", "publication_status", "redacted_at", "redaction_reason", "updated_at"} {
		assertTableColumn(t, db, "memory_candidates", column)
	}
	for _, column := range []string{"status_changed_at", "status_reason", "lifecycle_request_id", "hard_delete_after"} {
		assertTableColumn(t, db, "memory_entries", column)
	}
	for _, removed := range []struct{ table, column string }{
		{"memory_candidates", "evidence_summary"}, {"memory_candidates", "policy_decision"},
		{"memory_candidates", "lifecycle_state"}, {"memory_candidates", "lifecycle_reason"},
		{"memory_candidates", "source_session_id"}, {"memory_candidates", "formation_eligible_at"},
		{"memory_entries", "evidence"}, {"memory_entries", "candidate_id"},
		{"memory_entries", "approval_state"}, {"memory_entries", "profile_approved"},
		{"memory_candidates", "statement_key"}, {"memory_candidates", "claim_key"}, {"memory_candidates", "source_authority"},
		{"memory_entries", "statement_key"}, {"memory_entries", "claim_key"}, {"memory_entries", "source_authority"},
		{"session_turns", "formation_eligible_at"}, {"session_turns", "topic_tags"},
		{"durable_jobs", "job_type"}, {"durable_jobs", "last_error_message"},
		{"memory_events", "updated_at"}, {"memory_events", "content_expires_at"},
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

	if _, err := db.SQL().Exec(candidateInsertSQL, "user-a", "approved-none", "approved", "none", "Candidate", "candidate", "evidence", turnA, nil); err != nil {
		t.Fatalf("insert approved candidate: %v", err)
	}
	if _, err := db.SQL().Exec(candidateInsertSQL, "user-a", "published-without-link", "approved", "published", "Candidate", "candidate-2", "evidence", turnA, nil); err == nil {
		t.Fatal("published lifecycle accepted without published memory")
	}
	if _, err := db.SQL().Exec(candidateInsertSQL, "user-a", "link-without-published", "approved", "none", "Candidate", "candidate-3", "evidence", turnA, memoryA); err == nil {
		t.Fatal("published memory link accepted without published lifecycle")
	}
	if _, err := db.SQL().Exec(candidateInsertSQL, "user-a", "published", "approved", "published", "Candidate", "candidate-4", "evidence", turnA, memoryA); err != nil {
		t.Fatalf("insert published candidate: %v", err)
	}
	if _, err := db.SQL().Exec(candidateInsertSQL, "user-b", "cross-turn", "approved", "none", "Candidate", "candidate-5", "evidence", turnA, nil); err == nil {
		t.Fatal("cross-tenant candidate source turn accepted")
	}
	if _, err := db.SQL().Exec(candidateInsertSQL, "user-b", "cross-memory", "approved", "published", "Candidate", "candidate-6", "evidence", nil, memoryA); err == nil {
		t.Fatal("cross-tenant candidate publication accepted")
	}
	if _, err := db.SQL().Exec(candidateInsertSQL, "user-a", "redacted", "rejected", "none", "", "redacted:1", "", nil, nil); err != nil {
		t.Fatalf("content-free candidate tombstone rejected: %v", err)
	}
}

func TestFormationAuditIsAppendOnlyForActiveTenant(t *testing.T) {
	db := openTestDB(t)
	insertFormationUser(t, db, "user-a")
	if _, err := db.SQL().Exec(`INSERT INTO memory_events(canonical_user_id,event_kind,idempotency_key,event_type,created_at,metadata) VALUES ('user-a','formation_audit','audit','candidate.approved',?,'content')`, formationTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`UPDATE memory_events SET event_type = 'changed' WHERE idempotency_key = 'audit'`); err == nil {
		t.Fatal("formation audit identity update succeeded")
	}
	if _, err := db.SQL().Exec(`DELETE FROM memory_events WHERE idempotency_key = 'audit'`); err == nil {
		t.Fatal("content-bearing formation audit deletion succeeded")
	}
	if _, err := db.SQL().Exec(`UPDATE memory_events SET metadata = '', request_id = '', session_id = '' WHERE idempotency_key = 'audit'`); err != nil {
		t.Fatalf("formation audit redaction failed: %v", err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM memory_events WHERE idempotency_key = 'audit'`); err != nil {
		t.Fatalf("content-free formation audit tombstone deletion failed: %v", err)
	}
}

func insertFormationUser(t *testing.T, db *DB, userID string) {
	t.Helper()
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id,created_at,updated_at) VALUES (?,?,?)`, userID, formationTestTime, formationTestTime); err != nil {
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
	result, err := db.SQL().Exec(`INSERT INTO memory_entries(canonical_user_id,scope,category,statement,confidence,importance,status,created_at,updated_at,status_changed_at,claim_slot,claim_value) VALUES (?,'long_term','notes',?,0.9,3,'active',?,?,?,'notes.fact',?)`, userID, statement, formationTestTime, formationTestTime, formationTestTime, statement)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return id
}

const candidateInsertSQL = `INSERT INTO memory_candidates(canonical_user_id,idempotency_key,state,publication_status,scope,category,statement,claim_value,evidence,source_turn_id,formation_mode,provenance_type,sensitivity,extractor_version,published_memory_id,claim_slot,created_at,updated_at) VALUES (?,?,?,?,'long_term','notes',?,?,?,?,'automatic_extraction','user_statement','low','test-v1',?,'notes.fact','2026-07-18T12:00:00Z','2026-07-18T12:00:00Z')`

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
