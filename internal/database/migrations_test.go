package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
)

func TestCompactV4BaselineDefinitionIsExecutedDirectly(t *testing.T) {
	for _, forbidden := range []string{"ALTER TABLE", "DROP TABLE", "DROP TRIGGER", "data-transform:", "legacy-ledger:"} {
		if strings.Contains(compactV4BaselineDefinition, forbidden) {
			t.Fatalf("direct baseline contains forbidden historical operation %q", forbidden)
		}
	}

	direct, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	if _, err := direct.Exec(compactV4BaselineDefinition); err != nil {
		t.Fatalf("execute baseline definition: %v", err)
	}

	applied, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer applied.Close()
	conn, err := applied.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := applyCompactV4Baseline(context.Background(), conn); err != nil {
		conn.Close() // nolint:errcheck
		t.Fatalf("apply compact baseline: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	directSchema := schemaSnapshot(t, direct)
	appliedSchema := schemaSnapshot(t, applied)
	if directSchema != appliedSchema {
		t.Fatal("executed compact baseline differs from its checksum definition")
	}
}

func TestCompactV4BaselineIsFreshAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	var name, checksum string
	if err := db.SQL().QueryRow(`SELECT version, name, checksum FROM schema_migration_versions`).Scan(&version, &name, &checksum); err != nil {
		t.Fatal(err)
	}
	if version != 1 || name != "v4_compact_baseline" || len(checksum) != 64 {
		t.Fatalf("unexpected baseline ledger: %d %q %q", version, name, checksum)
	}
	for _, removed := range []string{"schema_migrations", "memory_confirmation_presentations", "memory_relations", "maintenance_runs", "tenant_profile_versions", "tenant_profile_version_facts", "tenant_profile_version_counters", "tenant_sessions", "tenant_session_generations", "deployment_memory_candidates", "deployment_memory_entries", "deployment_memory_evidence", "global_memory_claims", "global_memory_evidence"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, removed).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("obsolete table %s still exists", removed)
		}
	}
	toolAnnotations := strings.Join([]string{
		toolnames.UserMemorySearch,
		toolnames.UserMemoryList,
		toolnames.GlobalMemorySearch,
		toolnames.SessionTranscriptSearch,
	}, ",")
	if _, err := db.SQL().Exec(`
INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES ('restart-user', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z');
INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, tool_names, created_at)
VALUES ('restart-session', 'restart-user', 'remember this', 'saved', ?, '2026-07-21T00:00:00Z');
INSERT INTO memory_candidates(
	canonical_user_id, idempotency_key, state, scope, category, statement, claim_slot, claim_value,
	provenance_type, formation_mode, created_at, updated_at
) VALUES ('restart-user', 'restart-candidate', 'approved', 'long_term', 'notes', 'Persisted final tool name.',
	'notes.fact', 'persisted final tool name', 'user_statement', 'explicit_remember', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`, toolAnnotations); err != nil {
		t.Fatalf("persist final tool names: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen compact baseline: %v", err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("baseline reapplied: count=%d err=%v", count, err)
	}
	var persistedAnnotations string
	if err := reopened.SQL().QueryRow(`SELECT tool_names FROM session_turns WHERE canonical_user_id = 'restart-user'`).Scan(&persistedAnnotations); err != nil {
		t.Fatal(err)
	}
	if persistedAnnotations != toolAnnotations {
		t.Fatalf("persisted tool names changed across restart: annotations=%q", persistedAnnotations)
	}
}

func TestCompactV4CanonicalTableInventory(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expected := []string{
		"account_link_challenges", "account_users", "derived_index_revisions", "durable_jobs",
		"global_memories", "linked_accounts", "mcp_servers", "memory_candidates",
		"memory_entries", "memory_events", "schema_migration_versions", "session_summaries",
		"session_turns", "sessions", "websocket_bootstrap_state",
		"websocket_clients", "websocket_device_authorizations",
	}
	rows, err := db.SQL().Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT GLOB 'memory_entries_fts*' AND name NOT GLOB 'session_turns_fts*' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actual []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, name)
	}
	if strings.Join(actual, ",") != strings.Join(expected, ",") {
		t.Fatalf("canonical table inventory changed:\nactual: %v\nexpected: %v", actual, expected)
	}
}

func TestCompactV4CanonicalObjectInventory(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for objectType, want := range map[string]int{"table": 16, "index": 44, "trigger": 18, "view": 0} {
		var got int
		if err := db.SQL().QueryRow(`
SELECT COUNT(*) FROM sqlite_master
WHERE type = ? AND name NOT LIKE 'sqlite_%'
	AND name != 'schema_migration_versions'
	AND name NOT GLOB 'memory_entries_fts*'
	AND name NOT GLOB 'session_turns_fts*'`, objectType).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("canonical %s count=%d, want %d", objectType, got, want)
		}
	}
}

func TestCompactV4BaselineRejectsDevelopmentLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE schema_migration_versions (version INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL, applied_at TEXT NOT NULL); INSERT INTO schema_migration_versions VALUES (1, 'legacy_core_schema', ?, datetime('now'))`, strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(path, nil); err == nil {
		db.Close()
		t.Fatal("expected development migration ledger rejection")
	} else if !strings.Contains(err.Error(), "checksum drift") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestCompactV4BaselineRejectsEmptyLedgerWithOtherObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
CREATE TABLE schema_migration_versions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	applied_at TEXT NOT NULL
);
CREATE TABLE unrelated (id INTEGER PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	if db, err := Open(path, nil); err == nil {
		db.Close()
		t.Fatal("expected empty migration ledger with other objects to be rejected")
	} else if !strings.Contains(err.Error(), "invalid frozen v4 schema migration ledger") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestSchemaMigrationApplyFailureRollsBackEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := &DB{path: path, db: raw}
	registry := []schemaMigration{{
		version:    1,
		name:       "failing_baseline",
		definition: "CREATE TABLE rollback_probe (id INTEGER PRIMARY KEY); invalid statement;",
		apply: func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, "CREATE TABLE rollback_probe (id INTEGER PRIMARY KEY); invalid statement;")
			return err
		},
	}}
	if err := db.runSchemaMigrations(context.Background(), registry); err == nil {
		t.Fatal("expected schema migration failure")
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed baseline left %d schema objects", count)
	}
}

func TestCompactV4ConcurrentOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	const openCount = 8
	dbs := make([]*DB, openCount)
	errs := make([]error, openCount)
	var wg sync.WaitGroup
	for i := range dbs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dbs[i], errs[i] = Open(path, nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent open %d: %v", i, err)
		}
		defer dbs[i].Close()
	}
	var count int
	if err := dbs[0].SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migration ledger row count=%d, want 1", count)
	}
}

func TestCompactV4BaselineForeignKeysAreValid(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.SQL().Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("fresh compact baseline has a foreign-key violation")
	}
}

func TestGlobalMemoryCanonicalConstraints(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SQL().Exec(`INSERT INTO global_memories(memory, memory_key, created_at) VALUES ('', 'empty', '2026-07-21T00:00:00Z')`); err == nil {
		t.Fatal("expected empty global memory to fail")
	}
	if _, err := db.SQL().Exec(`INSERT INTO global_memories(memory, memory_key, created_at) VALUES (?, 'long', '2026-07-21T00:00:00Z')`, strings.Repeat("x", 1001)); err == nil {
		t.Fatal("expected overlong global memory to fail")
	}
	result, err := db.SQL().Exec(`INSERT INTO global_memories(memory, memory_key, created_at) VALUES ('Oswald uses Go.', 'oswald uses go.', '2026-07-21T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert valid global memory: %v", err)
	}
	memoryID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO global_memories(memory, memory_key, created_at) VALUES ('Duplicate key.', 'oswald uses go.', '2026-07-21T00:00:00Z')`); err == nil {
		t.Fatal("expected duplicate normalized key to fail")
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, created_at, updated_at) VALUES ('derived_index', 'global-upsert', NULL, 'global_memory', ?, 'upsert', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`, memoryID); err != nil {
		t.Fatalf("insert global-memory index job: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, created_at, updated_at) VALUES ('derived_index', 'bad-global-upsert', 'missing-user', 'global_memory', ?, 'upsert', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`, memoryID); err == nil {
		t.Fatal("expected tenant-owned global-memory index job to fail")
	}
}

func TestDurableJobRunningLeaseConstraints(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES ('user', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, state, source_session_generation, extractor_version, available_at, created_at, updated_at) VALUES ('memory_formation', 'formation', 'user', 'running', 1, 'v1', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`,
		`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, state, entity_kind, entity_id, operation, available_at, created_at, updated_at) VALUES ('derived_index', 'index', 'user', 'running', 'memory', 1, 'upsert', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`,
	} {
		if _, err := db.SQL().Exec(statement); err == nil {
			t.Fatalf("running durable job without lease was accepted: %s", statement)
		}
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, lease_owner, lease_until, available_at, created_at, updated_at) VALUES ('derived_index', 'queued-owned', 'user', 'memory', 1, 'upsert', 'owner', '2026-07-21T01:00:00Z', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`); err == nil {
		t.Fatal("non-running durable job with lease ownership was accepted")
	}
}

func TestIndexTableIdentityConstraints(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SQL().Exec(`INSERT INTO derived_index_revisions(index_kind, provider, schema_version, revision, table_name, state, created_at, updated_at) VALUES ('memory_fts', 'sqlite_fts5', 1, 1, 'unique_table', 'failed', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO derived_index_revisions(index_kind, provider, schema_version, revision, table_name, state, created_at, updated_at) VALUES ('transcript_fts', 'sqlite_fts5', 1, 1, 'unique_table', 'failed', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`); err == nil {
		t.Fatal("duplicate derived-index table identity was accepted")
	}
}

func schemaSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`
SELECT type, name, tbl_name, sql
FROM sqlite_master
WHERE name NOT LIKE 'sqlite_%'
ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot strings.Builder
	for rows.Next() {
		var objectType, name, table, definition string
		if err := rows.Scan(&objectType, &name, &table, &definition); err != nil {
			t.Fatal(err)
		}
		snapshot.WriteString(objectType)
		snapshot.WriteByte('|')
		snapshot.WriteString(name)
		snapshot.WriteByte('|')
		snapshot.WriteString(table)
		snapshot.WriteByte('|')
		snapshot.WriteString(definition)
		snapshot.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot.String()
}
