package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSessionFileMemoryMigrationPreservesExistingSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	old := &DB{path: path, db: raw}
	migrations := orderedMigrations()
	if len(migrations) != 16 || migrations[15].name != "v4.0.15" {
		t.Fatalf("unexpected migration registry: %+v", migrations)
	}
	if err := old.runSchemaMigrations(context.Background(), migrations[:15]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO account_users(canonical_user_id) VALUES('user');
		INSERT INTO sessions(canonical_user_id, session_id, generation, is_active, last_seen_at, expires_at,
		profile_version, profile_version_high_water, renderer_version, source_digest, speaker_intro, rendered_content)
		VALUES('user', 'existing', 3, 1, '2026-09-26T00:00:00Z', '2027-09-26T00:00:00Z', 1, 1, 'session-context-v1', '', '', '')`); err != nil {
		t.Fatal(err)
	}
	if err := old.runSchemaMigrations(context.Background(), migrations); err != nil {
		t.Fatal(err)
	}
	var user, notes sql.NullString
	var generation int
	if err := raw.QueryRow(`SELECT generation, file_user_snapshot, file_memory_snapshot FROM sessions WHERE session_id='existing'`).Scan(&generation, &user, &notes); err != nil || generation != 3 || user.Valid || notes.Valid {
		t.Fatalf("old session snapshot: generation=%d user=%+v notes=%+v err=%v", generation, user, notes, err)
	}
	if _, err := raw.Exec(`UPDATE sessions SET file_user_snapshot='', file_memory_snapshot='saved' WHERE session_id='existing'`); err != nil {
		t.Fatal(err)
	}
	if err := old.runSchemaMigrations(context.Background(), migrations); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT file_user_snapshot, file_memory_snapshot FROM sessions WHERE session_id='existing'`).Scan(&user, &notes); err != nil || !user.Valid || user.String != "" || notes.String != "saved" {
		t.Fatalf("reopen snapshot: user=%+v notes=%+v err=%v", user, notes, err)
	}
	var invalid int
	if err := raw.QueryRow(`PRAGMA foreign_key_check`).Scan(&invalid); err != sql.ErrNoRows {
		t.Fatalf("foreign key check: %v", err)
	}
}
