package database

import (
	"fmt"
	"strings"
)

const sessionTurnsFTSMigration = "session_turns_fts_v1"

func (d *DB) initializeSessionTurnsFTS5() error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin session transcript FTS5 initialization: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	_, err = tx.Exec(`
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

CREATE TRIGGER session_turns_fts_insert AFTER INSERT ON session_turns BEGIN
	INSERT INTO session_turns_fts(rowid, canonical_user_id, session_id, session_generation, user_text, assistant_text)
	VALUES (NEW.id, NEW.canonical_user_id, NEW.session_id, NEW.session_generation, NEW.user_text, NEW.assistant_text);
END;

CREATE TRIGGER session_turns_fts_delete AFTER DELETE ON session_turns BEGIN
	DELETE FROM session_turns_fts WHERE rowid = OLD.id;
END;

CREATE TRIGGER session_turns_fts_update
AFTER UPDATE OF canonical_user_id, session_id, session_generation, user_text, assistant_text ON session_turns BEGIN
	DELETE FROM session_turns_fts WHERE rowid = OLD.id;
	INSERT INTO session_turns_fts(rowid, canonical_user_id, session_id, session_generation, user_text, assistant_text)
	VALUES (NEW.id, NEW.canonical_user_id, NEW.session_id, NEW.session_generation, NEW.user_text, NEW.assistant_text);
END;

DELETE FROM session_turns_fts;
INSERT INTO session_turns_fts(rowid, canonical_user_id, session_id, session_generation, user_text, assistant_text)
SELECT id, canonical_user_id, session_id, session_generation, user_text, assistant_text FROM session_turns;
`)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such module: fts5") {
			return fmt.Errorf("%w: rebuild with the sqlite_fts5 build tag", ErrFTS5Unavailable)
		}
		return fmt.Errorf("create session transcript FTS5 index: %w", err)
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
