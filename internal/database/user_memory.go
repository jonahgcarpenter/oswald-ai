package database

import (
	"database/sql"
	"fmt"
)

func (d *DB) initializeUserMemory() error {
	if _, err := d.db.Exec(userMemoryBaselineSQL); err != nil {
		return fmt.Errorf("failed to initialize user memory tables: %w", err)
	}
	if err := d.ensureUserMemoryColumn("memory_entries", "profile_approved", "INTEGER NOT NULL DEFAULT 0 CHECK (profile_approved IN (0, 1))"); err != nil {
		return err
	}
	if err := d.ensureUserMemoryColumn("session_turns", "session_generation", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if _, err := d.db.Exec(userMemoryCleanupIndexesSQL); err != nil {
		return fmt.Errorf("failed to initialize session cleanup indexes: %w", err)
	}
	if err := d.migrateStableTenantProfiles(); err != nil {
		return err
	}
	if err := d.migrateMemoryFormationSchema(); err != nil {
		return err
	}
	if err := d.migrateSessionCompactionSchema(); err != nil {
		return err
	}
	if _, err := d.db.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_entries_profile_candidates ON memory_entries (canonical_user_id, profile_approved, status, scope, category, expires_at)`); err != nil {
		return fmt.Errorf("failed to initialize profile candidate index: %w", err)
	}
	return nil
}

const userMemoryBaselineSQL = `
CREATE TABLE IF NOT EXISTS user_memory_profiles (
	canonical_user_id TEXT PRIMARY KEY,
	intro TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS memory_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	scope TEXT NOT NULL CHECK (scope IN ('short_term', 'long_term')),
	category TEXT NOT NULL CHECK (category IN ('identity', 'communication_preferences', 'durable_preferences', 'projects', 'relationships', 'environment', 'notes')),
	statement TEXT NOT NULL,
	statement_key TEXT NOT NULL,
	evidence TEXT NOT NULL,
	confidence REAL NOT NULL DEFAULT 0.8,
	importance INTEGER NOT NULL DEFAULT 3,
	status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'expired', 'superseded', 'deleted')),
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
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (supersedes_id) REFERENCES memory_entries(id) ON DELETE SET NULL,
	FOREIGN KEY (candidate_id) REFERENCES memory_candidates(id) ON DELETE SET NULL,
	FOREIGN KEY (source_turn_id) REFERENCES session_turns(id) ON DELETE SET NULL,
	UNIQUE (canonical_user_id, scope, statement_key)
);

CREATE INDEX IF NOT EXISTS idx_memory_entries_user_scope_category
ON memory_entries (canonical_user_id, scope, category, status);

CREATE INDEX IF NOT EXISTS idx_memory_entries_user_updated
ON memory_entries (canonical_user_id, updated_at);

CREATE INDEX IF NOT EXISTS idx_memory_entries_expiry
ON memory_entries (expires_at, status);

CREATE TABLE IF NOT EXISTS session_turns (
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
	delivered_at TEXT,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_session_turns_session_created
ON session_turns (session_id, created_at);

CREATE INDEX IF NOT EXISTS idx_session_turns_user_created
ON session_turns (canonical_user_id, created_at);

CREATE TABLE IF NOT EXISTS memory_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	memory_id INTEGER,
	event_type TEXT NOT NULL,
	request_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	metadata TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (memory_id) REFERENCES memory_entries(id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS tenant_profile_versions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	version INTEGER NOT NULL,
	renderer_version TEXT NOT NULL,
	source_digest TEXT NOT NULL,
	speaker_intro TEXT NOT NULL DEFAULT '',
	rendered_content TEXT NOT NULL,
	fact_count INTEGER NOT NULL,
	profile_bytes INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	UNIQUE (canonical_user_id, version),
	UNIQUE (canonical_user_id, id)
);

CREATE INDEX IF NOT EXISTS idx_tenant_profile_versions_latest
ON tenant_profile_versions (canonical_user_id, version DESC);

CREATE TABLE IF NOT EXISTS tenant_profile_version_counters (
	canonical_user_id TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS tenant_profile_version_facts (
	profile_version_id INTEGER NOT NULL,
	ordinal INTEGER NOT NULL,
	source_memory_id INTEGER,
	category TEXT NOT NULL,
	statement TEXT NOT NULL,
	PRIMARY KEY (profile_version_id, ordinal),
	FOREIGN KEY (profile_version_id) REFERENCES tenant_profile_versions(id) ON DELETE CASCADE,
	FOREIGN KEY (source_memory_id) REFERENCES memory_entries(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_tenant_profile_facts_source
ON tenant_profile_version_facts (source_memory_id);

CREATE TABLE IF NOT EXISTS tenant_sessions (
	canonical_user_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	generation INTEGER NOT NULL,
	profile_version_id INTEGER NOT NULL,
	started_at TEXT NOT NULL,
	last_seen_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	PRIMARY KEY (canonical_user_id, session_id),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (canonical_user_id, profile_version_id) REFERENCES tenant_profile_versions(canonical_user_id, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS tenant_session_generations (
	canonical_user_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	generation INTEGER NOT NULL,
	PRIMARY KEY (canonical_user_id, session_id),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS schema_migrations (
	name TEXT PRIMARY KEY,
	applied_at TEXT NOT NULL
);
`

const userMemoryCleanupIndexesSQL = `
CREATE INDEX IF NOT EXISTS idx_session_turns_context
ON session_turns (canonical_user_id, session_id, session_generation, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_session_turns_expiry
ON session_turns (expires_at) WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_tenant_sessions_expiry
ON tenant_sessions (expires_at);
`

func (d *DB) migrateStableTenantProfiles() error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin stable tenant profile migration: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	var applied int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = 'stable_tenant_profiles_v1'`).Scan(&applied); err != nil {
		return fmt.Errorf("failed to inspect stable tenant profile migration: %w", err)
	}
	if applied != 0 {
		return tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE memory_entries SET category = 'communication_preferences' WHERE category = 'system_rules'`); err != nil {
		return fmt.Errorf("failed to migrate system_rules memories: %w", err)
	}
	if _, err := tx.Exec(`UPDATE memory_entries SET profile_approved = 1`); err != nil {
		return fmt.Errorf("failed to approve existing canonical memories: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES ('stable_tenant_profiles_v1', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`); err != nil {
		return fmt.Errorf("failed to record stable tenant profile migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit stable tenant profile migration: %w", err)
	}
	return nil
}

func (d *DB) ensureUserMemoryColumn(table, name, definition string) error {
	rows, err := d.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("failed to inspect %s columns: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("failed to scan %s columns: %w", table, err)
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close() // nolint:errcheck
		return fmt.Errorf("failed to read %s columns: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("failed to close %s columns: %w", table, err)
	}
	if found {
		return nil
	}
	if _, err := d.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + definition); err != nil {
		return fmt.Errorf("failed to add %s.%s: %w", table, name, err)
	}
	return nil
}
