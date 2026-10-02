-- Operator-approved fresh-install schema. SQLite creates sqlite_sequence and
-- FTS shadow tables from AUTOINCREMENT and the virtual-table declarations.
CREATE TABLE schema_version (version INTEGER NOT NULL);
CREATE TABLE system_prompts (hash TEXT PRIMARY KEY, prompt TEXT NOT NULL);
CREATE TABLE sessions (
 id TEXT PRIMARY KEY, source TEXT NOT NULL, user_id TEXT, session_key TEXT,
 chat_id TEXT, chat_type TEXT, thread_id TEXT, display_name TEXT, origin_json TEXT,
 expiry_finalized INTEGER DEFAULT 0, model TEXT, model_config TEXT, system_prompt TEXT,
 system_prompt_hash TEXT, parent_session_id TEXT, started_at REAL NOT NULL,
 ended_at REAL, end_reason TEXT, message_count INTEGER DEFAULT 0,
 tool_call_count INTEGER DEFAULT 0, input_tokens INTEGER DEFAULT 0,
 output_tokens INTEGER DEFAULT 0, cache_read_tokens INTEGER DEFAULT 0,
 cache_write_tokens INTEGER DEFAULT 0, reasoning_tokens INTEGER DEFAULT 0,
 cwd TEXT, git_branch TEXT, git_repo_root TEXT,
 git_metadata_generation INTEGER NOT NULL DEFAULT 0,
 billing_provider TEXT, billing_base_url TEXT, billing_mode TEXT,
 estimated_cost_usd REAL, actual_cost_usd REAL, cost_status TEXT, cost_source TEXT,
 pricing_version TEXT, title TEXT, title_source TEXT, last_activity_at REAL,
 last_activity_description TEXT, last_activity_provenance TEXT,
 api_call_count INTEGER DEFAULT 0, handoff_state TEXT, handoff_platform TEXT,
 handoff_error TEXT, compression_failure_cooldown_until REAL,
 compression_failure_error TEXT, compression_fallback_streak INTEGER NOT NULL DEFAULT 0,
 compression_ineffective_count INTEGER NOT NULL DEFAULT 0,
 compression_recovery_deadline REAL, profile_name TEXT, transport_profile TEXT,
 rewind_count INTEGER NOT NULL DEFAULT 0, archived INTEGER NOT NULL DEFAULT 0,
 pinned INTEGER NOT NULL DEFAULT 0, hidden INTEGER NOT NULL DEFAULT 0,
 last_read_at REAL, tool_names TEXT,
 FOREIGN KEY (parent_session_id) REFERENCES sessions(id),
 FOREIGN KEY (system_prompt_hash) REFERENCES system_prompts(hash)
);
CREATE TABLE messages (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 session_id TEXT NOT NULL REFERENCES sessions(id), role TEXT NOT NULL, content TEXT,
 tool_call_id TEXT, tool_calls TEXT, tool_name TEXT, effect_disposition TEXT,
 timestamp REAL NOT NULL, token_count INTEGER, finish_reason TEXT, reasoning TEXT,
 reasoning_content TEXT, reasoning_details TEXT, codex_reasoning_items TEXT,
 codex_message_items TEXT, platform_message_id TEXT, observed INTEGER DEFAULT 0,
 _compressed_summary INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1,
 compacted INTEGER NOT NULL DEFAULT 0, api_content TEXT, display_kind TEXT,
 display_metadata TEXT, display_identity BLOB, display_order INTEGER
);
CREATE TABLE session_model_usage (
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 model TEXT NOT NULL, billing_provider TEXT NOT NULL DEFAULT '',
 billing_base_url TEXT NOT NULL DEFAULT '', billing_mode TEXT NOT NULL DEFAULT '',
 task TEXT NOT NULL DEFAULT '', api_call_count INTEGER NOT NULL DEFAULT 0,
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0,
 reasoning_tokens INTEGER NOT NULL DEFAULT 0, estimated_cost_usd REAL NOT NULL DEFAULT 0,
 actual_cost_usd REAL NOT NULL DEFAULT 0, cost_status TEXT, cost_source TEXT,
 first_seen REAL, last_seen REAL,
 PRIMARY KEY (session_id, model, billing_provider, billing_base_url, billing_mode, task)
);
CREATE TABLE state_meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE gateway_routing (
 scope TEXT NOT NULL DEFAULT '', session_key TEXT NOT NULL, entry_json TEXT NOT NULL,
 updated_at REAL NOT NULL, PRIMARY KEY (scope, session_key)
);
CREATE TABLE gateway_hygiene_state (
 session_key TEXT PRIMARY KEY, failure_streak INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE conversation_generations (
 source TEXT NOT NULL, session_key TEXT NOT NULL, generation INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY (source, session_key)
);
CREATE TABLE gateway_heartbeats (
 backend_id TEXT PRIMARY KEY, pid INTEGER NOT NULL, started_at REAL NOT NULL,
 last_heartbeat REAL NOT NULL, profile TEXT NOT NULL DEFAULT '', host TEXT NOT NULL DEFAULT ''
);
CREATE TABLE compression_locks (
 session_id TEXT PRIMARY KEY, holder TEXT NOT NULL, acquired_at REAL NOT NULL, expires_at REAL NOT NULL
);
CREATE TABLE session_turn_leases (
 conversation_id TEXT PRIMARY KEY, holder TEXT NOT NULL, acquired_at REAL NOT NULL, expires_at REAL NOT NULL
);
CREATE TABLE async_delegations (
 delegation_id TEXT PRIMARY KEY, origin_session TEXT NOT NULL,
 origin_ui_session_id TEXT NOT NULL DEFAULT '', parent_session_id TEXT,
 state TEXT NOT NULL, dispatched_at REAL NOT NULL, completed_at REAL,
 updated_at REAL NOT NULL, event_json TEXT, result_json TEXT,
 delivery_state TEXT NOT NULL DEFAULT 'pending', delivery_attempts INTEGER NOT NULL DEFAULT 0,
 delivered_at REAL, owner_pid INTEGER, owner_started_at INTEGER, task_json TEXT,
 delivery_claim TEXT, delivery_claimed_at REAL, origin_session_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_sessions_source ON sessions(source);
CREATE INDEX idx_sessions_source_id ON sessions(source, id);
CREATE INDEX idx_sessions_parent ON sessions(parent_session_id);
CREATE INDEX idx_sessions_started ON sessions(started_at DESC);
CREATE INDEX idx_messages_session ON messages(session_id, timestamp);
CREATE INDEX idx_messages_session_id ON messages(session_id, id);
CREATE INDEX idx_messages_assistant_calls_by_session ON messages(session_id)
 WHERE role = 'assistant' AND tool_calls IS NOT NULL;
CREATE INDEX idx_compression_locks_expires ON compression_locks(expires_at);
CREATE INDEX idx_session_turn_leases_expires ON session_turn_leases(expires_at);
CREATE INDEX idx_session_model_usage_session ON session_model_usage(session_id);
CREATE INDEX idx_session_model_usage_model ON session_model_usage(model);
CREATE INDEX idx_async_delegations_delivery ON async_delegations(delivery_state, completed_at);
CREATE INDEX idx_messages_platform_msg_id ON messages(session_id, platform_message_id)
 WHERE platform_message_id IS NOT NULL;
CREATE INDEX idx_messages_session_active ON messages(session_id, active, timestamp);
CREATE INDEX idx_messages_display_page ON messages(session_id, display_order, active DESC, id DESC)
 WHERE active = 1 OR compacted = 1;
CREATE INDEX idx_messages_display_backfill ON messages(session_id)
 WHERE (display_order IS NULL OR display_identity IS NULL) AND (active = 1 OR compacted = 1);
CREATE INDEX idx_messages_display_identity ON messages(session_id, display_identity, display_order)
 WHERE display_identity IS NOT NULL AND (active = 1 OR compacted = 1);
CREATE TRIGGER messages_display_order_insert AFTER INSERT ON messages WHEN new.display_order IS NULL
BEGIN
 UPDATE messages SET display_order = COALESCE((
  SELECT display_order FROM messages WHERE session_id = new.session_id AND id <> new.id
   AND (active = 1 OR compacted = 1) AND display_identity = new.display_identity
   AND display_order IS NOT NULL ORDER BY display_order LIMIT 1
 ), new.id) WHERE id = new.id;
END;
CREATE TRIGGER messages_display_visibility_update AFTER UPDATE OF active, compacted ON messages
WHEN (new.active = 1 OR new.compacted = 1) <> (old.active = 1 OR old.compacted = 1)
BEGIN
 UPDATE messages SET display_order = MIN(new.id, COALESCE((
  SELECT display_order FROM messages WHERE session_id = new.session_id AND id <> new.id
   AND (active = 1 OR compacted = 1) AND display_identity = new.display_identity
   AND display_order IS NOT NULL ORDER BY display_order LIMIT 1
 ), new.id)) WHERE id = new.id AND (new.active = 1 OR new.compacted = 1);
 UPDATE messages SET display_order = (SELECT display_order FROM messages WHERE id = new.id)
 WHERE session_id = new.session_id AND id <> new.id AND (active = 1 OR compacted = 1)
  AND display_identity = new.display_identity AND (new.active = 1 OR new.compacted = 1);
 UPDATE messages SET display_order = (
  SELECT MIN(peer.id) FROM messages AS peer WHERE peer.session_id = old.session_id
   AND (peer.active = 1 OR peer.compacted = 1) AND peer.display_identity = old.display_identity
 ) WHERE session_id = old.session_id AND (active = 1 OR compacted = 1)
  AND display_identity = old.display_identity AND NOT (new.active = 1 OR new.compacted = 1);
END;
CREATE TRIGGER messages_display_identity_update
AFTER UPDATE OF role, content, timestamp, tool_call_id, tool_calls, tool_name, display_kind ON messages
WHEN new.role IS NOT old.role OR new.content IS NOT old.content OR new.timestamp IS NOT old.timestamp
 OR new.tool_call_id IS NOT old.tool_call_id OR new.tool_calls IS NOT old.tool_calls
 OR new.tool_name IS NOT old.tool_name OR new.display_kind IS NOT old.display_kind
BEGIN
 UPDATE messages SET display_identity = NULL, display_order = NULL
 WHERE id = new.id OR (session_id = old.session_id AND display_identity = old.display_identity
  AND (active = 1 OR compacted = 1));
END;
CREATE TRIGGER messages_display_identity_delete AFTER DELETE ON messages
WHEN old.active = 1 OR old.compacted = 1
BEGIN
 UPDATE messages SET display_order = (
  SELECT MIN(peer.id) FROM messages AS peer WHERE peer.session_id = old.session_id
   AND (peer.active = 1 OR peer.compacted = 1) AND peer.display_identity = old.display_identity
 ) WHERE session_id = old.session_id AND (active = 1 OR compacted = 1)
  AND display_identity = old.display_identity;
END;
CREATE INDEX idx_messages_active_null ON messages(active) WHERE active IS NULL;
CREATE INDEX idx_sessions_session_key ON sessions(session_key, started_at DESC);
CREATE INDEX idx_sessions_gateway_peer ON sessions(source, user_id, chat_id, chat_type, thread_id, started_at DESC);
CREATE INDEX idx_sessions_handoff_state ON sessions(handoff_state, started_at);
CREATE INDEX idx_sessions_system_prompt_hash ON sessions(system_prompt_hash);
CREATE INDEX idx_sessions_tool_names ON sessions(tool_names);
CREATE INDEX idx_sessions_effective_activity ON sessions(COALESCE(last_activity_at, started_at) DESC, started_at DESC);
CREATE UNIQUE INDEX idx_sessions_title_unique ON sessions(title) WHERE title IS NOT NULL;
CREATE VIEW messages_fts_src AS SELECT id,
 CASE WHEN role = 'tool' THEN substr(COALESCE(content, ''), 1, 8192) ELSE content END AS content,
 tool_name, tool_calls FROM messages;
CREATE VIRTUAL TABLE messages_fts USING fts5(content, tool_name, tool_calls,
 content='messages_fts_src', content_rowid='id');
CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages
WHEN (new.id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_high_water'), -1)
 OR new.id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_progress'), -1))
BEGIN
 INSERT INTO messages_fts(rowid, content, tool_name, tool_calls) VALUES (new.id,
 CASE WHEN new.role = 'tool' THEN substr(COALESCE(new.content, ''), 1, 8192) ELSE new.content END,
 new.tool_name, new.tool_calls);
END;
CREATE TRIGGER messages_fts_delete AFTER DELETE ON messages
WHEN (old.id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_high_water'), -1)
 OR old.id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_progress'), -1))
BEGIN
 INSERT INTO messages_fts(messages_fts, rowid, content, tool_name, tool_calls) VALUES ('delete', old.id,
 CASE WHEN old.role = 'tool' THEN substr(COALESCE(old.content, ''), 1, 8192) ELSE old.content END,
 old.tool_name, old.tool_calls);
END;
CREATE TRIGGER messages_fts_update AFTER UPDATE OF content, tool_name, tool_calls, role ON messages
WHEN (old.content IS NOT new.content OR old.tool_name IS NOT new.tool_name
 OR old.tool_calls IS NOT new.tool_calls OR old.role IS NOT new.role)
 AND (old.id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_high_water'), -1)
 OR old.id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_progress'), -1))
BEGIN
 INSERT INTO messages_fts(messages_fts, rowid, content, tool_name, tool_calls) VALUES ('delete', old.id,
 CASE WHEN old.role = 'tool' THEN substr(COALESCE(old.content, ''), 1, 8192) ELSE old.content END,
 old.tool_name, old.tool_calls);
 INSERT INTO messages_fts(rowid, content, tool_name, tool_calls) VALUES (new.id,
 CASE WHEN new.role = 'tool' THEN substr(COALESCE(new.content, ''), 1, 8192) ELSE new.content END,
 new.tool_name, new.tool_calls);
END;
CREATE VIEW messages_fts_trigram_src AS SELECT m.id, m.role, m.content, m.tool_name FROM messages AS m
 JOIN sessions AS s ON s.id = m.session_id WHERE m.role <> 'tool'
 AND s.source NOT IN ('cron', 'subagent')
 AND json_extract((CASE WHEN json_valid(s.model_config) THEN s.model_config ELSE json_object() END), '$._delegate_from') IS NULL;
CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content, tool_name,
 content='messages_fts_trigram_src', content_rowid='id', tokenize='trigram');
CREATE TRIGGER messages_fts_trigram_insert AFTER INSERT ON messages
WHEN new.role <> 'tool' AND EXISTS (SELECT 1 FROM sessions WHERE id = new.session_id
 AND source NOT IN ('cron', 'subagent')
 AND json_extract((CASE WHEN json_valid(model_config) THEN model_config ELSE json_object() END), '$._delegate_from') IS NULL)
 AND (new.id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_high_water'), -1)
 OR new.id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_progress'), -1))
BEGIN
 INSERT INTO messages_fts_trigram(rowid, content, tool_name) VALUES (new.id, new.content, new.tool_name);
END;
CREATE TRIGGER messages_fts_trigram_delete AFTER DELETE ON messages
WHEN old.role <> 'tool' AND EXISTS (SELECT 1 FROM sessions WHERE id = old.session_id
 AND source NOT IN ('cron', 'subagent')
 AND json_extract((CASE WHEN json_valid(model_config) THEN model_config ELSE json_object() END), '$._delegate_from') IS NULL)
 AND (old.id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_high_water'), -1)
 OR old.id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_progress'), -1))
BEGIN
 INSERT INTO messages_fts_trigram(messages_fts_trigram, rowid, content, tool_name)
 VALUES ('delete', old.id, old.content, old.tool_name);
END;
CREATE TRIGGER messages_fts_trigram_update AFTER UPDATE OF content, tool_name, role ON messages
WHEN (old.content IS NOT new.content OR old.tool_name IS NOT new.tool_name OR old.role IS NOT new.role)
 AND (old.id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_high_water'), -1)
 OR old.id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = 'fts_rebuild_progress'), -1))
BEGIN
 INSERT INTO messages_fts_trigram(messages_fts_trigram, rowid, content, tool_name)
 SELECT 'delete', old.id, old.content, old.tool_name WHERE old.role <> 'tool'
 AND EXISTS (SELECT 1 FROM sessions WHERE id = old.session_id AND source NOT IN ('cron', 'subagent')
 AND json_extract((CASE WHEN json_valid(model_config) THEN model_config ELSE json_object() END), '$._delegate_from') IS NULL);
 INSERT INTO messages_fts_trigram(rowid, content, tool_name)
 SELECT new.id, new.content, new.tool_name WHERE new.role <> 'tool'
 AND EXISTS (SELECT 1 FROM sessions WHERE id = new.session_id AND source NOT IN ('cron', 'subagent')
 AND json_extract((CASE WHEN json_valid(model_config) THEN model_config ELSE json_object() END), '$._delegate_from') IS NULL);
END;
