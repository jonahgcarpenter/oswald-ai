package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// SessionFileMemory reads the frozen pair for an active session generation.
// A false bound value means neither file has been captured yet, including on
// existing sessions created before the snapshot migration.
func (s *Store) SessionFileMemory(ctx context.Context, userID, sessionID string, generation int) (user, notes string, bound bool, err error) {
	var userValue, notesValue sql.NullString
	err = s.sql.QueryRowContext(ctx, `SELECT file_user_snapshot, file_memory_snapshot FROM sessions
		WHERE canonical_user_id = ? AND session_id = ? AND generation = ? AND is_active = 1 AND expires_at > ?`,
		userID, sessionID, generation, formatTime(time.Now().UTC())).Scan(&userValue, &notesValue)
	if err != nil {
		return "", "", false, fmt.Errorf("load session file memory: %w", err)
	}
	if userValue.Valid != notesValue.Valid {
		return "", "", false, errors.New("incomplete session file memory snapshot")
	}
	return userValue.String, notesValue.String, userValue.Valid, nil
}

// BindSessionFileMemory atomically captures both files once in the exact active
// generation. Concurrent first requests receive the same winning pair.
func (s *Store) BindSessionFileMemory(ctx context.Context, userID, sessionID string, generation int, user, notes string) (string, string, error) {
	if !utf8.ValidString(user) || !utf8.ValidString(notes) || utf8.RuneCountInString(user) > 1375 || utf8.RuneCountInString(notes) > 2200 {
		return "", "", errors.New("invalid session file memory snapshot")
	}
	if _, err := s.sql.ExecContext(ctx, `UPDATE sessions SET file_user_snapshot = ?, file_memory_snapshot = ?
		WHERE canonical_user_id = ? AND session_id = ? AND generation = ? AND is_active = 1 AND expires_at > ?
		AND file_user_snapshot IS NULL AND file_memory_snapshot IS NULL`,
		user, notes, userID, sessionID, generation, formatTime(time.Now().UTC())); err != nil {
		return "", "", fmt.Errorf("bind session file memory: %w", err)
	}
	storedUser, storedNotes, bound, err := s.SessionFileMemory(ctx, userID, sessionID, generation)
	if err != nil {
		return "", "", err
	}
	if !bound {
		return "", "", errors.New("session file memory snapshot was not bound")
	}
	return storedUser, storedNotes, nil
}
