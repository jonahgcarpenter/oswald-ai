package usermemory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// PrivacyItem is one stable, tenant-owned row shown by privacy inspection.
type PrivacyItem struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	SessionID  string `json:"session_id,omitempty"`
	Generation int    `json:"generation,omitempty"`
}

// PrivacyPage is a deterministic bounded inspection page.
type PrivacyPage struct {
	Section string        `json:"section"`
	Page    int           `json:"page"`
	Items   []PrivacyItem `json:"items"`
	HasMore bool          `json:"has_more"`
}

// PrivacyChallenge describes a persisted confirmation challenge.
type PrivacyChallenge struct {
	OperationID string
	ExpiresAt   time.Time
}

// PrivacyConfirmation describes the committed confirmed operation.
type PrivacyConfirmation struct {
	OperationType      string
	DeletedUserID      string
	ExternalIdentities []string
	SessionIDs         []string
	AccountCount       int
}

// UserErasureInvalidation identifies runtime state invalidated by a committed erasure.
type UserErasureInvalidation struct {
	ExternalIdentities []string
	SessionIDs         []string
}

// PrivacyInvalidationEvent is one leased durable runtime invalidation.
type PrivacyInvalidationEvent struct {
	ID                 int64
	OperationID        string
	ExternalIdentities []string
	SessionIDs         []string
	CloseConnections   bool
	Attempts           int
}

// PrivacySessionIDs returns all session identifiers currently associated with a user.
func (s *Store) PrivacySessionIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT session_id FROM sessions WHERE canonical_user_id = ? ORDER BY session_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, err
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	return sessionIDs, rows.Err()
}

const privacyPageSize = 25

const privacyCloseInvalidationDelay = 30 * time.Second

// InspectPrivacy returns lifecycle metadata without exposing content.
func (s *Store) InspectPrivacy(ctx context.Context, userID, section string, page int) (PrivacyPage, error) {
	if page < 1 {
		page = 1
	}
	section = strings.ToLower(strings.TrimSpace(section))
	if section == "" {
		section = "all"
	}
	tx, err := s.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PrivacyPage{}, err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := requireActivePrivacyUser(ctx, tx, userID); err != nil {
		return PrivacyPage{}, err
	}
	var items []PrivacyItem
	queries := []struct{ section, query string }{
		{"memories", `SELECT CAST(id AS TEXT), status, '', 0 FROM memory_entries WHERE canonical_user_id = ?`},
		{"candidates", `SELECT CAST(id AS TEXT), CASE WHEN statement = '' AND decision_reason = 'privacy_delete' THEN 'deleted' ELSE state END, source_session_id, source_session_generation FROM memory_candidates WHERE canonical_user_id = ?`},
		{"sessions", `SELECT session_id || ':' || generation, CASE WHEN is_active = 1 THEN 'active' ELSE 'inactive' END, session_id, generation FROM sessions WHERE canonical_user_id = ?`},
	}
	for _, item := range queries {
		if section != "all" && section != item.section {
			continue
		}
		rows, queryErr := tx.QueryContext(ctx, item.query+` ORDER BY 1`, userID)
		if queryErr != nil {
			return PrivacyPage{}, queryErr
		}
		for rows.Next() {
			var result PrivacyItem
			if err := rows.Scan(&result.ID, &result.State, &result.SessionID, &result.Generation); err != nil {
				rows.Close()
				return PrivacyPage{}, err
			}
			result.Kind = item.section
			items = append(items, result)
		}
		if err := rows.Close(); err != nil {
			return PrivacyPage{}, err
		}
	}
	if section != "all" && section != "memories" && section != "candidates" && section != "sessions" {
		return PrivacyPage{}, fmt.Errorf("unknown privacy section %q", section)
	}
	start := (page - 1) * privacyPageSize
	if start > len(items) {
		start = len(items)
	}
	end := min(start+privacyPageSize, len(items))
	return PrivacyPage{Section: section, Page: page, Items: items[start:end], HasMore: end < len(items)}, nil
}

// ForgetMemory immediately removes one memory from every serving surface while
// retaining canonical content for the configured grace period.
func (s *Store) ForgetMemory(ctx context.Context, userID, actorHash string, memoryID int64, requestID string, now time.Time, policy config.RetentionPolicy) (string, error) {
	if memoryID <= 0 {
		return "", fmt.Errorf("memory id must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	status := ""
	err := s.withPrivacyTx(ctx, userID, func(tx *sql.Tx) error {
		externalIdentities, sessionIDs, err := privacyInvalidationScopeTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM memory_entries WHERE canonical_user_id = ? AND id = ?`, userID, memoryID).Scan(&current); err != nil {
			return err
		}
		if current == StatusDeleted || current == "forgotten" {
			status = current
			if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", memoryID, "delete", "forget:"+formatTime(now)); err != nil {
				return err
			}
			if err := recordCompletedPrivacyOperationTx(ctx, tx, userID, actorHash, requestID, "forget_memory", strconv.FormatInt(memoryID, 10), now); err != nil {
				return err
			}
			return enqueuePrivacyInvalidationTx(ctx, tx, requestID, externalIdentities, sessionIDs, false, now)
		}
		nowText := formatTime(now)
		if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET status = 'forgotten', forgotten_at = ?, hard_delete_after = ?, lifecycle_request_id = ?, valid_until = ?, invalidated_at = ?, invalidation_reason = 'privacy_forget', updated_at = ? WHERE canonical_user_id = ? AND id = ?`, nowText, formatTime(now.Add(policy.ForgottenContentGrace)), requestID, nowText, nowText, nowText, userID, memoryID); err != nil {
			return err
		}
		if err := deleteProfileCopiesTx(ctx, tx, userID, memoryID); err != nil {
			return err
		}
		if err := deleteDerivedRowsTx(ctx, tx, "memory", []int64{memoryID}, userID); err != nil {
			return err
		}
		if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", memoryID, "delete", "forget:"+nowText); err != nil {
			return err
		}
		status = "forgotten"
		if err := recordCompletedPrivacyOperationTx(ctx, tx, userID, actorHash, requestID, "forget_memory", strconv.FormatInt(memoryID, 10), now); err != nil {
			return err
		}
		return enqueuePrivacyInvalidationTx(ctx, tx, requestID, externalIdentities, sessionIDs, false, now)
	})
	if err == nil {
		s.signalDerivedIndex()
	}
	return status, err
}

