package database

import "fmt"

func (d *DB) initializeUserMemory() error {
	if _, err := d.db.Exec(`
CREATE TABLE IF NOT EXISTS user_memory_profiles (
	canonical_user_id TEXT PRIMARY KEY,
	intro TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS user_memory_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	category TEXT NOT NULL CHECK (category IN ('identity', 'system_rules', 'preferences', 'notes')),
	statement TEXT NOT NULL,
	statement_key TEXT NOT NULL,
	evidence TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	UNIQUE (canonical_user_id, statement_key)
);

CREATE INDEX IF NOT EXISTS idx_user_memory_entries_user_category
ON user_memory_entries (canonical_user_id, category);

CREATE INDEX IF NOT EXISTS idx_user_memory_entries_user_updated
ON user_memory_entries (canonical_user_id, updated_at);
`); err != nil {
		return fmt.Errorf("failed to initialize user memory tables: %w", err)
	}
	return nil
}
