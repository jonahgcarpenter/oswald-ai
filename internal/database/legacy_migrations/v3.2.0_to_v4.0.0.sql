CREATE TEMP TABLE legacy_v320_account_users AS
SELECT canonical_user_id, is_admin, is_banned, ban_reason FROM account_users;
CREATE TEMP TABLE legacy_v320_linked_accounts AS
SELECT gateway, identifier, canonical_user_id, display_name, verified
FROM linked_accounts
WHERE gateway != 'websocket';
CREATE TEMP TABLE legacy_v320_mcp_servers AS
SELECT id, scope, owner_user_id, name, transport, url_ciphertext, headers_ciphertext, enabled FROM mcp_servers;

DROP TABLE IF EXISTS memory_entry_vectors;
DROP TABLE memory_events;
DROP TABLE session_turns;
DROP TABLE memory_entries;
DROP TABLE user_memory_profiles;
DROP TABLE mcp_servers;
DROP TABLE linked_accounts;
DROP TABLE account_users;

-- PERMANENT-MIGRATION-v4.0.0

INSERT INTO account_users(canonical_user_id, is_admin, is_banned, ban_reason, lifecycle_state)
SELECT canonical_user_id, is_admin, is_banned, ban_reason, 'active' FROM legacy_v320_account_users;
INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, display_name, verified)
SELECT gateway, identifier, canonical_user_id, display_name, verified FROM legacy_v320_linked_accounts;
INSERT INTO mcp_servers(id, scope, owner_user_id, name, transport, url_ciphertext, headers_ciphertext, enabled)
SELECT id, scope, owner_user_id, name, transport, url_ciphertext, headers_ciphertext, enabled FROM legacy_v320_mcp_servers;

DROP TABLE legacy_v320_mcp_servers;
DROP TABLE legacy_v320_linked_accounts;
DROP TABLE legacy_v320_account_users;