// DeleteMemory irreversibly scrubs one canonical memory and linked artifacts.
func (s *Store) DeleteMemory(ctx context.Context, userID, actorHash string, memoryID int64, requestID string, now time.Time) (string, error) {
	if memoryID <= 0 {
		return "", fmt.Errorf("memory id must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	status := ""
	err := s.withPrivacyTx(ctx, userID, func(tx *sql.Tx) error {
		externalIdentities, sessionIDs, err := privacyInvalidationScopeTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM memory_entries WHERE canonical_user_id = ? AND id = ?`, userID, memoryID).Scan(&current); err != nil {
			return err
		}
		if current == StatusDeleted {
			status = StatusDeleted
			if err := deleteDerivedRowsTx(ctx, tx, "memory", []int64{memoryID}, userID); err != nil {
				return err
			}
			if err := recordCompletedPrivacyOperationTx(ctx, tx, userID, actorHash, requestID, "delete_memory", strconv.FormatInt(memoryID, 10), now); err != nil {
				return err
			}
			return enqueuePrivacyInvalidationTx(ctx, tx, requestID, externalIdentities, sessionIDs, false, now)
		}
		sourceTurns, err := relatedMemorySourceTurnsTx(ctx, tx, userID, memoryID)
		if err != nil {
			return err
		}
		if err := scrubMemoryTx(ctx, tx, userID, memoryID, requestID, now); err != nil {
			return err
		}
		for _, sourceTurn := range sourceTurns {
			if err := deleteSourceExchangeTx(ctx, tx, userID, sourceTurn, requestID); err != nil {
				return err
			}
		}
		status = StatusDeleted
		if err := recordCompletedPrivacyOperationTx(ctx, tx, userID, actorHash, requestID, "delete_memory", strconv.FormatInt(memoryID, 10), now); err != nil {
			return err
		}
		return enqueuePrivacyInvalidationTx(ctx, tx, requestID, externalIdentities, sessionIDs, false, now)
	})
	if err == nil {
		s.signalDerivedIndex()
	}
	return status, err
}

// DeleteCandidate scrubs one proposal and leaves a content-free rejected tombstone.
func (s *Store) DeleteCandidate(ctx context.Context, userID, actorHash string, candidateID int64, requestID string, now time.Time) error {
	if candidateID <= 0 {
		return fmt.Errorf("candidate id must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return s.withPrivacyTx(ctx, userID, func(tx *sql.Tx) error {
		externalIdentities, sessionIDs, err := privacyInvalidationScopeTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		var published sql.NullInt64
		var candidateTurn, memoryTurn sql.NullInt64
		turns := map[int64]struct{}{}
		if err := tx.QueryRowContext(ctx, `SELECT published_memory_id, source_turn_id FROM memory_candidates WHERE canonical_user_id = ? AND id = ?`, userID, candidateID).Scan(&published, &candidateTurn); err != nil {
			return err
		}
		if published.Valid {
			relatedTurns, err := relatedMemorySourceTurnsTx(ctx, tx, userID, published.Int64)
			if err != nil {
				return err
			}
			for _, turnID := range relatedTurns {
				turns[turnID] = struct{}{}
			}
			if err := tx.QueryRowContext(ctx, `SELECT source_turn_id FROM memory_entries WHERE canonical_user_id = ? AND id = ?`, userID, published.Int64).Scan(&memoryTurn); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err := scrubMemoryTx(ctx, tx, userID, published.Int64, requestID, now); err != nil {
				return err
			}
		}
		if err := scrubCandidateTx(ctx, tx, userID, candidateID, now); err != nil {
			return err
		}
		if candidateTurn.Valid {
			turns[candidateTurn.Int64] = struct{}{}
		}
		if memoryTurn.Valid {
			turns[memoryTurn.Int64] = struct{}{}
		}
		for turnID := range turns {
			if err := deleteSourceExchangeTx(ctx, tx, userID, turnID, requestID); err != nil {
				return err
			}
		}
		if err := recordCompletedPrivacyOperationTx(ctx, tx, userID, actorHash, requestID, "delete_candidate", strconv.FormatInt(candidateID, 10), now); err != nil {
			return err
		}
		return enqueuePrivacyInvalidationTx(ctx, tx, requestID, externalIdentities, sessionIDs, false, now)
	})
}

// DeleteSessionPrivacy deletes only the authenticated current session generation.
func (s *Store) DeleteSessionPrivacy(ctx context.Context, userID, actorHash, sessionID, requestID string, now time.Time) (int, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, fmt.Errorf("session id is required")
	}
	generation := 0
	err := s.withPrivacyTx(ctx, userID, func(tx *sql.Tx) error {
		externalIdentities, _, err := privacyInvalidationScopeTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `
SELECT generation FROM sessions WHERE canonical_user_id = ? AND session_id = ? AND is_active = 1
UNION ALL
SELECT generation FROM (
	SELECT MAX(generation) AS generation FROM (
		SELECT session_generation AS generation FROM session_turns WHERE canonical_user_id = ? AND session_id = ?
		UNION ALL SELECT session_generation FROM session_summaries WHERE canonical_user_id = ? AND session_id = ?
		UNION ALL SELECT session_generation FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ?
	)
) artifacts
WHERE generation IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM sessions WHERE canonical_user_id = ? AND session_id = ? AND is_active = 1
)
LIMIT 1`, userID, sessionID, userID, sessionID, userID, sessionID, userID, sessionID, userID, sessionID).Scan(&generation)
		if errors.Is(err, sql.ErrNoRows) {
			if err := recordCompletedPrivacyOperationTx(ctx, tx, userID, actorHash, requestID, "delete_session", sessionID, now); err != nil {
				return err
			}
			return enqueuePrivacyInvalidationTx(ctx, tx, requestID, externalIdentities, []string{sessionID}, false, now)
		}
		if err != nil {
			return err
		}
		if err := deleteSessionGenerationTx(ctx, tx, userID, sessionID, generation, requestID); err != nil {
			return err
		}
		if err := recordCompletedPrivacyOperationTx(ctx, tx, userID, actorHash, requestID, "delete_session", sessionID+":"+strconv.Itoa(generation), now); err != nil {
			return err
		}
		return enqueuePrivacyInvalidationTx(ctx, tx, requestID, externalIdentities, []string{sessionID}, false, now)
	})
	if err == nil {
		s.signalDerivedIndex()
	}
	return generation, err
}

// CreatePrivacyChallenge persists a hashed, identity-bound one-time challenge.
func (s *Store) CreatePrivacyChallenge(ctx context.Context, userID, actorHash, operationID, operationType, targetDigest, challengeHash string, now, expiresAt time.Time) (PrivacyChallenge, error) {
	if operationType != "delete_all_memories" && operationType != "delete_user" {
		return PrivacyChallenge{}, fmt.Errorf("operation does not require confirmation")
	}
	err := s.withPrivacyTx(ctx, userID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO privacy_operations(operation_id, idempotency_key, actor_hash, target_user_id, target_hash, operation_type, target_digest, challenge_hash, challenge_expires_at, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`, operationID, operationID, actorHash, userID, hashText(userID), operationType, targetDigest, challengeHash, formatTime(expiresAt), formatTime(now), formatTime(now))
		return err
	})
	return PrivacyChallenge{OperationID: operationID, ExpiresAt: expiresAt}, err
}

