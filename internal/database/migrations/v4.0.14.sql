CREATE TABLE linked_accounts_new (
	gateway TEXT NOT NULL,
	identifier TEXT NOT NULL,
	canonical_user_id TEXT NOT NULL,
	display_name TEXT NOT NULL DEFAULT '',
	verified INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (gateway, identifier),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);
INSERT INTO linked_accounts_new SELECT gateway, identifier, canonical_user_id, display_name, verified FROM linked_accounts;
DROP TABLE linked_accounts;
ALTER TABLE linked_accounts_new RENAME TO linked_accounts;
CREATE UNIQUE INDEX idx_linked_accounts_single_gateway ON linked_accounts(canonical_user_id, gateway) WHERE gateway != 'openai';
CREATE INDEX idx_linked_accounts_owner ON linked_accounts(canonical_user_id);

CREATE TABLE api_keys (
	key_id TEXT PRIMARY KEY,
	canonical_user_id TEXT NOT NULL REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	secret_hash BLOB NOT NULL CHECK (length(secret_hash) = 32),
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_api_keys_owner ON api_keys(canonical_user_id);
