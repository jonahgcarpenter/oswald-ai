package database

import (
	"context"
	"database/sql"
	"fmt"
)

const phase8MigrationDefinition = phase8PrivacyOperationsRebuildSQL + phase8PrivacyTriggerCorrectionsSQL

func applyPhase8Migration(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, phase8PrivacyOperationsRebuildSQL); err != nil {
		return fmt.Errorf("rebuild privacy_operations for phase 8: %w", err)
	}
	if _, err := conn.ExecContext(ctx, phase8PrivacyTriggerCorrectionsSQL); err != nil {
		return fmt.Errorf("create phase 8 privacy trigger corrections: %w", err)
	}
	return nil
}

const phase8PrivacyOperationsRebuildSQL = `
DROP TRIGGER privacy_operations_no_delete;
DROP TRIGGER privacy_operations_identity_immutable;

CREATE TABLE privacy_operations_phase8 (
	operation_id TEXT PRIMARY KEY,
	idempotency_key TEXT NOT NULL,
	actor_hash TEXT NOT NULL CHECK (length(actor_hash) = 64),
	target_user_id TEXT,
	target_hash TEXT NOT NULL CHECK (length(target_hash) = 64),
	operation_type TEXT NOT NULL CHECK (operation_type IN ('forget_memory', 'delete_memory', 'delete_candidate', 'delete_session', 'delete_all_memories', 'delete_user', 'export_user')),
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

INSERT INTO privacy_operations_phase8 (
	operation_id, idempotency_key, actor_hash, target_user_id, target_hash,
	operation_type, target_digest, challenge_hash, challenge_expires_at, status,
	created_at, updated_at, started_at, completed_at, last_error_code
)
SELECT operation_id, idempotency_key, actor_hash, target_user_id, target_hash,
	operation_type, target_digest, challenge_hash, challenge_expires_at, status,
	created_at, updated_at, started_at, completed_at, last_error_code
FROM privacy_operations;

DROP TABLE privacy_operations;
ALTER TABLE privacy_operations_phase8 RENAME TO privacy_operations;

CREATE INDEX idx_privacy_operations_status
ON privacy_operations (status, updated_at, operation_id);
CREATE INDEX idx_privacy_operations_target
ON privacy_operations (target_user_id, created_at) WHERE target_user_id IS NOT NULL;

CREATE TRIGGER privacy_operations_no_delete
BEFORE DELETE ON privacy_operations
WHEN OLD.target_user_id IS NULL OR EXISTS (
	SELECT 1 FROM account_users
	WHERE canonical_user_id = OLD.target_user_id AND lifecycle_state = 'active'
)
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
`

const phase8PrivacyTriggerCorrectionsSQL = `
DROP TRIGGER memory_formation_audit_no_update;
DROP TRIGGER memory_formation_audit_no_delete;

CREATE TRIGGER memory_formation_audit_no_update
BEFORE UPDATE ON memory_formation_audit
WHEN NEW.canonical_user_id != OLD.canonical_user_id
	OR NEW.id != OLD.id
	OR NEW.idempotency_key != OLD.idempotency_key
	OR NEW.event_type != OLD.event_type
	OR COALESCE(NEW.candidate_id, 0) != COALESCE(OLD.candidate_id, 0)
	OR COALESCE(NEW.memory_id, 0) != COALESCE(OLD.memory_id, 0)
	OR COALESCE(NEW.job_id, 0) != COALESCE(OLD.job_id, 0)
	OR COALESCE(NEW.turn_id, 0) != COALESCE(OLD.turn_id, 0)
	OR NEW.created_at != OLD.created_at
BEGIN
	SELECT RAISE(ABORT, 'memory formation audit identity is immutable');
END;

CREATE TRIGGER memory_formation_audit_no_delete
BEFORE DELETE ON memory_formation_audit
WHEN EXISTS (
	SELECT 1 FROM account_users
	WHERE canonical_user_id = OLD.canonical_user_id AND lifecycle_state = 'active'
)
BEGIN
	SELECT RAISE(ABORT, 'memory formation audit is append-only');
END;
`