// ConfirmPrivacyChallenge atomically consumes a challenge and performs its mutation.
func (s *Store) ConfirmPrivacyChallenge(ctx context.Context, userID, actorHash, challengeHash, requestID string, now time.Time) (PrivacyConfirmation, error) {
	var confirmation PrivacyConfirmation
	expired := false
	err := s.withPrivacyTx(ctx, userID, func(tx *sql.Tx) error {
		var operationID, expires string
		err := tx.QueryRowContext(ctx, `SELECT operation_id, operation_type, challenge_expires_at FROM privacy_operations WHERE target_user_id = ? AND actor_hash = ? AND challenge_hash = ? AND status = 'pending' ORDER BY created_at DESC LIMIT 1`, userID, actorHash, challengeHash).Scan(&operationID, &confirmation.OperationType, &expires)
		if err != nil {
			return fmt.Errorf("privacy confirmation is invalid, expired, or already used")
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, expires)
		if err != nil || !expiresAt.After(now) {
			if _, updateErr := tx.ExecContext(ctx, `UPDATE privacy_operations SET status = 'expired', challenge_hash = '', challenge_expires_at = NULL, updated_at = ?, last_error_code = 'expired' WHERE operation_id = ? AND status = 'pending'`, formatTime(now), operationID); updateErr != nil {
				return updateErr
			}
			expired = true
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE privacy_operations SET status = 'running', started_at = ?, updated_at = ? WHERE operation_id = ? AND status = 'pending'`, formatTime(now), formatTime(now), operationID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("privacy confirmation is invalid, expired, or already used")
		}
		externalIdentities, sessionIDs, err := privacyInvalidationScopeTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		switch confirmation.OperationType {
		case "delete_all_memories":
			if err := deleteAllMemoriesTx(ctx, tx, userID, requestID, now); err != nil {
				return err
			}
			if err := enqueuePrivacyInvalidationTx(ctx, tx, operationID, externalIdentities, sessionIDs, false, now); err != nil {
				return err
			}
		case "delete_user":
			var err error
			confirmation.ExternalIdentities, confirmation.SessionIDs, confirmation.AccountCount, err = eraseUserTx(ctx, tx, userID, operationID, now)
			if err != nil {
				return err
			}
			confirmation.DeletedUserID = userID
			if err := enqueuePrivacyInvalidationTx(ctx, tx, operationID, confirmation.ExternalIdentities, confirmation.SessionIDs, true, now); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported pending privacy operation")
		}
		_, err = tx.ExecContext(ctx, `UPDATE privacy_operations SET status = 'completed', target_user_id = NULL, challenge_hash = '', challenge_expires_at = NULL, completed_at = ?, updated_at = ?, last_error_code = '' WHERE operation_id = ?`, formatTime(now), formatTime(now), operationID)
		return err
	})
	if err == nil && expired {
		return PrivacyConfirmation{}, fmt.Errorf("privacy confirmation is invalid, expired, or already used")
	}
	if err == nil {
		s.signalDerivedIndex()
	}
	return confirmation, err
}

// ExportPrivacy returns a stable JSON export produced from one read transaction.
func (s *Store) ExportPrivacy(ctx context.Context, userID string, exportedAt time.Time) ([]byte, error) {
	tx, err := s.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := requireActivePrivacyUser(ctx, tx, userID); err != nil {
		return nil, err
	}
	tables := []struct {
		name, query string
		args        []any
	}{
		{"user", `SELECT canonical_user_id, created_at, updated_at, is_admin, is_banned, banned_at, banned_by, ban_reason, lifecycle_state, speaker_intro FROM account_users WHERE canonical_user_id = ?`, []any{userID}},
		{"linked_accounts", `SELECT gateway, identifier, display_name, linked_at, verified FROM linked_accounts WHERE canonical_user_id = ? ORDER BY gateway, identifier`, []any{userID}},
		{"websocket_clients", `SELECT client_id, websocket_identifier, client_name, token_version, is_bootstrap, created_at, last_used_at, refresh_expires_at, revoked_at FROM websocket_clients WHERE canonical_user_id = ? ORDER BY created_at, client_id`, []any{userID}},
		{"memories", `SELECT * FROM memory_entries WHERE canonical_user_id = ? ORDER BY id`, []any{userID}},
		{"memory_events", `SELECT * FROM memory_events WHERE event_kind = 'lifecycle' AND canonical_user_id = ? ORDER BY id`, []any{userID}},
		{"candidates", `SELECT * FROM memory_candidates WHERE canonical_user_id = ? ORDER BY id`, []any{userID}},
		{"evidence", `SELECT * FROM memory_evidence WHERE canonical_user_id = ? ORDER BY id`, []any{userID}},
		{"formation_jobs", `SELECT * FROM durable_jobs WHERE job_kind = 'memory_formation' AND canonical_user_id = ? ORDER BY id`, []any{userID}},
		{"audit", `SELECT * FROM memory_formation_audit WHERE canonical_user_id = ? ORDER BY id`, []any{userID}},
		{"sessions", `SELECT * FROM sessions WHERE canonical_user_id = ? ORDER BY session_id`, []any{userID}},
		{"session_turns", `SELECT * FROM session_turns WHERE canonical_user_id = ? ORDER BY session_id, session_generation, id`, []any{userID}},
		{"session_summaries", `SELECT * FROM session_summaries WHERE canonical_user_id = ? ORDER BY session_id, session_generation, id`, []any{userID}},
		{"compaction_jobs", `SELECT * FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ? ORDER BY id`, []any{userID}},
		{"privacy_operations", `SELECT operation_id, operation_type, status, created_at, updated_at, started_at, completed_at, last_error_code FROM privacy_operations WHERE target_user_id = ? ORDER BY created_at, operation_id`, []any{userID}},
		{"derived_index_changes", `SELECT id, entity_kind, entity_id, operation, state, attempt_count, created_at, updated_at, completed_at, last_error_code FROM durable_jobs WHERE job_kind = 'derived_index' AND canonical_user_id = ? ORDER BY id`, []any{userID}},
		{"account_link_challenges", `SELECT id, initiator_user_id, initiator_gateway, initiator_identifier, created_at, expires_at, consumed_at, consumed_by_user_id, consumed_gateway, consumed_identifier, result_user_id, invalidated_at, invalidated_by_user_id, invalidated_reason FROM account_link_challenges WHERE initiator_user_id = ? OR consumed_by_user_id = ? OR result_user_id = ? OR invalidated_by_user_id = ? ORDER BY created_at, id`, []any{userID, userID, userID, userID}},
		{"mcp_servers", `SELECT id, scope, name, type, transport, enabled, created_at, updated_at, '[redacted]' AS endpoint FROM mcp_servers WHERE scope = 'user' AND owner_user_id = ? ORDER BY name, id`, []any{userID}},
		{"derived_index_health", `SELECT index_kind, provider, model, dimension, schema_version, revision, state, expected_count, indexed_count, created_at, updated_at FROM derived_index_revisions ORDER BY index_kind, revision`, nil},
	}
	payload := map[string]any{"schema": "oswald.user-export.v1", "exported_at": formatTime(exportedAt)}
	for _, table := range tables {
		rows, err := queryObjects(ctx, tx, table.query, table.args...)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", table.name, err)
		}
		payload[table.name] = rows
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// PrivacyExportPreflight rejects accounts whose content alone clearly exceeds a limit.
func (s *Store) PrivacyExportPreflight(ctx context.Context, userID string, limit int64) error {
	if limit <= 0 {
		return fmt.Errorf("privacy export size limit must be positive")
	}
	tx, err := s.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := requireActivePrivacyUser(ctx, tx, userID); err != nil {
		return err
	}
	var estimated int64
	err = tx.QueryRowContext(ctx, `
SELECT
	COALESCE((SELECT SUM(length(statement) + length(evidence)) FROM memory_entries WHERE canonical_user_id = ?), 0) +
	COALESCE((SELECT SUM(length(statement) + length(evidence_summary) + length(content_context)) FROM memory_candidates WHERE canonical_user_id = ?), 0) +
	COALESCE((SELECT SUM(length(content)) FROM memory_evidence WHERE canonical_user_id = ?), 0) +
	COALESCE((SELECT SUM(length(extraction_payload)) FROM durable_jobs WHERE job_kind = 'memory_formation' AND canonical_user_id = ?), 0) +
	COALESCE((SELECT SUM(length(metadata)) FROM memory_events WHERE canonical_user_id = ?), 0) +
	COALESCE((SELECT SUM(length(user_text) + length(assistant_text) + length(tool_names)) FROM session_turns WHERE canonical_user_id = ?), 0) +
	COALESCE((SELECT SUM(length(narrative) + length(open_tasks) + length(commitments) + length(entities) + length(decisions) + length(topic_tags)) FROM session_summaries WHERE canonical_user_id = ?), 0) +
	COALESCE((SELECT SUM(length(artifact_payload)) FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ?), 0)
`, userID, userID, userID, userID, userID, userID, userID, userID).Scan(&estimated)
	if err != nil {
		return fmt.Errorf("estimate privacy export size: %w", err)
	}
	if estimated > limit {
		return fmt.Errorf("privacy export content exceeds the %d-byte attachment limit before serialization", limit)
	}
	return nil
}

// RecordPrivacyExport records a successfully built content-free export lifecycle event.
func (s *Store) RecordPrivacyExport(ctx context.Context, userID, actorHash, requestID string, now time.Time) error {
	return s.withPrivacyTx(ctx, userID, func(tx *sql.Tx) error {
		return recordCompletedPrivacyOperationTx(ctx, tx, userID, actorHash, requestID, "export_user", "self", now)
	})
}

func (s *Store) withPrivacyTx(ctx context.Context, userID string, fn func(*sql.Tx) error) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := requireActivePrivacyUser(ctx, tx, userID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func requireActivePrivacyUser(ctx context.Context, tx *sql.Tx, userID string) error {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT lifecycle_state FROM account_users WHERE canonical_user_id = ?`, userID).Scan(&state); err != nil || state != "active" {
		return fmt.Errorf("privacy identity is stale or account is not active")
	}
	return nil
}

func relatedMemorySourceTurnsTx(ctx context.Context, tx *sql.Tx, userID string, memoryID int64) ([]int64, error) {
	return privacyIDsTx(ctx, tx, `
SELECT source_turn_id FROM memory_entries
WHERE canonical_user_id = ? AND id = ? AND source_turn_id IS NOT NULL
UNION
SELECT source_turn_id FROM memory_candidates
WHERE canonical_user_id = ? AND source_turn_id IS NOT NULL
	AND (published_memory_id = ? OR supersedes_memory_id = ? OR id IN (
		SELECT candidate_id FROM memory_entries WHERE canonical_user_id = ? AND id = ? AND candidate_id IS NOT NULL
	))
ORDER BY 1`, userID, memoryID, userID, memoryID, memoryID, userID, memoryID)
}

func scrubMemoryTx(ctx context.Context, tx *sql.Tx, userID string, memoryID int64, requestID string, now time.Time) error {
	nowText := formatTime(now)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM memory_candidates WHERE canonical_user_id = ? AND (published_memory_id = ? OR supersedes_memory_id = ? OR id IN (SELECT candidate_id FROM memory_entries WHERE id = ? AND candidate_id IS NOT NULL))`, userID, memoryID, memoryID, memoryID)
	if err != nil {
		return err
	}
	var candidateIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		candidateIDs = append(candidateIDs, id)
	}
	rows.Close()
	for _, id := range candidateIDs {
		if err := scrubCandidateTx(ctx, tx, userID, id, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence SET content = '', correlation_key = '', source_request_id = '', source_session_id = '', source_turn_id = NULL WHERE canonical_user_id = ? AND memory_id = ?`, userID, memoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_formation_audit SET request_id = '', session_id = '', actor_id = '', metadata = '', redacted_at = ? WHERE canonical_user_id = ? AND (memory_id = ? OR candidate_id IN (SELECT id FROM memory_candidates WHERE canonical_user_id = ? AND published_memory_id = ?))`, nowText, userID, memoryID, userID, memoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_events WHERE canonical_user_id = ? AND memory_id = ?`, userID, memoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET statement = '', statement_key = 'deleted:' || id, claim_key = 'deleted:' || id, claim_slot = '', claim_value = '', evidence = '', status = 'deleted', profile_approved = 0, embedding_model = '', embedding_dim = 0, candidate_id = NULL, source_session_id = '', source_request_id = '', source_turn_id = NULL, valid_until = ?, invalidated_at = ?, invalidation_reason = 'privacy_delete', erased_at = ?, erasure_reason = 'privacy_delete', erasure_request_id = ?, lifecycle_request_id = ?, forgotten_at = NULL, hard_delete_after = NULL, updated_at = ? WHERE canonical_user_id = ? AND id = ?`, nowText, nowText, nowText, requestID, requestID, nowText, userID, memoryID); err != nil {
		return err
	}
	if err := deleteProfileCopiesTx(ctx, tx, userID, memoryID); err != nil {
		return err
	}
	return deleteDerivedRowsTx(ctx, tx, "memory", []int64{memoryID}, userID)
}

func scrubCandidateTx(ctx context.Context, tx *sql.Tx, userID string, candidateID int64, now time.Time) error {
	nowText := formatTime(now)
	if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence SET content = '', correlation_key = '', source_request_id = '', source_session_id = '', source_turn_id = NULL WHERE canonical_user_id = ? AND candidate_id = ?`, userID, candidateID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET extraction_payload = '', source_request_id = '', source_session_id = '', source_turn_id = NULL WHERE job_kind = 'memory_formation' AND canonical_user_id = ? AND source_turn_id IN (SELECT source_turn_id FROM memory_candidates WHERE canonical_user_id = ? AND id = ?)`, userID, userID, candidateID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_formation_audit SET request_id = '', session_id = '', actor_id = '', metadata = '', redacted_at = ? WHERE canonical_user_id = ? AND candidate_id = ?`, nowText, userID, candidateID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET statement = '', statement_key = 'deleted:' || id, claim_key = 'deleted:' || id, claim_slot = '', claim_value = '', evidence_summary = '', state = 'rejected', source_request_id = '', source_session_id = '', source_turn_id = NULL, extraction_model = '', explicit_tool_source = '', confirmation_session_id = '', confirmation_request_id = '', published_memory_id = NULL, supersedes_memory_id = NULL, decision_reason = 'privacy_delete', decided_at = ?, decided_by = 'privacy', updated_at = ? WHERE canonical_user_id = ? AND id = ?`, nowText, nowText, userID, candidateID)
	return err
}

func deleteProfileCopiesTx(ctx context.Context, tx *sql.Tx, userID string, memoryID int64) error {
	return rebindProfileCopiesTx(ctx, tx, userID, memoryID, time.Now().UTC())
}

func deleteSourceExchangeTx(ctx context.Context, tx *sql.Tx, userID string, turnID int64, token string) error {
	var sessionID string
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT session_id, session_generation FROM session_turns WHERE canonical_user_id = ? AND id = ?`, userID, turnID).Scan(&sessionID, &generation); errors.Is(err, sql.ErrNoRows) {
		return deleteDerivedRowsTx(ctx, tx, "session_turn", []int64{turnID}, userID)
	} else if err != nil {
		return err
	}
	return deleteSessionTurnsTx(ctx, tx, userID, sessionID, generation, []int64{turnID}, token)
}

func deleteSessionGenerationTx(ctx context.Context, tx *sql.Tx, userID, sessionID string, generation int, token string) error {
	ids, err := privacyIDsTx(ctx, tx, `SELECT id FROM session_turns WHERE canonical_user_id = ? AND session_id = ? AND session_generation = ? ORDER BY id`, userID, sessionID, generation)
	if err != nil {
		return err
	}
	if err := deleteSessionTurnsTx(ctx, tx, userID, sessionID, generation, ids, token); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET is_active = 0, generation = generation + 1 WHERE canonical_user_id = ? AND session_id = ? AND generation = ?`, userID, sessionID, generation); err != nil {
		return err
	}
	return nil
}

func deleteSessionTurnsTx(ctx context.Context, tx *sql.Tx, userID, sessionID string, generation int, ids []int64, token string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ? AND session_generation = ?`, userID, sessionID, generation); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_summaries WHERE canonical_user_id = ? AND session_id = ? AND session_generation = ?`, userID, sessionID, generation); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET source_turn_id = NULL WHERE canonical_user_id = ? AND source_turn_id = ?`, userID, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET source_turn_id = NULL WHERE canonical_user_id = ? AND source_turn_id = ?`, userID, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence SET source_turn_id = NULL WHERE canonical_user_id = ? AND source_turn_id = ?`, userID, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET source_turn_id = NULL, extraction_payload = '' WHERE job_kind = 'memory_formation' AND canonical_user_id = ? AND source_turn_id = ?`, userID, id); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_turns WHERE canonical_user_id = ? AND session_id = ? AND session_generation = ? AND id = ?`, userID, sessionID, generation, id); err != nil {
			return err
		}
	}
	return deleteDerivedRowsTx(ctx, tx, "session_turn", ids, userID)
}

