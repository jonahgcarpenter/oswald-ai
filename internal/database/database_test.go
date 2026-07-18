package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestAccountLinkChallengesSchema(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close() // nolint:errcheck

	_, err = db.SQL().Exec(`
INSERT INTO account_link_challenges (
	id, code_hash, initiator_user_id, initiator_gateway, initiator_identifier,
	created_at, expires_at, consumed_at, consumed_gateway, consumed_identifier,
	consumed_by_user_id, result_user_id, invalidated_at, invalidated_by_user_id, invalidated_reason
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"challenge-1", "hash-1", "user-1", "discord", "account-1",
		"2026-07-18T12:00:00Z", "2026-07-18T12:10:00Z", nil, nil, nil,
		nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("insert challenge: %v", err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO account_link_challenges (
	id, code_hash, initiator_user_id, initiator_gateway, initiator_identifier, created_at, expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"challenge-2", "hash-1", "user-2", "websocket", "account-2",
		"2026-07-18T12:00:00Z", "2026-07-18T12:10:00Z",
	); err == nil {
		t.Fatal("expected duplicate code_hash to fail")
	}

	rows, err := db.SQL().Query(`PRAGMA foreign_key_list(account_link_challenges)`)
	if err != nil {
		t.Fatalf("inspect foreign keys: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("account_link_challenges must not have foreign keys")
	}

	for _, name := range []string{"idx_account_link_challenges_expiry", "idx_account_link_challenges_initiator_state"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&count); err != nil {
			t.Fatalf("inspect index %s: %v", name, err)
		}
		if count != 1 {
			t.Fatalf("expected index %s", name)
		}
	}
}

func TestWithTxCommitsAndRollsBack(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close() // nolint:errcheck

	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, "committed", "2026-07-18T12:00:00Z", "2026-07-18T12:00:00Z")
		return err
	})
	if err != nil {
		t.Fatalf("commit transaction: %v", err)
	}

	sentinel := errors.New("rollback")
	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, "rolled-back", "2026-07-18T12:00:00Z", "2026-07-18T12:00:00Z"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected callback error, got %v", err)
	}

	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM account_users WHERE canonical_user_id IN ('committed', 'rolled-back')`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one committed user, got %d", count)
	}
}

func TestUserMemoryMigrationAddsProfilesAndDemotesSystemRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`
CREATE TABLE account_users (canonical_user_id TEXT PRIMARY KEY, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE memory_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	scope TEXT NOT NULL,
	category TEXT NOT NULL CHECK (category IN ('identity', 'system_rules', 'communication_preferences', 'durable_preferences', 'projects', 'relationships', 'environment', 'notes')),
	statement TEXT NOT NULL,
	statement_key TEXT NOT NULL,
	evidence TEXT NOT NULL,
	confidence REAL NOT NULL DEFAULT 0.8,
	importance INTEGER NOT NULL DEFAULT 3,
	status TEXT NOT NULL DEFAULT 'active',
	source_session_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	last_used_at TEXT,
	expires_at TEXT,
	supersedes_id INTEGER,
	embedding_model TEXT NOT NULL DEFAULT '',
	embedding_dim INTEGER NOT NULL DEFAULT 0,
	UNIQUE (canonical_user_id, scope, statement_key)
);
CREATE TABLE session_turns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	canonical_user_id TEXT NOT NULL,
	user_text TEXT NOT NULL,
	assistant_text TEXT NOT NULL,
	tool_names TEXT NOT NULL DEFAULT '',
	importance INTEGER NOT NULL DEFAULT 2,
	topic_tags TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	expires_at TEXT
);
INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES ('user', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO memory_entries (canonical_user_id, scope, category, statement, statement_key, evidence, created_at, updated_at) VALUES ('user', 'long_term', 'system_rules', 'Prefer concise replies.', 'prefer concise replies.', 'legacy', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO session_turns (session_id, canonical_user_id, user_text, assistant_text, created_at) VALUES ('session', 'user', 'hello', 'hi', '2026-01-01T00:00:00Z');
`)
	if err != nil {
		raw.Close() // nolint:errcheck
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	defer db.Close() // nolint:errcheck
	var category string
	var approved, generation int
	if err := db.SQL().QueryRow(`SELECT category, profile_approved FROM memory_entries WHERE canonical_user_id = 'user'`).Scan(&category, &approved); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT session_generation FROM session_turns WHERE canonical_user_id = 'user'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if category != "communication_preferences" || approved != 1 || generation != 1 {
		t.Fatalf("category=%q approved=%d generation=%d", category, approved, generation)
	}
	for _, table := range []string{"tenant_profile_versions", "tenant_profile_version_facts", "tenant_sessions"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("profile table %s count=%d err=%v", table, count, err)
		}
	}
	if err := db.initializeUserMemory(); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
}
