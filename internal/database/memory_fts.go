package database

import (
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
	var existingSQL string
	err = tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'memory_entries_fts'`).Scan(&existingSQL)
	tableExisted := 1
	if err == sql.ErrNoRows {
		tableExisted = 0
	} else if err != nil {
		return fmt.Errorf("inspect memory FTS5 table: %w", err)
	}
	if strings.Contains(strings.ToLower(existingSQL), "unindexed") || strings.Contains(strings.ToLower(existingSQL), "content='memory_entries'") {
		if _, err := tx.Exec(`
DROP TRIGGER IF EXISTS memory_entries_fts_insert;
DROP TRIGGER IF EXISTS memory_entries_fts_delete;
DROP TRIGGER IF EXISTS memory_entries_fts_update;
DROP TABLE memory_entries_fts;
`); err != nil {
			return fmt.Errorf("replace legacy memory FTS5 table: %w", err)
		}
		tableExisted = 0
	}
	_, err = tx.Exec(`
CREATE VIRTUAL TABLE IF NOT EXISTS memory_entries_fts USING fts5(
	canonical_user_id,
	statement,
	evidence
);

DROP TRIGGER IF EXISTS memory_entries_fts_insert;
DROP TRIGGER IF EXISTS memory_entries_fts_delete;
DROP TRIGGER IF EXISTS memory_entries_fts_update;

CREATE TRIGGER memory_entries_fts_insert AFTER INSERT ON memory_entries
WHEN new.status = 'active' AND new.approval_state = 'approved' BEGIN
	INSERT INTO memory_entries_fts(rowid, canonical_user_id, statement, evidence)
	VALUES (new.id, new.canonical_user_id, new.statement, new.evidence);
END;

CREATE TRIGGER memory_entries_fts_delete AFTER DELETE ON memory_entries BEGIN
	DELETE FROM memory_entries_fts WHERE rowid = old.id;
END;

CREATE TRIGGER memory_entries_fts_update AFTER UPDATE OF canonical_user_id, statement, evidence, status, approval_state ON memory_entries BEGIN
	DELETE FROM memory_entries_fts WHERE rowid = old.id;
	INSERT INTO memory_entries_fts(rowid, canonical_user_id, statement, evidence)
	SELECT new.id, new.canonical_user_id, new.statement, new.evidence
	WHERE new.status = 'active' AND new.approval_state = 'approved';
END;
`)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such module: fts5") {
			return fmt.Errorf("%w: rebuild with the sqlite_fts5 build tag", ErrFTS5Unavailable)
		}
		return fmt.Errorf("create memory FTS5 index: %w", err)
	}
	var applied int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, memoryFTSMigration).Scan(&applied); err != nil {
		return fmt.Errorf("inspect memory FTS5 migration: %w", err)
	}
	if applied == 0 || tableExisted == 0 {
		if _, err := tx.Exec(`DELETE FROM memory_entries_fts; INSERT INTO memory_entries_fts(rowid, canonical_user_id, statement, evidence) SELECT id, canonical_user_id, statement, evidence FROM memory_entries WHERE status = 'active' AND approval_state = 'approved'`); err != nil {
			return fmt.Errorf("backfill memory FTS5 index: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(name, applied_at) VALUES (?, datetime('now')) ON CONFLICT(name) DO NOTHING`, memoryFTSMigration); err != nil {
			return fmt.Errorf("record memory FTS5 migration: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit memory FTS5 initialization: %w", err)
	}
	return nil
}
