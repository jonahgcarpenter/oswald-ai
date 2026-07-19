package database

import (
	"context"
	"database/sql"
	"fmt"
)

// Keep this definition byte-for-byte stable: it is part of the immutable
// ordered migration checksum.
const phase9MigrationDefinition = phase9PrivacyOperationRetentionTriggerSQL + phase9AuditRetentionTriggerSQL

const phase9PrivacyOperationRetentionTriggerSQL = `
DROP TRIGGER privacy_operations_no_delete;

CREATE TRIGGER privacy_operations_no_delete
BEFORE DELETE ON privacy_operations
WHEN OLD.status IN ('pending', 'running')
BEGIN
	SELECT RAISE(ABORT, 'active privacy operation records must be retained');
END;
`

const phase9AuditRetentionTriggerSQL = `
DROP TRIGGER memory_formation_audit_no_delete;

CREATE TRIGGER memory_formation_audit_no_delete
BEFORE DELETE ON memory_formation_audit
WHEN OLD.redacted_at IS NULL AND EXISTS (
	SELECT 1 FROM account_users
	WHERE canonical_user_id = OLD.canonical_user_id AND lifecycle_state = 'active'
)
BEGIN
	SELECT RAISE(ABORT, 'unredacted memory formation audit is append-only');
END;
`

func applyPhase9Migration(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, phase9PrivacyOperationRetentionTriggerSQL); err != nil {
		return fmt.Errorf("correct privacy operation retention trigger: %w", err)
	}
	if _, err := conn.ExecContext(ctx, phase9AuditRetentionTriggerSQL); err != nil {
		return fmt.Errorf("correct memory formation audit retention trigger: %w", err)
	}
	return nil
}
