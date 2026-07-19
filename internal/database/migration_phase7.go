package database

import (
	"context"
	"database/sql"
	"fmt"
)

const (
	phase7AccountLifecycleColumnDefinition = "TEXT NOT NULL DEFAULT 'active' CHECK (lifecycle_state IN ('active', 'erasing'))"
	phase7AuditContentExpiryDefinition     = "TEXT"
	phase7AuditRedactedAtDefinition        = "TEXT"

	phase7EnsureAccountLifecycleOperation = "ensure-column-if-missing:ALTER TABLE account_users ADD COLUMN lifecycle_state " + phase7AccountLifecycleColumnDefinition + ";\n"
	phase7EnsureAuditContentExpiry        = "ensure-column-if-missing:ALTER TABLE memory_formation_audit ADD COLUMN content_expires_at " + phase7AuditContentExpiryDefinition + ";\n"
	phase7EnsureAuditRedactedAt           = "ensure-column-if-missing:ALTER TABLE memory_formation_audit ADD COLUMN redacted_at " + phase7AuditRedactedAtDefinition + ";\n"
)

const phase7MigrationDefinition = phase7EnsureAccountLifecycleOperation + phase7MemoryEntriesRebuildSQL + phase7MemoryEventsRebuildSQL + phase7EnsureAuditContentExpiry + phase7EnsureAuditRedactedAt + phase7FoundationSQL

func applyPhase7Migration(ctx context.Context, conn *sql.Conn) error {
	if err := ensureColumnConn(ctx, conn, "account_users", "lifecycle_state", phase7AccountLifecycleColumnDefinition); err != nil {
		return err
	}
	if err := rebuildMemoryEntriesPhase7(ctx, conn); err != nil {
		return err
	}
	if err := rebuildMemoryEventsPhase7(ctx, conn); err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "content_expires_at", definition: phase7AuditContentExpiryDefinition},
		{name: "redacted_at", definition: phase7AuditRedactedAtDefinition},
	} {
		if err := ensureColumnConn(ctx, conn, "memory_formation_audit", column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, phase7FoundationSQL); err != nil {
		return fmt.Errorf("create phase 7 operational schema: %w", err)
	}
	return nil
}

func rebuildMemoryEntriesPhase7(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, phase7MemoryEntriesRebuildSQL); err != nil {
		return fmt.Errorf("rebuild memory_entries for phase 7: %w", err)
	}
	return nil
}

