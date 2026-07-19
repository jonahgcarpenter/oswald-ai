package database

import (
	"context"
	"database/sql"
	"fmt"
)

const phase11PrivacyInvalidationOutboxSQL = `
CREATE TABLE privacy_invalidation_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	idempotency_key TEXT NOT NULL UNIQUE CHECK (length(trim(idempotency_key)) > 0),
	operation_id TEXT NOT NULL CHECK (length(trim(operation_id)) > 0),
	external_identities TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(external_identities) AND json_type(external_identities) = 'array'),
	session_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(session_ids) AND json_type(session_ids) = 'array'),
	close_connections INTEGER NOT NULL DEFAULT 0 CHECK (close_connections IN (0, 1)),
	state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'processing', 'completed', 'failed')),
	attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
	available_at TEXT NOT NULL,
	lease_expires_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	completed_at TEXT,
	last_error_code TEXT NOT NULL DEFAULT '',
	CHECK ((state = 'processing') = (lease_expires_at IS NOT NULL)),
	CHECK (state != 'completed' OR (external_identities = '[]' AND session_ids = '[]' AND lease_expires_at IS NULL AND completed_at IS NOT NULL))
);
CREATE INDEX privacy_invalidation_events_dispatch_idx
	ON privacy_invalidation_events(state, available_at, id);
CREATE INDEX privacy_invalidation_events_lease_idx
	ON privacy_invalidation_events(lease_expires_at, id)
	WHERE state = 'processing';
CREATE INDEX privacy_invalidation_events_completed_idx
	ON privacy_invalidation_events(completed_at, id)
	WHERE state = 'completed';
`

const phase11MigrationDefinition = phase11PrivacyInvalidationOutboxSQL

func applyPhase11Migration(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, phase11PrivacyInvalidationOutboxSQL); err != nil {
		return fmt.Errorf("create privacy invalidation outbox: %w", err)
	}
	return nil
}
