CREATE TABLE IF NOT EXISTS account_users (
	canonical_user_id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	is_admin INTEGER NOT NULL DEFAULT 0,
	is_banned INTEGER NOT NULL DEFAULT 0,
	banned_at TEXT NOT NULL DEFAULT '',
	banned_by TEXT NOT NULL DEFAULT '',
	ban_reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS linked_accounts (
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
	category TEXT NOT NULL CHECK (category IN ('identity', 'system_rules', 'communication_preferences', 'durable_preferences', 'projects', 'relationships', 'environment', 'tasks', 'notes')),
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
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (supersedes_id) REFERENCES memory_entries(id) ON DELETE SET NULL,
	UNIQUE (canonical_user_id, scope, statement_key)
);
CREATE INDEX IF NOT EXISTS idx_memory_entries_user_scope_category ON memory_entries (canonical_user_id, scope, category, status);
CREATE INDEX IF NOT EXISTS idx_memory_entries_user_updated ON memory_entries (canonical_user_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_memory_entries_expiry ON memory_entries (expires_at, status);
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
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_session_turns_session_created ON session_turns (session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_session_turns_user_created ON session_turns (canonical_user_id, created_at);
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
CREATE TABLE IF NOT EXISTS mcp_servers (
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
CREATE UNIQUE INDEX IF NOT EXISTS mcp_servers_global_name_unique ON mcp_servers(name) WHERE scope = 'global';
CREATE UNIQUE INDEX IF NOT EXISTS mcp_servers_user_name_unique ON mcp_servers(owner_user_id, name) WHERE scope = 'user';