const phase7MemoryEntriesRebuildSQL = `
DROP TRIGGER IF EXISTS memory_entries_fts_insert;
DROP TRIGGER IF EXISTS memory_entries_fts_delete;
DROP TRIGGER IF EXISTS memory_entries_fts_update;
DROP TRIGGER IF EXISTS memory_formation_audit_tenant_insert;
DROP TRIGGER IF EXISTS memory_formation_audit_tenant_update;
DROP TRIGGER IF EXISTS memory_candidates_tenant_insert;
DROP TRIGGER IF EXISTS memory_candidates_tenant_update;

CREATE TABLE memory_entries_phase7 (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	scope TEXT NOT NULL CHECK (scope IN ('short_term', 'long_term')),
	category TEXT NOT NULL CHECK (category IN ('identity', 'communication_preferences', 'durable_preferences', 'projects', 'relationships', 'environment', 'notes')),
	statement TEXT NOT NULL,
	statement_key TEXT NOT NULL,
	evidence TEXT NOT NULL,
	confidence REAL NOT NULL DEFAULT 0.8,
	importance INTEGER NOT NULL DEFAULT 3,
	status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'expired', 'superseded', 'deleted', 'forgotten')),
	source_session_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	last_used_at TEXT,
	expires_at TEXT,
	supersedes_id INTEGER,
	embedding_model TEXT NOT NULL DEFAULT '',
	embedding_dim INTEGER NOT NULL DEFAULT 0,
	profile_approved INTEGER NOT NULL DEFAULT 0 CHECK (profile_approved IN (0, 1)),
	candidate_id INTEGER,
	provenance_type TEXT NOT NULL DEFAULT 'legacy_import',
	source_authority TEXT NOT NULL DEFAULT 'unknown',
	source_request_id TEXT NOT NULL DEFAULT '',
	source_turn_id INTEGER,
	formation_mode TEXT NOT NULL DEFAULT 'legacy_import',
	sensitivity TEXT NOT NULL DEFAULT 'unknown',
	approval_state TEXT NOT NULL DEFAULT 'approved' CHECK (approval_state IN ('proposed', 'pending_confirmation', 'approved', 'rejected')),
	approved_at TEXT NOT NULL DEFAULT '',
	approved_by TEXT NOT NULL DEFAULT '',
	valid_from TEXT NOT NULL DEFAULT '',
	valid_until TEXT,
	invalidated_at TEXT,
	invalidation_reason TEXT NOT NULL DEFAULT '',
	erased_at TEXT,
	erasure_reason TEXT NOT NULL DEFAULT '',
	erasure_request_id TEXT NOT NULL DEFAULT '',
	forgotten_at TEXT,
	hard_delete_after TEXT,
	lifecycle_request_id TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (supersedes_id) REFERENCES memory_entries_phase7(id) ON DELETE SET NULL,
	FOREIGN KEY (candidate_id) REFERENCES memory_candidates(id) ON DELETE SET NULL,
	FOREIGN KEY (source_turn_id) REFERENCES session_turns(id) ON DELETE SET NULL,
	UNIQUE (canonical_user_id, scope, statement_key),
	UNIQUE (canonical_user_id, id)
);

INSERT INTO memory_entries_phase7 (
	id, canonical_user_id, scope, category, statement, statement_key, evidence,
	confidence, importance, status, source_session_id, created_at, updated_at,
	last_used_at, expires_at, supersedes_id, embedding_model, embedding_dim,
	profile_approved, candidate_id, provenance_type, source_authority,
	source_request_id, source_turn_id, formation_mode, sensitivity, approval_state,
	approved_at, approved_by, valid_from, valid_until, invalidated_at,
	invalidation_reason, erased_at, erasure_reason, erasure_request_id
)
SELECT id, canonical_user_id, scope, category, statement, statement_key, evidence,
	confidence, importance, status, source_session_id, created_at, updated_at,
	last_used_at, expires_at, supersedes_id, embedding_model, embedding_dim,
	profile_approved, candidate_id, provenance_type, source_authority,
	source_request_id, source_turn_id, formation_mode, sensitivity, approval_state,
	approved_at, approved_by, valid_from, valid_until, invalidated_at,
	invalidation_reason, erased_at, erasure_reason, erasure_request_id
FROM memory_entries;

DROP TABLE memory_entries;
ALTER TABLE memory_entries_phase7 RENAME TO memory_entries;

CREATE INDEX idx_memory_entries_user_scope_category
ON memory_entries (canonical_user_id, scope, category, status);
CREATE INDEX idx_memory_entries_user_updated
ON memory_entries (canonical_user_id, updated_at);
CREATE INDEX idx_memory_entries_expiry
ON memory_entries (expires_at, status);
CREATE INDEX idx_memory_entries_profile_candidates
ON memory_entries (canonical_user_id, profile_approved, status, scope, category, expires_at);
CREATE INDEX idx_memory_entries_candidate
ON memory_entries (canonical_user_id, candidate_id);
CREATE INDEX idx_memory_entries_source_request
ON memory_entries (canonical_user_id, source_request_id);
CREATE INDEX idx_memory_entries_source_turn
ON memory_entries (canonical_user_id, source_turn_id);
CREATE INDEX idx_memory_entries_hard_delete
ON memory_entries (hard_delete_after, id) WHERE hard_delete_after IS NOT NULL;
CREATE INDEX idx_memory_entries_lifecycle_request
ON memory_entries (canonical_user_id, lifecycle_request_id) WHERE lifecycle_request_id != '';

CREATE TRIGGER memory_entries_formation_tenant_insert
BEFORE INSERT ON memory_entries
WHEN (NEW.candidate_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_candidates WHERE id = NEW.candidate_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant canonical memory reference');
END;

CREATE TRIGGER memory_entries_formation_tenant_update
BEFORE UPDATE OF canonical_user_id, candidate_id, source_turn_id ON memory_entries
WHEN (NEW.candidate_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_candidates WHERE id = NEW.candidate_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant canonical memory reference');
END;

CREATE TRIGGER memory_formation_audit_tenant_insert
BEFORE INSERT ON memory_formation_audit
WHEN (NEW.candidate_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_candidates WHERE id = NEW.candidate_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.memory_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_entries WHERE id = NEW.memory_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.job_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_formation_jobs WHERE id = NEW.job_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.turn_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM session_turns WHERE id = NEW.turn_id AND canonical_user_id = NEW.canonical_user_id))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory formation audit reference');
END;

CREATE TRIGGER memory_formation_audit_tenant_update
BEFORE UPDATE OF canonical_user_id, candidate_id, memory_id, job_id, turn_id ON memory_formation_audit
WHEN (NEW.candidate_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_candidates WHERE id = NEW.candidate_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.memory_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_entries WHERE id = NEW.memory_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.job_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM memory_formation_jobs WHERE id = NEW.job_id AND canonical_user_id = NEW.canonical_user_id))
	OR (NEW.turn_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM session_turns WHERE id = NEW.turn_id AND canonical_user_id = NEW.canonical_user_id))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory formation audit reference');
END;

CREATE TRIGGER memory_candidates_tenant_insert
BEFORE INSERT ON memory_candidates
WHEN (NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.published_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.published_memory_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.supersedes_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.supersedes_memory_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory candidate reference');
END;

CREATE TRIGGER memory_candidates_tenant_update
BEFORE UPDATE OF canonical_user_id, source_turn_id, published_memory_id, supersedes_memory_id ON memory_candidates
WHEN (NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.published_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.published_memory_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.supersedes_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.supersedes_memory_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory candidate reference');
END;
`

