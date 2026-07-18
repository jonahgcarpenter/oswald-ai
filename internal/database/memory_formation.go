package database

import (
	"database/sql"
	"fmt"
)

const memoryFormationMigration = "canonical_memory_formation_v1"

func (d *DB) migrateMemoryFormationSchema() error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin memory formation migration: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	var applied int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, memoryFormationMigration).Scan(&applied); err != nil {
		return fmt.Errorf("inspect memory formation migration: %w", err)
	}
	if applied != 0 {
		return tx.Commit()
	}

	columns := []struct {
		name       string
		definition string
	}{
		{name: "candidate_id", definition: "INTEGER REFERENCES memory_candidates(id) ON DELETE SET NULL"},
		{name: "provenance_type", definition: "TEXT NOT NULL DEFAULT 'legacy_import'"},
		{name: "source_authority", definition: "TEXT NOT NULL DEFAULT 'unknown'"},
		{name: "source_request_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "source_turn_id", definition: "INTEGER REFERENCES session_turns(id) ON DELETE SET NULL"},
		{name: "formation_mode", definition: "TEXT NOT NULL DEFAULT 'legacy_import'"},
		{name: "sensitivity", definition: "TEXT NOT NULL DEFAULT 'unknown'"},
		{name: "approval_state", definition: "TEXT NOT NULL DEFAULT 'approved' CHECK (approval_state IN ('proposed', 'pending_confirmation', 'approved', 'rejected'))"},
		{name: "approved_at", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "approved_by", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "valid_from", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "valid_until", definition: "TEXT"},
		{name: "invalidated_at", definition: "TEXT"},
		{name: "invalidation_reason", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "erased_at", definition: "TEXT"},
		{name: "erasure_reason", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "erasure_request_id", definition: "TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		if err := ensureColumnTx(tx, "memory_entries", column.name, column.definition); err != nil {
			return err
		}
	}
	if err := ensureColumnTx(tx, "session_turns", "source_request_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumnTx(tx, "session_turns", "formation_eligible_at", "TEXT"); err != nil {
		return err
	}

	if _, err := tx.Exec(`
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_entries_tenant_id
ON memory_entries (canonical_user_id, id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_session_turns_tenant_id
ON session_turns (canonical_user_id, id);

CREATE TABLE IF NOT EXISTS memory_candidates (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'proposed' CHECK (state IN ('proposed', 'pending_confirmation', 'approved', 'rejected')),
	scope TEXT NOT NULL CHECK (scope IN ('short_term', 'long_term')),
	category TEXT NOT NULL CHECK (category IN ('identity', 'communication_preferences', 'durable_preferences', 'projects', 'relationships', 'environment', 'notes')),
	statement TEXT NOT NULL,
	statement_key TEXT NOT NULL,
	evidence_summary TEXT NOT NULL DEFAULT '',
	confidence REAL NOT NULL DEFAULT 0.8 CHECK (confidence >= 0 AND confidence <= 1),
	importance INTEGER NOT NULL DEFAULT 3 CHECK (importance BETWEEN 1 AND 5),
	provenance_type TEXT NOT NULL,
	source_authority TEXT NOT NULL DEFAULT 'unknown',
	source_request_id TEXT NOT NULL DEFAULT '',
		source_session_id TEXT NOT NULL DEFAULT '',
		source_session_generation INTEGER NOT NULL DEFAULT 0,
		source_turn_id INTEGER,
		extraction_model TEXT NOT NULL DEFAULT '',
		extractor_version TEXT NOT NULL DEFAULT '',
		explicit_tool_source TEXT NOT NULL DEFAULT '',
		formation_mode TEXT NOT NULL,
		sensitivity TEXT NOT NULL DEFAULT 'unknown',
		content_context TEXT NOT NULL DEFAULT 'direct_assertion',
		policy_decision TEXT NOT NULL DEFAULT 'proposed',
		supersedes_memory_id INTEGER,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	expires_at TEXT,
	decided_at TEXT,
	decided_by TEXT NOT NULL DEFAULT '',
	decision_reason TEXT NOT NULL DEFAULT '',
	confirmation_session_id TEXT NOT NULL DEFAULT '',
	confirmation_request_id TEXT NOT NULL DEFAULT '',
	confirmation_presented_at TEXT,
	formation_eligible_at TEXT,
	published_memory_id INTEGER,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (source_turn_id) REFERENCES session_turns(id) ON DELETE SET NULL,
		FOREIGN KEY (published_memory_id) REFERENCES memory_entries(id) ON DELETE SET NULL,
	FOREIGN KEY (canonical_user_id, supersedes_memory_id) REFERENCES memory_entries(canonical_user_id, id),
	UNIQUE (canonical_user_id, idempotency_key),
	UNIQUE (canonical_user_id, id)
);

CREATE INDEX IF NOT EXISTS idx_memory_candidates_state
ON memory_candidates (canonical_user_id, state, created_at);

CREATE INDEX IF NOT EXISTS idx_memory_candidates_statement
ON memory_candidates (canonical_user_id, scope, statement_key);

CREATE INDEX IF NOT EXISTS idx_memory_candidates_source_turn
ON memory_candidates (canonical_user_id, source_turn_id);

CREATE TABLE IF NOT EXISTS memory_confirmation_presentations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	candidate_id INTEGER NOT NULL,
	session_id TEXT NOT NULL,
	session_generation INTEGER NOT NULL,
	request_id TEXT NOT NULL,
	delivered_at TEXT NOT NULL,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (canonical_user_id, candidate_id) REFERENCES memory_candidates(canonical_user_id, id) ON DELETE CASCADE ON UPDATE CASCADE,
	UNIQUE (canonical_user_id, candidate_id, session_id, session_generation)
);

CREATE INDEX IF NOT EXISTS idx_memory_confirmation_presentations_session
ON memory_confirmation_presentations (canonical_user_id, session_id, session_generation, delivered_at);

CREATE TABLE IF NOT EXISTS memory_evidence (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	candidate_id INTEGER,
	memory_id INTEGER,
	idempotency_key TEXT NOT NULL,
	evidence_type TEXT NOT NULL,
	content TEXT NOT NULL,
	source_authority TEXT NOT NULL DEFAULT 'unknown',
	source_request_id TEXT NOT NULL DEFAULT '',
	source_session_id TEXT NOT NULL DEFAULT '',
	source_turn_id INTEGER,
	created_at TEXT NOT NULL,
	CHECK ((candidate_id IS NULL) != (memory_id IS NULL)),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (canonical_user_id, candidate_id) REFERENCES memory_candidates(canonical_user_id, id) ON DELETE CASCADE ON UPDATE CASCADE,
	FOREIGN KEY (canonical_user_id, memory_id) REFERENCES memory_entries(canonical_user_id, id) ON DELETE CASCADE ON UPDATE CASCADE,
	FOREIGN KEY (source_turn_id) REFERENCES session_turns(id) ON DELETE SET NULL,
	UNIQUE (canonical_user_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_memory_evidence_candidate
ON memory_evidence (canonical_user_id, candidate_id);

CREATE INDEX IF NOT EXISTS idx_memory_evidence_memory
ON memory_evidence (canonical_user_id, memory_id);

CREATE TABLE IF NOT EXISTS memory_relations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	relation_type TEXT NOT NULL CHECK (relation_type IN ('duplicate', 'contradicts', 'supersedes', 'derived_from')),
	source_candidate_id INTEGER,
	source_memory_id INTEGER,
	target_candidate_id INTEGER,
	target_memory_id INTEGER,
	created_at TEXT NOT NULL,
	metadata TEXT NOT NULL DEFAULT '',
	CHECK ((source_candidate_id IS NULL) != (source_memory_id IS NULL)),
	CHECK ((target_candidate_id IS NULL) != (target_memory_id IS NULL)),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (canonical_user_id, source_candidate_id) REFERENCES memory_candidates(canonical_user_id, id) ON DELETE CASCADE ON UPDATE CASCADE,
	FOREIGN KEY (canonical_user_id, source_memory_id) REFERENCES memory_entries(canonical_user_id, id) ON DELETE CASCADE ON UPDATE CASCADE,
	FOREIGN KEY (canonical_user_id, target_candidate_id) REFERENCES memory_candidates(canonical_user_id, id) ON DELETE CASCADE ON UPDATE CASCADE,
	FOREIGN KEY (canonical_user_id, target_memory_id) REFERENCES memory_entries(canonical_user_id, id) ON DELETE CASCADE ON UPDATE CASCADE,
	UNIQUE (canonical_user_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_memory_relations_source_candidate
ON memory_relations (canonical_user_id, source_candidate_id);

CREATE INDEX IF NOT EXISTS idx_memory_relations_source_memory
ON memory_relations (canonical_user_id, source_memory_id);

CREATE INDEX IF NOT EXISTS idx_memory_relations_target_candidate
ON memory_relations (canonical_user_id, target_candidate_id);

CREATE INDEX IF NOT EXISTS idx_memory_relations_target_memory
ON memory_relations (canonical_user_id, target_memory_id);

CREATE TABLE IF NOT EXISTS memory_formation_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	job_type TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'retry', 'succeeded', 'skipped', 'dead')),
	source_request_id TEXT NOT NULL DEFAULT '',
		source_session_id TEXT NOT NULL DEFAULT '',
		source_session_generation INTEGER NOT NULL DEFAULT 0,
		source_turn_id INTEGER,
		extraction_model TEXT NOT NULL DEFAULT '',
	extractor_version TEXT NOT NULL DEFAULT '',
	extraction_payload TEXT NOT NULL DEFAULT '',
	attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
	redrive_count INTEGER NOT NULL DEFAULT 0 CHECK (redrive_count >= 0),
	available_at TEXT NOT NULL,
		started_at TEXT,
		lease_until TEXT,
	completed_at TEXT,
	last_error_code TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (source_turn_id) REFERENCES session_turns(id) ON DELETE SET NULL,
	UNIQUE (canonical_user_id, idempotency_key),
	UNIQUE (canonical_user_id, id)
);

CREATE INDEX IF NOT EXISTS idx_memory_formation_jobs_ready
ON memory_formation_jobs (state, available_at, id);

CREATE INDEX IF NOT EXISTS idx_memory_formation_jobs_tenant_state
ON memory_formation_jobs (canonical_user_id, state, created_at);

CREATE TABLE IF NOT EXISTS memory_formation_audit (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	event_type TEXT NOT NULL,
	candidate_id INTEGER,
	memory_id INTEGER,
	job_id INTEGER,
	request_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	turn_id INTEGER,
	actor_type TEXT NOT NULL,
	actor_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	metadata TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	UNIQUE (canonical_user_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_memory_formation_audit_tenant_time
ON memory_formation_audit (canonical_user_id, created_at, id);

CREATE TRIGGER IF NOT EXISTS memory_formation_audit_no_update
BEFORE UPDATE ON memory_formation_audit
WHEN NEW.canonical_user_id != OLD.canonical_user_id
	OR NEW.id != OLD.id
	OR NEW.idempotency_key != OLD.idempotency_key
	OR NEW.event_type != OLD.event_type
	OR COALESCE(NEW.candidate_id, 0) != COALESCE(OLD.candidate_id, 0)
	OR COALESCE(NEW.memory_id, 0) != COALESCE(OLD.memory_id, 0)
	OR COALESCE(NEW.job_id, 0) != COALESCE(OLD.job_id, 0)
	OR NEW.request_id != OLD.request_id
	OR NEW.session_id != OLD.session_id
	OR COALESCE(NEW.turn_id, 0) != COALESCE(OLD.turn_id, 0)
	OR NEW.actor_type != OLD.actor_type
	OR NEW.actor_id != OLD.actor_id
	OR NEW.created_at != OLD.created_at
	OR NEW.metadata != OLD.metadata
BEGIN
	SELECT RAISE(ABORT, 'memory formation audit is append-only');
END;

CREATE TRIGGER IF NOT EXISTS memory_formation_audit_no_delete
BEFORE DELETE ON memory_formation_audit
WHEN EXISTS (
	SELECT 1 FROM account_users
	WHERE canonical_user_id = OLD.canonical_user_id
)
BEGIN
	SELECT RAISE(ABORT, 'memory formation audit is append-only');
END;

CREATE TRIGGER IF NOT EXISTS memory_formation_audit_tenant_insert
BEFORE INSERT ON memory_formation_audit
WHEN (NEW.candidate_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_candidates WHERE id = NEW.candidate_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.memory_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_entries WHERE id = NEW.memory_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.job_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_formation_jobs WHERE id = NEW.job_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.turn_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM session_turns WHERE id = NEW.turn_id AND canonical_user_id = NEW.canonical_user_id))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory formation audit reference');
END;

CREATE TRIGGER IF NOT EXISTS memory_formation_audit_tenant_update
BEFORE UPDATE OF canonical_user_id, candidate_id, memory_id, job_id, turn_id ON memory_formation_audit
WHEN (NEW.candidate_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_candidates WHERE id = NEW.candidate_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.memory_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_entries WHERE id = NEW.memory_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.job_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_formation_jobs WHERE id = NEW.job_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.turn_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM session_turns WHERE id = NEW.turn_id AND canonical_user_id = NEW.canonical_user_id))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory formation audit reference');
END;

CREATE TRIGGER IF NOT EXISTS memory_candidates_tenant_insert
BEFORE INSERT ON memory_candidates
WHEN (NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.published_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries
	WHERE id = NEW.published_memory_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.supersedes_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries
	WHERE id = NEW.supersedes_memory_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory candidate reference');
END;

CREATE TRIGGER IF NOT EXISTS memory_candidates_tenant_update
BEFORE UPDATE OF canonical_user_id, source_turn_id, published_memory_id, supersedes_memory_id ON memory_candidates
WHEN (NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.published_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries
	WHERE id = NEW.published_memory_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.supersedes_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries
	WHERE id = NEW.supersedes_memory_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory candidate reference');
END;

CREATE TRIGGER IF NOT EXISTS memory_evidence_tenant_insert
BEFORE INSERT ON memory_evidence
WHEN NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
)
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory evidence source turn');
END;

CREATE TRIGGER IF NOT EXISTS memory_evidence_tenant_update
BEFORE UPDATE OF canonical_user_id, source_turn_id ON memory_evidence
WHEN NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
)
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory evidence source turn');
END;

CREATE TRIGGER IF NOT EXISTS memory_formation_jobs_tenant_insert
BEFORE INSERT ON memory_formation_jobs
WHEN NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
)
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory formation job source turn');
END;

CREATE TRIGGER IF NOT EXISTS memory_formation_jobs_tenant_update
BEFORE UPDATE OF canonical_user_id, source_turn_id ON memory_formation_jobs
WHEN NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
)
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory formation job source turn');
END;

CREATE TRIGGER IF NOT EXISTS memory_entries_formation_tenant_insert
BEFORE INSERT ON memory_entries
WHEN (NEW.candidate_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_candidates
	WHERE id = NEW.candidate_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant canonical memory reference');
END;

CREATE TRIGGER IF NOT EXISTS memory_entries_formation_tenant_update
BEFORE UPDATE OF canonical_user_id, candidate_id, source_turn_id ON memory_entries
WHEN (NEW.candidate_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_candidates
	WHERE id = NEW.candidate_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant canonical memory reference');
END;

CREATE INDEX IF NOT EXISTS idx_memory_entries_candidate
ON memory_entries (canonical_user_id, candidate_id);

CREATE INDEX IF NOT EXISTS idx_memory_entries_source_request
ON memory_entries (canonical_user_id, source_request_id);

CREATE INDEX IF NOT EXISTS idx_memory_entries_source_turn
ON memory_entries (canonical_user_id, source_turn_id);
`); err != nil {
		return fmt.Errorf("create memory formation schema: %w", err)
	}

	if _, err := tx.Exec(`
UPDATE memory_entries
SET provenance_type = 'legacy_import',
	source_authority = 'unknown',
	formation_mode = 'legacy_import',
	sensitivity = 'unknown',
	approval_state = 'approved',
	approved_at = created_at,
	valid_from = created_at
`); err != nil {
		return fmt.Errorf("backfill legacy memory formation metadata: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`, memoryFormationMigration); err != nil {
		return fmt.Errorf("record memory formation migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit memory formation migration: %w", err)
	}
	return nil
}

func ensureColumnTx(tx *sql.Tx, table, name, definition string) error {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("scan %s columns: %w", table, err)
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close() // nolint:errcheck
		return fmt.Errorf("read %s columns: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close %s columns: %w", table, err)
	}
	if found {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, name, err)
	}
	return nil
}
