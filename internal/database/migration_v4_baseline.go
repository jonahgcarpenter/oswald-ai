package database

import (
	"context"
	"database/sql"
	"fmt"
)

// v4 databases are disposable until release. This is the complete canonical
// schema applied to a fresh database and hashed into the frozen migration
// ledger. The runner owns schema_migration_versions, while FTS tables are
// separately initialized derived capabilities.
const compactV4BaselineDefinition = `
CREATE TABLE account_users (
	canonical_user_id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	is_admin INTEGER NOT NULL DEFAULT 0,
	is_banned INTEGER NOT NULL DEFAULT 0,
	banned_at TEXT NOT NULL DEFAULT '',
	banned_by TEXT NOT NULL DEFAULT '',
	ban_reason TEXT NOT NULL DEFAULT '',
	lifecycle_state TEXT NOT NULL DEFAULT 'active' CHECK (lifecycle_state IN ('active', 'erasing')),
	speaker_intro TEXT NOT NULL DEFAULT ''
);

CREATE TABLE linked_accounts (
	gateway TEXT NOT NULL,
	identifier TEXT NOT NULL,
	canonical_user_id TEXT NOT NULL,
	display_name TEXT NOT NULL DEFAULT '',
	linked_at TEXT NOT NULL,
	verified INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (gateway, identifier),
	UNIQUE (canonical_user_id, gateway),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);

CREATE TABLE account_link_challenges (
	id TEXT PRIMARY KEY,
	code_hash TEXT NOT NULL UNIQUE,
	initiator_user_id TEXT NOT NULL,
	initiator_gateway TEXT NOT NULL,
	initiator_identifier TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	consumed_at TEXT,
	consumed_by_user_id TEXT,
	consumed_gateway TEXT,
	consumed_identifier TEXT,
	result_user_id TEXT,
	invalidated_at TEXT,
	invalidated_by_user_id TEXT,
	invalidated_reason TEXT
);
CREATE INDEX idx_account_link_challenges_expiry ON account_link_challenges(expires_at);
CREATE INDEX idx_account_link_challenges_initiator_state ON account_link_challenges(initiator_user_id, consumed_at, invalidated_at, expires_at);

CREATE TABLE session_turns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	canonical_user_id TEXT NOT NULL,
	user_text TEXT NOT NULL,
	assistant_text TEXT NOT NULL,
	tool_names TEXT NOT NULL DEFAULT '',
	importance INTEGER NOT NULL DEFAULT 2,
	created_at TEXT NOT NULL,
	expires_at TEXT,
	session_generation INTEGER NOT NULL DEFAULT 1,
	delivered_at TEXT,
	source_request_id TEXT NOT NULL DEFAULT '',
	delivery_failed_at TEXT,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);
CREATE INDEX idx_session_turns_session_created ON session_turns(session_id, created_at);
CREATE INDEX idx_session_turns_user_created ON session_turns(canonical_user_id, created_at);
CREATE INDEX idx_session_turns_context ON session_turns(canonical_user_id, session_id, session_generation, created_at DESC, id DESC);
CREATE INDEX idx_session_turns_expiry ON session_turns(expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX idx_session_turns_pending_delivery ON session_turns(created_at, id) WHERE delivered_at IS NULL AND delivery_failed_at IS NULL;
CREATE UNIQUE INDEX idx_session_turns_tenant_id ON session_turns(canonical_user_id, id);

CREATE TABLE mcp_servers (
	id TEXT PRIMARY KEY,
	scope TEXT NOT NULL CHECK (scope IN ('global', 'user')),
	owner_user_id TEXT,
	name TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT 'generic',
	transport TEXT NOT NULL CHECK (transport IN ('streamable_http', 'sse')),
	url_ciphertext TEXT NOT NULL,
	url_host_hash TEXT NOT NULL DEFAULT '',
	headers_ciphertext TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	CHECK ((scope = 'global' AND owner_user_id IS NULL) OR (scope = 'user' AND owner_user_id IS NOT NULL))
);
CREATE UNIQUE INDEX mcp_servers_global_name_unique ON mcp_servers(name) WHERE scope = 'global';
CREATE UNIQUE INDEX mcp_servers_user_name_unique ON mcp_servers(owner_user_id, name) WHERE scope = 'user';

CREATE TABLE memory_candidates (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'proposed' CHECK (state IN ('proposed', 'approved', 'rejected')),
	publication_status TEXT NOT NULL DEFAULT 'none' CHECK (publication_status IN ('none', 'published', 'blocked_conflict')),
	scope TEXT NOT NULL CHECK (scope IN ('short_term', 'long_term')),
	category TEXT NOT NULL CHECK (category IN ('identity', 'communication_preferences', 'durable_preferences', 'projects', 'relationships', 'environment', 'notes')),
	statement TEXT NOT NULL,
	evidence TEXT NOT NULL DEFAULT '',
	confidence REAL NOT NULL DEFAULT 0.8 CHECK (confidence >= 0 AND confidence <= 1),
	importance INTEGER NOT NULL DEFAULT 3 CHECK (importance BETWEEN 1 AND 5),
	provenance_type TEXT NOT NULL,
	source_turn_id INTEGER,
	extraction_model TEXT NOT NULL DEFAULT '',
	extractor_version TEXT NOT NULL DEFAULT '',
	formation_mode TEXT NOT NULL,
	sensitivity TEXT NOT NULL DEFAULT 'unknown',
	supersedes_memory_id INTEGER,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	expires_at TEXT,
	decision_reason TEXT NOT NULL DEFAULT '',
	published_memory_id INTEGER,
	claim_slot TEXT NOT NULL DEFAULT '',
	claim_value TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (source_turn_id) REFERENCES session_turns(id) ON DELETE SET NULL,
	FOREIGN KEY (published_memory_id) REFERENCES memory_entries(id) ON DELETE CASCADE,
	FOREIGN KEY (canonical_user_id, supersedes_memory_id) REFERENCES memory_entries(canonical_user_id, id),
	UNIQUE (canonical_user_id, idempotency_key),
	UNIQUE (canonical_user_id, id),
	CHECK ((publication_status = 'published') = (published_memory_id IS NOT NULL)),
	CHECK (length(trim(statement)) > 0),
	CHECK (publication_status != 'blocked_conflict' OR (state = 'approved' AND published_memory_id IS NULL)),
	CHECK (state = 'approved' OR publication_status = 'none')
);
CREATE INDEX idx_memory_candidates_state ON memory_candidates(canonical_user_id, state, created_at);
CREATE INDEX idx_memory_candidates_publication ON memory_candidates(canonical_user_id, publication_status, updated_at, id);
CREATE INDEX idx_memory_candidates_source_turn ON memory_candidates(canonical_user_id, source_turn_id);
CREATE INDEX idx_memory_candidates_claim_identity ON memory_candidates(canonical_user_id, scope, claim_slot, claim_value);
CREATE INDEX idx_memory_candidates_published ON memory_candidates(canonical_user_id, published_memory_id, created_at, id) WHERE published_memory_id IS NOT NULL;

CREATE TABLE memory_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	scope TEXT NOT NULL CHECK (scope IN ('short_term', 'long_term')),
	category TEXT NOT NULL CHECK (category IN ('identity', 'communication_preferences', 'durable_preferences', 'projects', 'relationships', 'environment', 'notes')),
	statement TEXT NOT NULL,
	confidence REAL NOT NULL DEFAULT 0.8,
	importance INTEGER NOT NULL DEFAULT 3,
	status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'expired', 'superseded', 'deleted')),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	last_used_at TEXT,
	expires_at TEXT,
	supersedes_id INTEGER,
	provenance_type TEXT NOT NULL DEFAULT 'legacy_import',
	sensitivity TEXT NOT NULL DEFAULT 'unknown',
	status_changed_at TEXT NOT NULL DEFAULT '',
	status_reason TEXT NOT NULL DEFAULT '',
	claim_slot TEXT NOT NULL DEFAULT '',
	claim_value TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (supersedes_id) REFERENCES memory_entries(id) ON DELETE SET NULL,
	UNIQUE (canonical_user_id, id)
);
CREATE INDEX idx_memory_entries_user_scope_category ON memory_entries(canonical_user_id, scope, category, status);
CREATE INDEX idx_memory_entries_user_updated ON memory_entries(canonical_user_id, updated_at);
CREATE INDEX idx_memory_entries_expiry ON memory_entries(expires_at, status);
CREATE INDEX idx_memory_entries_active_serving ON memory_entries(canonical_user_id, scope, category, updated_at DESC, id DESC) WHERE status = 'active';
CREATE INDEX idx_memory_entries_claim_identity ON memory_entries(canonical_user_id, scope, claim_slot, claim_value);
CREATE INDEX idx_memory_entries_claim_slot_status ON memory_entries(canonical_user_id, scope, claim_slot, status);

CREATE TABLE session_summaries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	session_generation INTEGER NOT NULL CHECK (session_generation > 0),
	covered_from_turn_id INTEGER NOT NULL,
	covered_through_turn_id INTEGER NOT NULL,
	narrative TEXT NOT NULL,
	open_tasks TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(open_tasks) AND json_type(open_tasks) = 'array'),
	commitments TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(commitments) AND json_type(commitments) = 'array'),
	entities TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(entities) AND json_type(entities) = 'array'),
	decisions TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(decisions) AND json_type(decisions) = 'array'),
	topic_tags TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(topic_tags) AND json_type(topic_tags) = 'array'),
	generation_model TEXT NOT NULL,
	generator_version TEXT NOT NULL,
	source_digest TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT,
	source_turn_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(source_turn_ids) AND json_type(source_turn_ids) = 'array'),
	CHECK (covered_from_turn_id <= covered_through_turn_id),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	UNIQUE (canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id),
	UNIQUE (canonical_user_id, id)
);
CREATE INDEX idx_session_summaries_context ON session_summaries(canonical_user_id, session_id, session_generation, covered_through_turn_id DESC);
CREATE INDEX idx_session_summaries_expiry ON session_summaries(expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE memory_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	memory_id INTEGER,
	event_type TEXT NOT NULL,
	request_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	metadata TEXT NOT NULL DEFAULT '',
	event_kind TEXT NOT NULL DEFAULT 'lifecycle' CHECK (event_kind IN ('lifecycle', 'formation_audit')),
	idempotency_key TEXT NOT NULL DEFAULT '',
	candidate_id INTEGER,
	job_id INTEGER,
	turn_id INTEGER,
	actor_type TEXT NOT NULL DEFAULT '',
	actor_id TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (memory_id) REFERENCES memory_entries(id) ON DELETE SET NULL
);
CREATE INDEX idx_memory_events_tenant_time ON memory_events(canonical_user_id, created_at, id);
CREATE INDEX idx_memory_events_memory ON memory_events(canonical_user_id, memory_id, created_at);
CREATE UNIQUE INDEX idx_memory_events_audit_key ON memory_events(canonical_user_id, idempotency_key) WHERE idempotency_key != '';

CREATE INDEX idx_account_users_lifecycle ON account_users(lifecycle_state, updated_at, canonical_user_id);

CREATE TABLE derived_index_revisions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	index_kind TEXT NOT NULL CHECK (index_kind IN ('memory_fts', 'transcript_fts', 'memory_vector', 'global_memory_fts', 'global_memory_vector')),
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	dimension INTEGER NOT NULL DEFAULT 0 CHECK (dimension >= 0),
	schema_version INTEGER NOT NULL CHECK (schema_version > 0),
	revision INTEGER NOT NULL CHECK (revision > 0),
	table_name TEXT NOT NULL UNIQUE,
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
CREATE UNIQUE INDEX idx_derived_index_revisions_live_kind ON derived_index_revisions(index_kind) WHERE state = 'live';
CREATE INDEX idx_derived_index_revisions_state ON derived_index_revisions(state, updated_at, id);

CREATE TABLE websocket_device_authorizations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	device_code_hash BLOB NOT NULL UNIQUE CHECK (length(device_code_hash) = 32),
	user_code_hash BLOB NOT NULL UNIQUE CHECK (length(user_code_hash) = 32),
	requested_client_name TEXT NOT NULL CHECK (length(trim(requested_client_name)) BETWEEN 1 AND 128),
	state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'approved', 'consumed', 'expired')),
	target_user_id TEXT,
	websocket_identifier TEXT NOT NULL DEFAULT '' CHECK (length(websocket_identifier) <= 256),
	poll_interval_seconds INTEGER NOT NULL DEFAULT 5 CHECK (poll_interval_seconds > 0),
	poll_count INTEGER NOT NULL DEFAULT 0 CHECK (poll_count >= 0),
	last_polled_at TEXT,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	approved_at TEXT,
	consumed_at TEXT,
	expired_at TEXT,
	FOREIGN KEY (target_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	CHECK ((state = 'pending') = (target_user_id IS NULL AND websocket_identifier = '' AND approved_at IS NULL AND consumed_at IS NULL AND expired_at IS NULL)),
	CHECK (state NOT IN ('approved', 'consumed') OR (target_user_id IS NOT NULL AND websocket_identifier != '' AND approved_at IS NOT NULL)),
	CHECK ((state = 'consumed') = (consumed_at IS NOT NULL)),
	CHECK ((state = 'expired') = (expired_at IS NOT NULL)),
	CHECK (state != 'expired' OR consumed_at IS NULL)
);
CREATE INDEX websocket_device_authorizations_state_expiry_idx ON websocket_device_authorizations(state, expires_at, id);
CREATE INDEX websocket_device_authorizations_target_idx ON websocket_device_authorizations(target_user_id, state, id) WHERE target_user_id IS NOT NULL;

CREATE TABLE websocket_clients (
	client_id TEXT PRIMARY KEY CHECK (length(client_id) BETWEEN 16 AND 128),
	canonical_user_id TEXT NOT NULL,
	websocket_identifier TEXT NOT NULL CHECK (length(websocket_identifier) BETWEEN 1 AND 256),
	client_name TEXT NOT NULL CHECK (length(trim(client_name)) BETWEEN 1 AND 128),
	refresh_token_hash BLOB CHECK (refresh_token_hash IS NULL OR length(refresh_token_hash) = 32),
	previous_refresh_token_hash BLOB CHECK (previous_refresh_token_hash IS NULL OR length(previous_refresh_token_hash) = 32),
	previous_token_grace_expires_at TEXT,
	refresh_expires_at TEXT,
	token_version INTEGER NOT NULL DEFAULT 1 CHECK (token_version > 0),
	created_at TEXT NOT NULL,
	last_used_at TEXT,
	revoked_at TEXT,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	CHECK ((refresh_token_hash IS NULL) = (refresh_expires_at IS NULL)),
	CHECK ((previous_refresh_token_hash IS NULL) = (previous_token_grace_expires_at IS NULL))
);
CREATE UNIQUE INDEX websocket_clients_refresh_hash_idx ON websocket_clients(refresh_token_hash) WHERE refresh_token_hash IS NOT NULL;
CREATE UNIQUE INDEX websocket_clients_previous_refresh_hash_idx ON websocket_clients(previous_refresh_token_hash) WHERE previous_refresh_token_hash IS NOT NULL;
CREATE INDEX websocket_clients_user_idx ON websocket_clients(canonical_user_id, revoked_at, created_at);
CREATE INDEX websocket_clients_identifier_idx ON websocket_clients(websocket_identifier, revoked_at);

CREATE TABLE global_memories (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	memory TEXT NOT NULL CHECK (length(trim(memory)) BETWEEN 1 AND 1000),
	memory_key TEXT NOT NULL UNIQUE CHECK (length(memory_key) BETWEEN 1 AND 1000),
	created_at TEXT NOT NULL
);
CREATE INDEX idx_global_memories_created ON global_memories(created_at, id);

CREATE TABLE sessions (
	canonical_user_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	generation INTEGER NOT NULL CHECK (generation > 0),
	is_active INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0, 1)),
	started_at TEXT NOT NULL,
	last_seen_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	profile_version INTEGER NOT NULL CHECK (profile_version > 0),
	profile_version_high_water INTEGER NOT NULL CHECK (profile_version_high_water >= profile_version),
	renderer_version TEXT NOT NULL,
	source_digest TEXT NOT NULL,
	speaker_intro TEXT NOT NULL DEFAULT '',
	rendered_content TEXT NOT NULL,
	fact_count INTEGER NOT NULL CHECK (fact_count >= 0),
	profile_bytes INTEGER NOT NULL CHECK (profile_bytes >= 0),
	source_memory_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(source_memory_ids) AND json_type(source_memory_ids) = 'array'),
	profile_created_at TEXT NOT NULL,
	PRIMARY KEY (canonical_user_id, session_id),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);
CREATE INDEX idx_sessions_active_expiry ON sessions(is_active, expires_at);
CREATE INDEX idx_sessions_profile_source ON sessions(canonical_user_id, profile_version DESC);

CREATE TABLE durable_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_kind TEXT NOT NULL CHECK (job_kind IN ('memory_formation', 'session_compaction', 'derived_index')),
	idempotency_key TEXT NOT NULL,
	canonical_user_id TEXT REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	state TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'retry', 'succeeded', 'skipped', 'dead')),
	source_request_id TEXT NOT NULL DEFAULT '',
	source_session_id TEXT NOT NULL DEFAULT '',
	source_session_generation INTEGER NOT NULL DEFAULT 0 CHECK (source_session_generation >= 0),
	source_turn_id INTEGER REFERENCES session_turns(id) ON DELETE SET NULL,
	extraction_model TEXT NOT NULL DEFAULT '',
	extractor_version TEXT NOT NULL DEFAULT '',
	extraction_payload TEXT NOT NULL DEFAULT '',
	session_id TEXT,
	session_generation INTEGER,
	covered_from_turn_id INTEGER,
	covered_through_turn_id INTEGER,
	artifact_payload TEXT NOT NULL DEFAULT '',
	artifact_summary_id INTEGER REFERENCES session_summaries(id) ON DELETE SET NULL,
	generation_model TEXT NOT NULL DEFAULT '',
	generator_version TEXT NOT NULL DEFAULT '',
	entity_kind TEXT,
	entity_id INTEGER,
	operation TEXT,
	attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
	redrive_count INTEGER NOT NULL DEFAULT 0 CHECK (redrive_count >= 0),
	available_at TEXT NOT NULL,
	lease_owner TEXT NOT NULL DEFAULT '',
	lease_until TEXT,
	started_at TEXT,
	completed_at TEXT,
	last_error_code TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (job_kind, idempotency_key),
	CHECK ((job_kind = 'derived_index' AND entity_kind = 'global_memory' AND canonical_user_id IS NULL)
		OR (NOT (job_kind = 'derived_index' AND entity_kind = 'global_memory') AND canonical_user_id IS NOT NULL)),
	CHECK (job_kind != 'memory_formation' OR (source_session_generation > 0 AND extractor_version != '')),
	CHECK (job_kind NOT IN ('memory_formation', 'derived_index') OR ((state = 'running' AND lease_owner != '' AND lease_until IS NOT NULL) OR (state != 'running' AND lease_owner = '' AND lease_until IS NULL))),
	CHECK (job_kind != 'session_compaction' OR (session_id IS NOT NULL AND session_generation > 0 AND covered_from_turn_id > 0 AND covered_through_turn_id >= covered_from_turn_id)),
	CHECK (job_kind != 'session_compaction' OR attempt_count <= 3),
	CHECK (job_kind != 'derived_index' OR (entity_kind IN ('memory', 'session_turn', 'global_memory') AND entity_id > 0 AND operation IN ('upsert', 'delete')))
);
CREATE UNIQUE INDEX idx_durable_jobs_compaction_range ON durable_jobs(canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id) WHERE job_kind = 'session_compaction';
CREATE INDEX idx_durable_jobs_ready ON durable_jobs(job_kind, state, available_at, id);
CREATE INDEX idx_durable_jobs_tenant ON durable_jobs(canonical_user_id, job_kind, state, created_at) WHERE canonical_user_id IS NOT NULL;
CREATE INDEX idx_durable_jobs_session ON durable_jobs(canonical_user_id, session_id, session_generation, state, id) WHERE job_kind = 'session_compaction';
CREATE INDEX idx_durable_jobs_source_turn ON durable_jobs(canonical_user_id, source_turn_id, id) WHERE job_kind = 'memory_formation';
CREATE INDEX idx_durable_jobs_entity ON durable_jobs(canonical_user_id, entity_kind, entity_id, id) WHERE job_kind = 'derived_index';

CREATE TRIGGER session_summaries_range_insert
BEFORE INSERT ON session_summaries
WHEN json_array_length(NEW.source_turn_ids) = 0
	OR EXISTS (
		SELECT 1 FROM json_each(NEW.source_turn_ids) source
		WHERE source.type != 'integer'
	)
	OR (SELECT value FROM json_each(NEW.source_turn_ids) ORDER BY key LIMIT 1) != NEW.covered_from_turn_id
	OR (SELECT value FROM json_each(NEW.source_turn_ids) ORDER BY key DESC LIMIT 1) != NEW.covered_through_turn_id
	OR EXISTS (
		SELECT 1
		FROM json_each(NEW.source_turn_ids) source
		JOIN json_each(NEW.source_turn_ids) previous ON previous.key = source.key - 1
		WHERE source.value <= previous.value
	)
	OR EXISTS (
		SELECT 1 FROM json_each(NEW.source_turn_ids) source
		WHERE NOT EXISTS (
			SELECT 1 FROM session_turns turn
			WHERE turn.id = source.value AND turn.canonical_user_id = NEW.canonical_user_id
				AND turn.session_id = NEW.session_id AND turn.session_generation = NEW.session_generation
		)
	)
BEGIN
	SELECT RAISE(ABORT, 'invalid session summary source turns');
END;
CREATE TRIGGER session_summaries_no_update
BEFORE UPDATE ON session_summaries
BEGIN
	SELECT RAISE(ABORT, 'session summary checkpoints are immutable');
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

CREATE TRIGGER memory_events_tenant_insert
BEFORE INSERT ON memory_events
WHEN (NEW.memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.memory_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.candidate_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_candidates WHERE id = NEW.candidate_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.job_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM durable_jobs WHERE id = NEW.job_id AND canonical_user_id = NEW.canonical_user_id AND job_kind = 'memory_formation'
)) OR (NEW.turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns WHERE id = NEW.turn_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory event reference');
END;
CREATE TRIGGER memory_events_tenant_update
BEFORE UPDATE OF canonical_user_id, memory_id, candidate_id, job_id, turn_id ON memory_events
WHEN (NEW.memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.memory_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.candidate_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_candidates WHERE id = NEW.candidate_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.job_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM durable_jobs WHERE id = NEW.job_id AND canonical_user_id = NEW.canonical_user_id AND job_kind = 'memory_formation'
)) OR (NEW.turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns WHERE id = NEW.turn_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory event reference');
END;

CREATE TRIGGER sessions_profile_sources_insert
BEFORE INSERT ON sessions
WHEN EXISTS (
	SELECT 1 FROM json_each(NEW.source_memory_ids) source
	WHERE source.type != 'integer'
)
	OR (SELECT COUNT(*) FROM json_each(NEW.source_memory_ids)) != (
		SELECT COUNT(DISTINCT value) FROM json_each(NEW.source_memory_ids)
	)
	OR EXISTS (
		SELECT 1 FROM json_each(NEW.source_memory_ids) source
		WHERE NOT EXISTS (
			SELECT 1 FROM memory_entries memory
			WHERE memory.id = source.value AND memory.canonical_user_id = NEW.canonical_user_id
		)
	)
BEGIN
	SELECT RAISE(ABORT, 'invalid session profile memory sources');
END;
CREATE TRIGGER sessions_profile_sources_update
BEFORE UPDATE OF canonical_user_id, source_memory_ids ON sessions
WHEN EXISTS (
	SELECT 1 FROM json_each(NEW.source_memory_ids) source
	WHERE source.type != 'integer'
)
	OR (SELECT COUNT(*) FROM json_each(NEW.source_memory_ids)) != (
		SELECT COUNT(DISTINCT value) FROM json_each(NEW.source_memory_ids)
	)
	OR EXISTS (
		SELECT 1 FROM json_each(NEW.source_memory_ids) source
		WHERE NOT EXISTS (
			SELECT 1 FROM memory_entries memory
			WHERE memory.id = source.value AND memory.canonical_user_id = NEW.canonical_user_id
		)
	)
BEGIN
	SELECT RAISE(ABORT, 'invalid session profile memory sources');
END;

CREATE TRIGGER memory_events_formation_identity_immutable
BEFORE UPDATE ON memory_events
WHEN OLD.event_kind = 'formation_audit'
	AND EXISTS (SELECT 1 FROM account_users WHERE canonical_user_id = OLD.canonical_user_id AND lifecycle_state = 'active')
	AND (NEW.canonical_user_id != OLD.canonical_user_id OR NEW.id != OLD.id OR NEW.event_kind != OLD.event_kind
		OR NEW.idempotency_key != OLD.idempotency_key OR NEW.event_type != OLD.event_type
		OR COALESCE(NEW.candidate_id, 0) != COALESCE(OLD.candidate_id, 0)
		OR COALESCE(NEW.memory_id, 0) != COALESCE(OLD.memory_id, 0)
		OR COALESCE(NEW.job_id, 0) != COALESCE(OLD.job_id, 0)
		OR COALESCE(NEW.turn_id, 0) != COALESCE(OLD.turn_id, 0) OR NEW.created_at != OLD.created_at)
BEGIN
	SELECT RAISE(ABORT, 'memory formation audit identity is immutable');
END;

CREATE TRIGGER session_turns_summary_content_update
BEFORE UPDATE OF user_text, assistant_text ON session_turns
WHEN EXISTS (
	SELECT 1 FROM session_summaries summary, json_each(summary.source_turn_ids) source
	WHERE CAST(source.value AS INTEGER) = OLD.id
)
BEGIN
	SELECT RAISE(ABORT, 'session summary source content is immutable');
END;
CREATE TRIGGER session_turns_summary_delete
BEFORE DELETE ON session_turns
WHEN EXISTS (
	SELECT 1 FROM session_summaries summary, json_each(summary.source_turn_ids) source
	WHERE CAST(source.value AS INTEGER) = OLD.id
)
BEGIN
	SELECT RAISE(ABORT, 'delete session summaries before source turns');
END;

CREATE TRIGGER durable_jobs_formation_source_insert
BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind = 'memory_formation' AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.source_session_id
		AND session_generation = NEW.source_session_generation
		AND source_request_id = NEW.source_request_id
		AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL
)
BEGIN
	SELECT RAISE(ABORT, 'invalid memory formation source turn');
END;
CREATE TRIGGER durable_jobs_formation_source_update
BEFORE UPDATE OF canonical_user_id, source_request_id, source_session_id, source_session_generation, source_turn_id ON durable_jobs
WHEN NEW.job_kind = 'memory_formation' AND NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.source_session_id
		AND session_generation = NEW.source_session_generation
		AND source_request_id = NEW.source_request_id
		AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL
)
BEGIN
	SELECT RAISE(ABORT, 'invalid memory formation source turn');
END;
CREATE TRIGGER durable_jobs_compaction_range_insert
BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind = 'session_compaction' AND (
	NOT EXISTS (SELECT 1 FROM session_turns WHERE id = NEW.covered_from_turn_id AND canonical_user_id = NEW.canonical_user_id AND session_id = NEW.session_id AND session_generation = NEW.session_generation)
	OR NOT EXISTS (SELECT 1 FROM session_turns WHERE id = NEW.covered_through_turn_id AND canonical_user_id = NEW.canonical_user_id AND session_id = NEW.session_id AND session_generation = NEW.session_generation)
)
BEGIN
	SELECT RAISE(ABORT, 'invalid session compaction job turn range');
END;
CREATE TRIGGER durable_jobs_compaction_range_update
BEFORE UPDATE OF canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id ON durable_jobs
WHEN OLD.job_kind = 'session_compaction' AND (
	NEW.canonical_user_id != OLD.canonical_user_id OR NEW.session_id != OLD.session_id
	OR NEW.session_generation != OLD.session_generation OR NEW.covered_from_turn_id != OLD.covered_from_turn_id
	OR NEW.covered_through_turn_id != OLD.covered_through_turn_id
)
BEGIN
	SELECT RAISE(ABORT, 'session compaction job range is immutable');
END;
CREATE TRIGGER durable_jobs_compaction_artifact_update
BEFORE UPDATE OF artifact_summary_id ON durable_jobs
WHEN NEW.job_kind = 'session_compaction' AND NEW.artifact_summary_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_summaries WHERE id = NEW.artifact_summary_id AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.session_id AND session_generation = NEW.session_generation
		AND covered_from_turn_id = NEW.covered_from_turn_id AND covered_through_turn_id = NEW.covered_through_turn_id
)
BEGIN
	SELECT RAISE(ABORT, 'invalid session compaction artifact');
END;
CREATE TRIGGER durable_jobs_compaction_artifact_insert
BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind = 'session_compaction' AND NEW.artifact_summary_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_summaries WHERE id = NEW.artifact_summary_id AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.session_id AND session_generation = NEW.session_generation
		AND covered_from_turn_id = NEW.covered_from_turn_id AND covered_through_turn_id = NEW.covered_through_turn_id
)
BEGIN
	SELECT RAISE(ABORT, 'invalid session compaction artifact');
END;
CREATE TRIGGER session_turns_durable_compaction_update
BEFORE UPDATE OF canonical_user_id, session_id, session_generation ON session_turns
WHEN EXISTS (
	SELECT 1 FROM durable_jobs job
	WHERE job.job_kind = 'session_compaction' AND OLD.id IN (job.covered_from_turn_id, job.covered_through_turn_id)
		AND (job.canonical_user_id != NEW.canonical_user_id OR job.session_id != NEW.session_id OR job.session_generation != NEW.session_generation)
)
BEGIN
	SELECT RAISE(ABORT, 'session turn has compaction references');
END;
`

func applyCompactV4Baseline(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, compactV4BaselineDefinition); err != nil {
		return fmt.Errorf("create compact v4 baseline: %w", err)
	}
	return nil
}