func rebuildMemoryEventsPhase7(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, phase7MemoryEventsRebuildSQL); err != nil {
		return fmt.Errorf("rebuild memory_events for phase 7: %w", err)
	}
	return nil
}

const phase7MemoryEventsRebuildSQL = `
CREATE TABLE memory_events_phase7 (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	memory_id INTEGER,
	event_type TEXT NOT NULL,
	request_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	metadata TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (memory_id) REFERENCES memory_entries(id) ON DELETE SET NULL
);

INSERT INTO memory_events_phase7 (
	id, canonical_user_id, memory_id, event_type, request_id, session_id, created_at, metadata
)
SELECT event.id, memory.canonical_user_id, event.memory_id, event.event_type,
	event.request_id, event.session_id, event.created_at, event.metadata
FROM memory_events AS event
JOIN memory_entries AS memory ON memory.id = event.memory_id;

DROP TABLE memory_events;
ALTER TABLE memory_events_phase7 RENAME TO memory_events;

CREATE INDEX idx_memory_events_tenant_time
ON memory_events (canonical_user_id, created_at, id);
CREATE INDEX idx_memory_events_memory
ON memory_events (canonical_user_id, memory_id, created_at);

CREATE TRIGGER memory_events_tenant_insert
BEFORE INSERT ON memory_events
WHEN NEW.memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.memory_id AND canonical_user_id = NEW.canonical_user_id
)
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory event reference');
END;

CREATE TRIGGER memory_events_tenant_update
BEFORE UPDATE OF canonical_user_id, memory_id ON memory_events
WHEN NEW.memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.memory_id AND canonical_user_id = NEW.canonical_user_id
)
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory event reference');
END;
`