func deleteAllMemoriesTx(ctx context.Context, tx *sql.Tx, userID, requestID string, now time.Time) error {
	sourceTurns, err := privacyIDsTx(ctx, tx, `SELECT source_turn_id FROM memory_entries WHERE canonical_user_id = ? AND source_turn_id IS NOT NULL UNION SELECT source_turn_id FROM memory_candidates WHERE canonical_user_id = ? AND source_turn_id IS NOT NULL ORDER BY 1`, userID, userID)
	if err != nil {
		return err
	}
	nowText := formatTime(now)
	if _, err := tx.ExecContext(ctx, `UPDATE memory_formation_audit SET request_id = '', session_id = '', actor_id = '', metadata = '', redacted_at = ? WHERE canonical_user_id = ?`, nowText, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence SET content = '', correlation_key = '', source_request_id = '', source_session_id = '', source_turn_id = NULL WHERE canonical_user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_events SET request_id = '', session_id = '', metadata = '' WHERE canonical_user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM durable_jobs WHERE job_kind = 'memory_formation' AND canonical_user_id = ?`, userID); err != nil {
		return err
	}
	ids, err := privacyIDsTx(ctx, tx, `SELECT id FROM memory_entries WHERE canonical_user_id = ? ORDER BY id`, userID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := scrubMemoryTx(ctx, tx, userID, id, requestID, now); err != nil {
			return err
		}
	}
	candidates, err := privacyIDsTx(ctx, tx, `SELECT id FROM memory_candidates WHERE canonical_user_id = ? ORDER BY id`, userID)
	if err != nil {
		return err
	}
	for _, id := range candidates {
		if err := scrubCandidateTx(ctx, tx, userID, id, now); err != nil {
			return err
		}
	}
	for _, turnID := range sourceTurns {
		if err := deleteSourceExchangeTx(ctx, tx, userID, turnID, requestID); err != nil {
			return err
		}
	}
	return rebindProfileCopiesTx(ctx, tx, userID, 0, now)
}

func eraseUserTx(ctx context.Context, tx *sql.Tx, userID, operationID string, now time.Time) ([]string, []string, int, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE account_users SET lifecycle_state = 'erasing', updated_at = ? WHERE canonical_user_id = ? AND lifecycle_state = 'active'`, formatTime(now), userID); err != nil {
		return nil, nil, 0, err
	}
	accountRows, err := tx.QueryContext(ctx, `SELECT gateway, identifier FROM linked_accounts WHERE canonical_user_id = ? ORDER BY gateway, identifier`, userID)
	if err != nil {
		return nil, nil, 0, err
	}
	var externalIdentities []string
	for accountRows.Next() {
		var gateway, identifier string
		if err := accountRows.Scan(&gateway, &identifier); err != nil {
			accountRows.Close()
			return nil, nil, 0, err
		}
		externalIdentities = append(externalIdentities, gateway+":"+identifier)
	}
	if err := accountRows.Close(); err != nil {
		return nil, nil, 0, err
	}
	clientRows, err := tx.QueryContext(ctx, `SELECT client_id FROM websocket_clients WHERE canonical_user_id = ? ORDER BY client_id`, userID)
	if err != nil {
		return nil, nil, 0, err
	}
	for clientRows.Next() {
		var clientID string
		if err := clientRows.Scan(&clientID); err != nil {
			clientRows.Close()
			return nil, nil, 0, err
		}
		externalIdentities = append(externalIdentities, "websocket-client:"+clientID)
	}
	if err := clientRows.Close(); err != nil {
		return nil, nil, 0, err
	}
	sessionRows, err := tx.QueryContext(ctx, `SELECT session_id FROM sessions WHERE canonical_user_id = ? ORDER BY session_id`, userID)
	if err != nil {
		return nil, nil, 0, err
	}
	var sessions []string
	for sessionRows.Next() {
		var id string
		if err := sessionRows.Scan(&id); err != nil {
			sessionRows.Close()
			return nil, nil, 0, err
		}
		sessions = append(sessions, id)
	}
	sessionRows.Close()
	var accounts int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM linked_accounts WHERE canonical_user_id = ?`, userID).Scan(&accounts); err != nil {
		return nil, nil, 0, err
	}
	if err := deleteDerivedRowsTx(ctx, tx, "all", nil, userID); err != nil {
		return nil, nil, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM durable_jobs WHERE job_kind = 'derived_index' AND canonical_user_id = ?`, userID); err != nil {
		return nil, nil, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_formation_audit WHERE canonical_user_id = ?`, userID); err != nil {
		return nil, nil, 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE privacy_operations SET
		status = CASE WHEN operation_id = ? AND ? != '' THEN 'completed' WHEN status = 'pending' THEN 'expired' WHEN status = 'running' THEN 'failed' ELSE status END,
		target_user_id = NULL, challenge_hash = '', challenge_expires_at = NULL,
		completed_at = COALESCE(completed_at, ?), updated_at = ?,
		last_error_code = CASE WHEN operation_id = ? AND ? != '' THEN '' WHEN status = 'pending' THEN 'account_erased' WHEN status = 'running' THEN 'account_erased' ELSE last_error_code END
		WHERE target_user_id = ?`, operationID, operationID, formatTime(now), formatTime(now), operationID, operationID, userID); err != nil {
		return nil, nil, 0, err
	}
	if operationID != "" {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM privacy_operations WHERE operation_id = ?`, operationID).Scan(&status); err != nil || status != "completed" {
			if err == nil {
				err = fmt.Errorf("privacy erasure operation was not completed")
			}
			return nil, nil, 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE privacy_operations SET target_user_id = NULL, challenge_hash = '', challenge_expires_at = NULL WHERE target_user_id = ?`, userID); err != nil {
		return nil, nil, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_link_challenges WHERE initiator_user_id = ? OR consumed_by_user_id = ? OR result_user_id = ? OR invalidated_by_user_id = ?`, userID, userID, userID, userID); err != nil {
		return nil, nil, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE scope = 'user' AND owner_user_id = ?`, userID); err != nil {
		return nil, nil, 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE account_users SET banned_by = '' WHERE banned_by = ?`, userID); err != nil {
		return nil, nil, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_users WHERE canonical_user_id = ? AND lifecycle_state = 'erasing'`, userID); err != nil {
		return nil, nil, 0, err
	}
	return externalIdentities, sessions, accounts, nil
}

