package database

import (
	"context"
	"database/sql"
	"fmt"
)

const sessionTurnsFTSMigration = "session_turns_fts_v1"

func (d *DB) initializeSessionTurnsFTS5() error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin session transcript FTS5 initialization: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	_, err = tx.Exec(sessionTurnsFTSSchemaSQL)
	if err != nil {
		return classifyFTSError("create session transcript FTS5 index", err)
	}
	if _, err := tx.Exec(`
INSERT INTO schema_migrations(name, applied_at) VALUES (?, datetime('now'))
ON CONFLICT(name) DO NOTHING`, sessionTurnsFTSMigration); err != nil {
		return fmt.Errorf("record session transcript FTS5 migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session transcript FTS5 initialization: %w", err)
	}
	return nil
}

const sessionTurnsFTSSchemaSQL = `
CREATE VIRTUAL TABLE IF NOT EXISTS session_turns_fts USING fts5(
	canonical_user_id,
	session_id,
	session_generation,
	user_text,
	assistant_text
);

DROP TRIGGER IF EXISTS session_turns_fts_insert;
DROP TRIGGER IF EXISTS session_turns_fts_delete;
DROP TRIGGER IF EXISTS session_turns_fts_update;
`

func applySessionTurnsFTSMigration(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, sessionTurnsFTSSchemaSQL); err != nil {
		return classifyFTSError("create session transcript FTS5 index", err)
	}
	return nil
}