const phase7FoundationSQL = `
CREATE INDEX idx_account_users_lifecycle
ON account_users (lifecycle_state, updated_at, canonical_user_id);

CREATE INDEX idx_memory_formation_audit_content_expiry
ON memory_formation_audit (content_expires_at, id)
WHERE content_expires_at IS NOT NULL AND redacted_at IS NULL;

CREATE TABLE privacy_operations (
	operation_id TEXT PRIMARY KEY,
	idempotency_key TEXT NOT NULL,
	actor_hash TEXT NOT NULL CHECK (length(actor_hash) = 64),
	target_user_id TEXT,
	target_hash TEXT NOT NULL CHECK (length(target_hash) = 64),
	operation_type TEXT NOT NULL CHECK (operation_type IN ('forget_memory', 'delete_user', 'export_user')),
	target_digest TEXT NOT NULL CHECK (length(target_digest) = 64),
	challenge_hash TEXT NOT NULL DEFAULT '',
	challenge_expires_at TEXT,
	status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'completed', 'failed', 'expired')),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	started_at TEXT,
	completed_at TEXT,
	last_error_code TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (target_user_id) REFERENCES account_users(canonical_user_id) ON DELETE SET NULL,
	UNIQUE (actor_hash, idempotency_key),
	CHECK ((challenge_hash = '') = (challenge_expires_at IS NULL)),
	CHECK (status NOT IN ('pending', 'running') OR target_user_id IS NOT NULL)
);

CREATE INDEX idx_privacy_operations_status
ON privacy_operations (status, updated_at, operation_id);
CREATE INDEX idx_privacy_operations_target
ON privacy_operations (target_user_id, created_at) WHERE target_user_id IS NOT NULL;

CREATE TRIGGER privacy_operations_no_delete
BEFORE DELETE ON privacy_operations
BEGIN
	SELECT RAISE(ABORT, 'privacy operation records must be retained');
END;

CREATE TRIGGER privacy_operations_identity_immutable
BEFORE UPDATE ON privacy_operations
WHEN NEW.operation_id != OLD.operation_id
	OR NEW.idempotency_key != OLD.idempotency_key
	OR NEW.actor_hash != OLD.actor_hash
	OR NEW.target_hash != OLD.target_hash
	OR NEW.operation_type != OLD.operation_type
	OR NEW.target_digest != OLD.target_digest
	OR NEW.created_at != OLD.created_at
BEGIN
	SELECT RAISE(ABORT, 'privacy operation identity is immutable');
END;

CREATE TABLE derived_index_revisions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	index_kind TEXT NOT NULL CHECK (index_kind IN ('memory_fts', 'transcript_fts', 'memory_vector')),
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	dimension INTEGER NOT NULL DEFAULT 0 CHECK (dimension >= 0),
	schema_version INTEGER NOT NULL CHECK (schema_version > 0),
	revision INTEGER NOT NULL CHECK (revision > 0),
	table_name TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('building', 'live', 'degraded', 'failed', 'retired')),
	expected_count INTEGER NOT NULL DEFAULT 0 CHECK (expected_count >= 0),
	indexed_count INTEGER NOT NULL DEFAULT 0 CHECK (indexed_count >= 0),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	build_started_at TEXT,
	last_successful_rebuild_at TEXT,
	published_at TEXT,
	completed_at TEXT,
	last_error_code TEXT NOT NULL DEFAULT '',
	UNIQUE (index_kind, revision),
	CHECK (indexed_count <= expected_count OR state = 'degraded')
);

CREATE UNIQUE INDEX idx_derived_index_revisions_live_kind
ON derived_index_revisions (index_kind) WHERE state = 'live';
CREATE INDEX idx_derived_index_revisions_state
ON derived_index_revisions (state, updated_at, id);

CREATE TABLE derived_index_changes (
	sequence INTEGER PRIMARY KEY AUTOINCREMENT,
	idempotency_key TEXT NOT NULL UNIQUE,
	canonical_user_id TEXT NOT NULL,
	entity_kind TEXT NOT NULL CHECK (entity_kind IN ('memory', 'session_turn', 'session_summary', 'tenant_profile')),
	entity_id TEXT NOT NULL,
	operation TEXT NOT NULL CHECK (operation IN ('upsert', 'delete')),
	state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'processing', 'completed', 'failed')),
	attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
	available_at TEXT NOT NULL,
	lease_owner TEXT NOT NULL DEFAULT '',
	lease_until TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	completed_at TEXT,
	last_error_code TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);

CREATE INDEX idx_derived_index_changes_ready
ON derived_index_changes (state, available_at, sequence);
CREATE INDEX idx_derived_index_changes_tenant_entity
ON derived_index_changes (canonical_user_id, entity_kind, entity_id, sequence);

CREATE TABLE maintenance_runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_type TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('running', 'completed', 'failed')),
	started_at TEXT NOT NULL,
	completed_at TEXT,
	rows_examined INTEGER NOT NULL DEFAULT 0 CHECK (rows_examined >= 0),
	rows_changed INTEGER NOT NULL DEFAULT 0 CHECK (rows_changed >= 0),
	last_error_code TEXT NOT NULL DEFAULT '',
	metadata TEXT NOT NULL DEFAULT '{}'
		CHECK (json_valid(metadata) AND json_type(metadata) = 'object')
);

CREATE INDEX idx_maintenance_runs_type_started
ON maintenance_runs (run_type, started_at DESC, id DESC);
`

func ensureColumnConn(ctx context.Context, conn *sql.Conn, table, name, definition string) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
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
		found = found || columnName == name
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
	if _, err := conn.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+name+` `+definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, name, err)
	}
	return nil
}
