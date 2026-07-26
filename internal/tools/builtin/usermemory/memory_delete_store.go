package usermemory

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ListedMemory is one active memory exposed to its owning user.
type ListedMemory struct {
	ID        int64
	Category  string
	Statement string
}

// UserDeletionScope identifies runtime state owned by a deleted account.
type UserDeletionScope struct {
	ExternalIdentities []string
	SessionIDs         []string
}

// ListActiveMemories returns every active, unexpired memory owned by a user.
func (s *Store) ListActiveMemories(ctx context.Context, userID string, now time.Time) ([]ListedMemory, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := requireActiveUser(ctx, tx, userID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, category, statement FROM memory_entries WHERE canonical_user_id = ? AND status = 'active' AND (expires_at IS NULL OR julianday(expires_at) > julianday(?)) ORDER BY id`, userID, formatTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	memories := make([]ListedMemory, 0)
	for rows.Next() {
		var memory ListedMemory
		if err := rows.Scan(&memory.ID, &memory.Category, &memory.Statement); err != nil {
			return nil, err
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}

// HardDeleteMemory permanently removes one durable memory and its memory-only artifacts.
func (s *Store) HardDeleteMemory(ctx context.Context, userID string, memoryID int64, now time.Time) error {
	if memoryID <= 0 {
		return fmt.Errorf("memory id must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := requireActiveUser(ctx, tx, userID); err != nil {
		return err
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM memory_entries WHERE canonical_user_id = ? AND id = ?`, userID, memoryID).Scan(&status); err != nil {
		return err
	}
	nowText := formatTime(now)
	if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET status = 'deleted', status_changed_at = ?, status_reason = 'hard_delete', updated_at = ? WHERE canonical_user_id = ? AND id = ?`, nowText, nowText, userID, memoryID); err != nil {
		return err
	}
	if err := rebindProfileCopiesTx(ctx, tx, userID, memoryID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET extraction_payload = '', source_request_id = '', source_session_id = '' WHERE job_kind = 'memory_formation' AND canonical_user_id = ? AND id IN (SELECT job_id FROM memory_events WHERE canonical_user_id = ? AND (memory_id = ? OR candidate_id IN (SELECT id FROM memory_candidates WHERE canonical_user_id = ? AND published_memory_id = ?)) AND job_id IS NOT NULL)`, userID, userID, memoryID, userID, memoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_events WHERE canonical_user_id = ? AND (memory_id = ? OR candidate_id IN (SELECT id FROM memory_candidates WHERE canonical_user_id = ? AND published_memory_id = ?))`, userID, memoryID, userID, memoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET supersedes_memory_id = NULL WHERE canonical_user_id = ? AND supersedes_memory_id = ? AND published_memory_id IS NOT ?`, userID, memoryID, memoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_candidates WHERE canonical_user_id = ? AND published_memory_id = ?`, userID, memoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM durable_jobs WHERE job_kind = 'derived_index' AND canonical_user_id = ? AND entity_kind = 'memory' AND entity_id = ?`, userID, memoryID); err != nil {
		return err
	}
	if err := deleteDerivedRowsTx(ctx, tx, "memory", []int64{memoryID}, userID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM memory_entries WHERE canonical_user_id = ? AND id = ?`, userID, memoryID)
	if err != nil {
		return err
	}
	if deleted, _ := result.RowsAffected(); deleted != 1 {
		return sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.signalDerivedIndex()
	return nil
}

// HardDeleteAllUserData removes all learned and conversational state while
// retaining the canonical account, linked identities, and authentication data.
func (s *Store) HardDeleteAllUserData(ctx context.Context, userID string, now time.Time) ([]string, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := requireActiveUser(ctx, tx, userID); err != nil {
		return nil, err
	}
	sessionRows, err := tx.QueryContext(ctx, `SELECT session_id FROM sessions WHERE canonical_user_id = ? ORDER BY session_id`, userID)
	if err != nil {
		return nil, err
	}
	sessionIDs, err := scanStringRows(sessionRows)
	if err != nil {
		return nil, err
	}
	nowText := formatTime(now)
	if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET status = 'deleted', status_changed_at = ?, status_reason = 'hard_delete', updated_at = ? WHERE canonical_user_id = ?`, nowText, nowText, userID); err != nil {
		return nil, err
	}
	if err := rebindProfileCopiesTx(ctx, tx, userID, 0, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM durable_jobs WHERE canonical_user_id = ?`, userID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_summaries WHERE canonical_user_id = ?`, userID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_events WHERE canonical_user_id = ?`, userID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_candidates WHERE canonical_user_id = ?`, userID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_entries WHERE canonical_user_id = ?`, userID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_turns WHERE canonical_user_id = ?`, userID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE scope = 'user' AND owner_user_id = ?`, userID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_link_challenges WHERE initiator_user_id = ? OR consumed_by_user_id = ? OR result_user_id = ? OR invalidated_by_user_id = ?`, userID, userID, userID, userID); err != nil {
		return nil, err
	}
	if err := deleteDerivedRowsTx(ctx, tx, "all", nil, userID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET generation = generation + 1, is_active = 0, started_at = ?, last_seen_at = ?, expires_at = ?, source_digest = '', rendered_content = speaker_intro, fact_count = 0, profile_bytes = length(CAST(speaker_intro AS BLOB)), source_memory_ids = '[]', profile_created_at = ? WHERE canonical_user_id = ?`, nowText, nowText, nowText, nowText, userID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.signalDerivedIndex()
	return sessionIDs, nil
}

// DeleteUserTx hard-deletes one account's canonical state in a caller-owned transaction.
func (s *Store) DeleteUserTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (UserDeletionScope, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var scope UserDeletionScope
	if _, err := tx.ExecContext(ctx, `UPDATE account_users SET lifecycle_state = 'erasing', updated_at = ? WHERE canonical_user_id = ? AND lifecycle_state = 'active'`, formatTime(now), userID); err != nil {
		return scope, err
	}
	accountRows, err := tx.QueryContext(ctx, `SELECT gateway || ':' || identifier FROM linked_accounts WHERE canonical_user_id = ? ORDER BY gateway, identifier`, userID)
	if err != nil {
		return scope, err
	}
	scope.ExternalIdentities, err = scanStringRows(accountRows)
	if err != nil {
		return scope, err
	}
	clientRows, err := tx.QueryContext(ctx, `SELECT 'websocket-client:' || client_id FROM websocket_clients WHERE canonical_user_id = ? ORDER BY client_id`, userID)
	if err != nil {
		return scope, err
	}
	clients, err := scanStringRows(clientRows)
	if err != nil {
		return scope, err
	}
	scope.ExternalIdentities = append(scope.ExternalIdentities, clients...)
	sessionRows, err := tx.QueryContext(ctx, `SELECT session_id FROM sessions WHERE canonical_user_id = ? ORDER BY session_id`, userID)
	if err != nil {
		return scope, err
	}
	scope.SessionIDs, err = scanStringRows(sessionRows)
	if err != nil {
		return scope, err
	}
	if err := deleteDerivedRowsTx(ctx, tx, "all", nil, userID); err != nil {
		return scope, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_link_challenges WHERE initiator_user_id = ? OR consumed_by_user_id = ? OR result_user_id = ? OR invalidated_by_user_id = ?`, userID, userID, userID, userID); err != nil {
		return scope, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE scope = 'user' AND owner_user_id = ?`, userID); err != nil {
		return scope, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE account_users SET banned_by = '' WHERE banned_by = ?`, userID); err != nil {
		return scope, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM account_users WHERE canonical_user_id = ? AND lifecycle_state = 'erasing'`, userID)
	if err != nil {
		return scope, err
	}
	if deleted, _ := result.RowsAffected(); deleted != 1 {
		return scope, sql.ErrNoRows
	}
	return scope, nil
}

func requireActiveUser(ctx context.Context, tx *sql.Tx, userID string) error {
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_users WHERE canonical_user_id = ? AND lifecycle_state = 'active'`, userID).Scan(&active); err != nil {
		return err
	}
	if active != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func deleteDerivedRowsTx(ctx context.Context, tx *sql.Tx, entityKind string, ids []int64, userID string) error {
	rows, err := tx.QueryContext(ctx, `
SELECT revisions.table_name, revisions.index_kind
FROM derived_index_revisions revisions
JOIN sqlite_master tables ON tables.type = 'table' AND tables.name = revisions.table_name
UNION SELECT name, 'memory_fts' FROM sqlite_master WHERE type = 'table' AND name = 'memory_entries_fts'
UNION SELECT name, 'transcript_fts' FROM sqlite_master WHERE type = 'table' AND name = 'session_turns_fts'
UNION SELECT name, 'memory_vector' FROM sqlite_master WHERE type = 'table' AND name = ?
ORDER BY 2, 1`, memoryVectorTableV2)
	if err != nil {
		return err
	}
	type target struct{ table, kind string }
	var targets []target
	for rows.Next() {
		var item target
		if err := rows.Scan(&item.table, &item.kind); err != nil {
			rows.Close()
			return err
		}
		if err := validateRevisionTable(item.table); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range targets {
		if item.kind == IndexKindGlobalMemoryFTS || item.kind == IndexKindGlobalMemoryVector {
			continue
		}
		if entityKind == "memory" && item.kind == IndexKindTranscriptFTS || entityKind == "session_turn" && item.kind != IndexKindTranscriptFTS {
			continue
		}
		if entityKind == "all" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+item.table+` WHERE canonical_user_id = ?`, userID); err != nil {
				return err
			}
			continue
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+item.table+` WHERE rowid = ? AND canonical_user_id = ?`, id, userID); err != nil {
				return err
			}
		}
	}
	return nil
}

func scanStringRows(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	values := make([]string, 0)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// ParseMemoryID parses an exact positive decimal stable ID.
func ParseMemoryID(value string) (int64, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.HasPrefix(value, "+") || (len(value) > 1 && value[0] == '0') {
		return 0, fmt.Errorf("invalid stable id")
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid stable id")
	}
	return id, nil
}
