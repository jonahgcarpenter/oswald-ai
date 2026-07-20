package database

import (
	"context"
	"database/sql"
	"fmt"
)

const phase12WebSocketDeviceAuthorizationSQL = `
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
CREATE INDEX websocket_device_authorizations_state_expiry_idx
	ON websocket_device_authorizations(state, expires_at, id);
CREATE INDEX websocket_device_authorizations_target_idx
	ON websocket_device_authorizations(target_user_id, state, id)
	WHERE target_user_id IS NOT NULL;

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
	is_bootstrap INTEGER NOT NULL DEFAULT 0 CHECK (is_bootstrap IN (0, 1)),
	created_at TEXT NOT NULL,
	last_used_at TEXT,
	revoked_at TEXT,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	CHECK ((refresh_token_hash IS NULL) = (refresh_expires_at IS NULL)),
	CHECK ((previous_refresh_token_hash IS NULL) = (previous_token_grace_expires_at IS NULL)),
	CHECK (is_bootstrap = 0 OR (refresh_token_hash IS NULL AND previous_refresh_token_hash IS NULL))
);
CREATE UNIQUE INDEX websocket_clients_refresh_hash_idx
	ON websocket_clients(refresh_token_hash) WHERE refresh_token_hash IS NOT NULL;
CREATE UNIQUE INDEX websocket_clients_previous_refresh_hash_idx
	ON websocket_clients(previous_refresh_token_hash) WHERE previous_refresh_token_hash IS NOT NULL;
CREATE INDEX websocket_clients_user_idx
	ON websocket_clients(canonical_user_id, revoked_at, created_at);
CREATE INDEX websocket_clients_identifier_idx
	ON websocket_clients(websocket_identifier, revoked_at);

CREATE TABLE websocket_bootstrap_state (
	singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
	default_user_id TEXT NOT NULL,
	websocket_identifier TEXT NOT NULL CHECK (length(websocket_identifier) BETWEEN 1 AND 256),
	bootstrap_client_id TEXT NOT NULL UNIQUE,
	state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'completed')),
	permanent_admin_user_id TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	completed_at TEXT,
	FOREIGN KEY (default_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (bootstrap_client_id) REFERENCES websocket_clients(client_id) ON DELETE CASCADE,
	FOREIGN KEY (permanent_admin_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	CHECK ((state = 'completed') = (completed_at IS NOT NULL)),
	CHECK (state != 'completed' OR permanent_admin_user_id IS NOT NULL),
	CHECK (permanent_admin_user_id IS NULL OR permanent_admin_user_id != default_user_id)
);
`

const phase12MigrationDefinition = phase12WebSocketDeviceAuthorizationSQL

func applyPhase12Migration(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, phase12WebSocketDeviceAuthorizationSQL); err != nil {
		return fmt.Errorf("create websocket device authorization schema: %w", err)
	}
	return nil
}
