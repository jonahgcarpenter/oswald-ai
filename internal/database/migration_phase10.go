package database

import (
	"context"
	"database/sql"
	"fmt"
)

const (
	phase10MemoryEventsRedactedAtOperation = "ensure-column-if-missing:ALTER TABLE memory_events ADD COLUMN redacted_at TEXT;\n"
	phase10MemoryEventsUpdatedAtOperation  = "ensure-column-if-missing:ALTER TABLE memory_events ADD COLUMN updated_at TEXT;\n"
	phase10MemoryEventsBackfillSQL         = `UPDATE memory_events SET updated_at = created_at WHERE updated_at IS NULL;`
	phase10MemoryEventsRedactionTriggerSQL = `
CREATE TRIGGER memory_events_redaction_timestamp
AFTER UPDATE OF metadata, request_id, session_id ON memory_events
WHEN NEW.redacted_at IS NULL
	AND NEW.metadata = '' AND NEW.request_id = '' AND NEW.session_id = ''
	AND (OLD.metadata != '' OR OLD.request_id != '' OR OLD.session_id != '')
BEGIN
	UPDATE memory_events
	SET redacted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
	WHERE id = NEW.id;
END;
`
	phase10MigrationDefinition = phase10MemoryEventsRedactedAtOperation + phase10MemoryEventsUpdatedAtOperation + phase10MemoryEventsBackfillSQL + phase10MemoryEventsRedactionTriggerSQL
)

func applyPhase10Migration(ctx context.Context, conn *sql.Conn) error {
	if err := ensureColumnConn(ctx, conn, "memory_events", "redacted_at", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumnConn(ctx, conn, "memory_events", "updated_at", "TEXT"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, phase10MemoryEventsBackfillSQL); err != nil {
		return fmt.Errorf("backfill memory event update timestamps: %w", err)
	}
	if _, err := conn.ExecContext(ctx, phase10MemoryEventsRedactionTriggerSQL); err != nil {
		return fmt.Errorf("create memory event redaction timestamp trigger: %w", err)
	}
	return nil
}