// EraseUserTx performs exhaustive trigger-safe canonical erasure in a caller-owned transaction.
// It is used by administrator deletion so that admin and self-service erasure have identical scope.
func (s *Store) EraseUserTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (UserErasureInvalidation, error) {
	return s.EraseUserWithInvalidationTx(ctx, tx, userID, "erase-user:"+hashText(userID)+":"+formatTime(now), now)
}

// EraseUserWithInvalidationTx erases a user and durably queues its pre-cascade runtime scope.
func (s *Store) EraseUserWithInvalidationTx(ctx context.Context, tx *sql.Tx, userID, operationID string, now time.Time) (UserErasureInvalidation, error) {
	var invalidation UserErasureInvalidation
	var err error
	invalidation.ExternalIdentities, invalidation.SessionIDs, _, err = eraseUserTx(ctx, tx, userID, "", now)
	if err == nil {
		err = enqueuePrivacyInvalidationTx(ctx, tx, operationID, invalidation.ExternalIdentities, invalidation.SessionIDs, true, now)
	}
	return invalidation, err
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
	rows.Close()
	for _, item := range targets {
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

func privacyIDsTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func queryObjects(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]map[string]any, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		item := make(map[string]any, len(columns))
		for i, value := range values {
			if bytes, ok := value.([]byte); ok {
				value = string(bytes)
			}
			item[columns[i]] = value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func recordCompletedPrivacyOperationTx(ctx context.Context, tx *sql.Tx, userID, actorHash, operationID, operationType, target string, now time.Time) error {
	if len(actorHash) != 64 || strings.TrimSpace(operationID) == "" {
		return fmt.Errorf("privacy operation identity is invalid")
	}
	nowText := formatTime(now)
	targetHash := hashText(userID)
	targetDigest := hashText(operationType + "\x00" + target)
	if _, err := tx.ExecContext(ctx, `INSERT INTO privacy_operations(operation_id, idempotency_key, actor_hash, target_user_id, target_hash, operation_type, target_digest, status, created_at, updated_at, started_at, completed_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'completed', ?, ?, ?, ?) ON CONFLICT DO NOTHING`, operationID, operationID, actorHash, userID, targetHash, operationType, targetDigest, nowText, nowText, nowText, nowText); err != nil {
		return err
	}
	var storedOperationID, storedKey, storedActor, storedTargetHash, storedType, storedDigest, status string
	err := tx.QueryRowContext(ctx, `SELECT operation_id, idempotency_key, actor_hash, target_hash, operation_type, target_digest, status FROM privacy_operations WHERE operation_id = ? OR (actor_hash = ? AND idempotency_key = ?) ORDER BY operation_id = ? DESC LIMIT 1`, operationID, actorHash, operationID, operationID).Scan(&storedOperationID, &storedKey, &storedActor, &storedTargetHash, &storedType, &storedDigest, &status)
	if err != nil {
		return err
	}
	if storedOperationID != operationID || storedKey != operationID || storedActor != actorHash || storedTargetHash != targetHash || storedType != operationType || storedDigest != targetDigest || status != "completed" {
		return fmt.Errorf("privacy operation idempotency payload mismatch")
	}
	return nil
}

func privacyInvalidationScopeTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, []string, error) {
	accountRows, err := tx.QueryContext(ctx, `SELECT gateway || ':' || identifier FROM linked_accounts WHERE canonical_user_id = ? ORDER BY gateway, identifier`, userID)
	if err != nil {
		return nil, nil, err
	}
	externalIdentities, err := scanStrings(accountRows)
	if err != nil {
		return nil, nil, err
	}
	sessionRows, err := tx.QueryContext(ctx, `SELECT session_id FROM sessions WHERE canonical_user_id = ? ORDER BY session_id`, userID)
	if err != nil {
		return nil, nil, err
	}
	sessionIDs, err := scanStrings(sessionRows)
	return externalIdentities, sessionIDs, err
}

func scanStrings(rows *sql.Rows) ([]string, error) {
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

func enqueuePrivacyInvalidationTx(ctx context.Context, tx *sql.Tx, operationID string, externalIdentities, sessionIDs []string, closeConnections bool, now time.Time) error {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return fmt.Errorf("privacy invalidation operation id is required")
	}
	externalJSON, err := json.Marshal(externalIdentities)
	if err != nil {
		return err
	}
	sessionJSON, err := json.Marshal(sessionIDs)
	if err != nil {
		return err
	}
	// nil and empty scopes have the same canonical representation.
	if string(externalJSON) == "null" {
		externalJSON = []byte("[]")
	}
	if string(sessionJSON) == "null" {
		sessionJSON = []byte("[]")
	}
	nowText := formatTime(now)
	availableAt := now
	closeValue := 0
	if closeConnections {
		closeValue = 1
		// The command path publishes this invalidation immediately after delivering
		// its confirmation. Delay crash-recovery dispatch so it cannot close the
		// initiating WebSocket before that response is written.
		availableAt = now.Add(privacyCloseInvalidationDelay)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, privacy_operation_id, external_identities, session_ids, close_connections, state, attempt_count, available_at, created_at, updated_at) VALUES ('privacy_invalidation', ?, NULL, ?, ?, ?, ?, 'queued', 0, ?, ?, ?) ON CONFLICT(job_kind, idempotency_key) DO NOTHING`, operationID, operationID, string(externalJSON), string(sessionJSON), closeValue, formatTime(availableAt), nowText, nowText)
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted == 1 {
		return nil
	}
	var storedOperation, storedExternal, storedSessions, state string
	var storedClose int
	if err := tx.QueryRowContext(ctx, `SELECT privacy_operation_id, external_identities, session_ids, close_connections, state FROM durable_jobs WHERE job_kind = 'privacy_invalidation' AND idempotency_key = ?`, operationID).Scan(&storedOperation, &storedExternal, &storedSessions, &storedClose, &state); err != nil {
		return err
	}
	if storedOperation != operationID || storedClose != closeValue {
		return fmt.Errorf("privacy invalidation idempotency payload mismatch")
	}
	if state != "succeeded" && (storedExternal != string(externalJSON) || storedSessions != string(sessionJSON)) {
		return fmt.Errorf("privacy invalidation idempotency payload mismatch")
	}
	return nil
}

// ReconcilePrivacyInvalidationLeases makes expired work available after a crash.
func (s *Store) ReconcilePrivacyInvalidationLeases(ctx context.Context, now time.Time) (int64, error) {
	nowText := formatTime(now.UTC())
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'retry', lease_until = NULL, available_at = ?, updated_at = ?, last_error_code = 'lease_expired' WHERE job_kind = 'privacy_invalidation' AND state = 'running' AND julianday(lease_until) <= julianday(?)`, nowText, nowText, nowText)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ClaimPrivacyInvalidation leases the oldest available event.
func (s *Store) ClaimPrivacyInvalidation(ctx context.Context, now time.Time, lease time.Duration) (*PrivacyInvalidationEvent, error) {
	if lease <= 0 {
		return nil, fmt.Errorf("privacy invalidation lease must be positive")
	}
	if _, err := s.ReconcilePrivacyInvalidationLeases(ctx, now); err != nil {
		return nil, err
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // nolint:errcheck
	var event PrivacyInvalidationEvent
	var externalJSON, sessionJSON string
	err = tx.QueryRowContext(ctx, `SELECT id, privacy_operation_id, external_identities, session_ids, close_connections, attempt_count FROM durable_jobs WHERE job_kind = 'privacy_invalidation' AND state IN ('queued','retry') AND julianday(available_at) <= julianday(?) ORDER BY julianday(available_at), id LIMIT 1`, formatTime(now.UTC())).Scan(&event.ID, &event.OperationID, &externalJSON, &sessionJSON, &event.CloseConnections, &event.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET state = 'running', attempt_count = attempt_count + 1, lease_until = ?, updated_at = ?, last_error_code = '' WHERE id = ? AND job_kind = 'privacy_invalidation' AND state IN ('queued','retry')`, formatTime(now.UTC().Add(lease)), formatTime(now.UTC()), event.ID)
	if err != nil {
		return nil, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(externalJSON), &event.ExternalIdentities); err != nil {
		return nil, fmt.Errorf("decode privacy invalidation identities: %w", err)
	}
	if err := json.Unmarshal([]byte(sessionJSON), &event.SessionIDs); err != nil {
		return nil, fmt.Errorf("decode privacy invalidation sessions: %w", err)
	}
	event.Attempts++
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &event, nil
}

// RetryPrivacyInvalidation releases a failed event for a later attempt.
func (s *Store) RetryPrivacyInvalidation(ctx context.Context, id int64, availableAt, now time.Time, errorCode string) error {
	if strings.TrimSpace(errorCode) == "" {
		errorCode = "publish_failed"
	}
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'retry', available_at = ?, lease_until = NULL, updated_at = ?, last_error_code = ? WHERE id = ? AND job_kind = 'privacy_invalidation' AND state = 'running'`, formatTime(availableAt.UTC()), formatTime(now.UTC()), errorCode, id)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("privacy invalidation lease is no longer active")
	}
	return nil
}

// CompletePrivacyInvalidation atomically completes an event and scrubs its payload.
func (s *Store) CompletePrivacyInvalidation(ctx context.Context, id int64, now time.Time) error {
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET external_identities = '[]', session_ids = '[]', state = 'succeeded', lease_until = NULL, completed_at = ?, updated_at = ?, last_error_code = '' WHERE id = ? AND job_kind = 'privacy_invalidation' AND state = 'running'`, formatTime(now.UTC()), formatTime(now.UTC()), id)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("privacy invalidation lease is no longer active")
	}
	return nil
}

// ParsePrivacyID parses an exact positive decimal stable ID.
func ParsePrivacyID(value string) (int64, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.HasPrefix(value, "+") || (len(value) > 1 && value[0] == '0') {
		return 0, fmt.Errorf("invalid stable id")
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid stable id")
	}
	return id, nil
}
