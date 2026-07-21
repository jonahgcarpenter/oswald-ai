package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const memoryFTSMigration = "memory_fts_v3_active_only"

// ErrFTS5Unavailable indicates that SQLite was built without FTS5 support.
var ErrFTS5Unavailable = errors.New("sqlite FTS5 unavailable")

func (d *DB) initializeMemoryFTS5() error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin memory FTS5 initialization: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	_, err = tx.Exec(memoryFTSSchemaSQL)
	if err != nil {
		return classifyFTSError("create memory FTS5 index", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit memory FTS5 initialization: %w", err)
	}
	return nil
}

const memoryFTSSchemaSQL = `
CREATE VIRTUAL TABLE IF NOT EXISTS memory_entries_fts USING fts5(
	canonical_user_id,
	statement,
	evidence
);
`

func applyMemoryFTSMigration(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, memoryFTSSchemaSQL); err != nil {
		return classifyFTSError("create memory FTS5 index", err)
	}
	return nil
}

func classifyFTSError(operation string, err error) error {
	if strings.Contains(strings.ToLower(err.Error()), "no such module: fts5") {
		return fmt.Errorf("%w: rebuild with the sqlite_fts5 build tag", ErrFTS5Unavailable)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
