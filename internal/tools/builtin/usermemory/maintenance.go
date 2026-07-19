package usermemory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// MaintenanceCounts contains privacy-safe aggregate results from one sweep.
type MaintenanceCounts struct {
	SessionCleanup           SessionCleanupCounts `json:"session_cleanup"`
	ForgottenMemories        int64                `json:"forgotten_memories"`
	AuditRowsRedacted        int64                `json:"audit_rows_redacted"`
	FormationJobsRedacted    int64                `json:"formation_jobs_redacted"`
	CompactionJobsRedacted   int64                `json:"compaction_jobs_redacted"`
	CandidatesRedacted       int64                `json:"candidates_redacted"`
	EvidenceRowsRedacted     int64                `json:"evidence_rows_redacted"`
	EventsRedacted           int64                `json:"events_redacted"`
	AuditTombstones          int64                `json:"audit_tombstones_deleted"`
	MemoryTombstonesDeleted  int64                `json:"memory_tombstones_deleted"`
	CandidateTombstones      int64                `json:"candidate_tombstones_deleted"`
	EventTombstones          int64                `json:"event_tombstones_deleted"`
	PrivacyTombstones        int64                `json:"privacy_tombstones_deleted"`
	InvalidationTombstones   int64                `json:"privacy_invalidation_tombstones_deleted"`
	PrivacyChallengesExpired int64                `json:"privacy_challenges_expired"`
	FormationJobsDeleted     int64                `json:"formation_jobs_deleted"`
	CompactionJobsDeleted    int64                `json:"compaction_jobs_deleted"`
	ChallengesDeleted        int64                `json:"account_challenges_deleted"`
	IndexRowsDeleted         int64                `json:"index_rows_deleted"`
	IndexRevisionsDegraded   int64                `json:"index_revisions_degraded"`
	IndexTablesDropped       int64                `json:"index_tables_dropped"`
	OptimizeRun              bool                 `json:"optimize_run"`
}

// Changed returns the number of rows changed, excluding database hygiene.
func (c MaintenanceCounts) Changed() int64 {
	s := c.SessionCleanup
	return s.SessionTurnsDeleted + s.TenantSessionsDeleted + s.ProfileVersionsDeleted + s.MemoryEntriesExpired + s.CandidatesErased + s.FormationJobsDeleted + s.SessionSummariesDeleted + s.CompactionJobsDeleted +
		c.ForgottenMemories + c.AuditRowsRedacted + c.FormationJobsRedacted + c.CompactionJobsRedacted + c.CandidatesRedacted + c.EvidenceRowsRedacted + c.EventsRedacted + c.AuditTombstones + c.MemoryTombstonesDeleted + c.CandidateTombstones + c.EventTombstones + c.PrivacyChallengesExpired + c.PrivacyTombstones + c.InvalidationTombstones + c.FormationJobsDeleted + c.CompactionJobsDeleted + c.ChallengesDeleted + c.IndexRowsDeleted + c.IndexRevisionsDegraded + c.IndexTablesDropped
}

// MaintenanceSweep performs one bounded, serialized retention and consistency
// pass. It is safe to repeat at the same timestamp.
func (s *Store) MaintenanceSweep(ctx context.Context, now time.Time, policy config.RetentionPolicy) (counts MaintenanceCounts, err error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	policy = normalizedMaintenancePolicy(policy)
	runID, runErr := s.startMaintenanceRun(ctx, now)
	if runErr != nil {
		return counts, runErr
	}
	defer func() {
		state, code := "completed", ""
		if err != nil {
			state, code = "failed", "maintenance_failed"
		}
		_ = s.finishMaintenanceRun(context.Background(), runID, state, code, counts, time.Now().UTC())
	}()
	if err := maintenanceForeignKeyCheckDB(ctx, s.sql); err != nil {
		return counts, err
	}

	counts.SessionCleanup, err = s.cleanupExpiredSessions(ctx, now, policy, true)
	if err != nil {
		return counts, err
	}

	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return counts, fmt.Errorf("begin maintenance sweep: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	if err := maintenanceForeignKeyCheck(ctx, tx); err != nil {
		return counts, err
	}

	batch := policy.BatchSize
	nowText := formatTime(now)
	contentCutoff := formatTime(now.Add(-policy.ContentBearingAuditJobRetention))
	tombstoneCutoff := formatTime(now.Add(-policy.ContentFreeTombstoneRetention))
	candidateCutoff := formatTime(now.Add(-policy.CandidateContentRetention))

	rows, err := tx.QueryContext(ctx, `SELECT id, canonical_user_id, source_turn_id, COALESCE(NULLIF(lifecycle_request_id, ''), 'maintenance-retention'), hard_delete_after FROM memory_entries WHERE status = 'forgotten' AND hard_delete_after IS NOT NULL ORDER BY julianday(hard_delete_after), hard_delete_after, id LIMIT ?`, batch)
	if err != nil {
		return counts, err
	}
	type forgottenRow struct {
		id         int64
		userID     string
		sourceTurn sql.NullInt64
		requestID  string
		hardDelete string
	}
	var forgotten []forgottenRow
	for rows.Next() {
		var row forgottenRow
		if err := rows.Scan(&row.id, &row.userID, &row.sourceTurn, &row.requestID, &row.hardDelete); err != nil {
			rows.Close()
			return counts, err
		}
		if !parseTime(row.hardDelete).After(now) {
			forgotten = append(forgotten, row)
		}
	}
	if err := rows.Close(); err != nil {
		return counts, err
	}
	for _, row := range forgotten {
		sourceTurns, sourceErr := relatedMemorySourceTurnsTx(ctx, tx, row.userID, row.id)
		if sourceErr != nil {
			return counts, fmt.Errorf("enumerate forgotten memory source exchanges: %w", sourceErr)
		}
		if err := scrubMemoryTx(ctx, tx, row.userID, row.id, row.requestID, now); err != nil {
			return counts, fmt.Errorf("erase due forgotten memory: %w", err)
		}
		for _, sourceTurn := range sourceTurns {
			if err := deleteSourceExchangeTx(ctx, tx, row.userID, sourceTurn, row.requestID); err != nil {
				return counts, fmt.Errorf("erase forgotten memory source exchange: %w", err)
			}
		}
		counts.ForgottenMemories++
	}

	if counts.AuditRowsRedacted, err = execAffected(ctx, tx, `WITH due AS (SELECT id FROM memory_formation_audit WHERE redacted_at IS NULL AND (julianday(content_expires_at) <= julianday(?) OR (content_expires_at IS NULL AND julianday(created_at) <= julianday(?))) ORDER BY id LIMIT ?) UPDATE memory_formation_audit SET metadata = '', request_id = '', session_id = '', actor_id = '', redacted_at = ? WHERE id IN (SELECT id FROM due)`, nowText, contentCutoff, batch, nowText); err != nil {
		return counts, err
	}
	if counts.FormationJobsRedacted, err = execAffected(ctx, tx, `WITH due AS (SELECT id FROM memory_formation_jobs WHERE state IN ('succeeded','skipped','dead') AND julianday(updated_at) <= julianday(?) AND (extraction_payload != '' OR source_request_id != '' OR source_session_id != '' OR source_turn_id IS NOT NULL) ORDER BY id LIMIT ?) UPDATE memory_formation_jobs SET extraction_payload = '', source_request_id = '', source_session_id = '', source_turn_id = NULL WHERE id IN (SELECT id FROM due)`, contentCutoff, batch); err != nil {
		return counts, err
	}
	if counts.CompactionJobsRedacted, err = execAffected(ctx, tx, `WITH due AS (SELECT id FROM session_compaction_jobs WHERE state IN ('succeeded','skipped','dead') AND julianday(updated_at) <= julianday(?) AND (artifact_payload != '' OR last_error_message != '') ORDER BY id LIMIT ?) UPDATE session_compaction_jobs SET artifact_payload = '', last_error_message = '' WHERE id IN (SELECT id FROM due)`, contentCutoff, batch); err != nil {
		return counts, err
	}
	linkedJobsRedacted, linkedJobsErr := execAffected(ctx, tx, `WITH due AS (
		SELECT jobs.id FROM memory_formation_jobs jobs JOIN memory_candidates candidate ON candidate.source_turn_id = jobs.source_turn_id AND candidate.canonical_user_id = jobs.canonical_user_id
		WHERE julianday(candidate.created_at) <= julianday(?) AND candidate.state IN ('proposed','pending_confirmation','rejected')
			AND (jobs.extraction_payload != '' OR jobs.source_request_id != '' OR jobs.source_session_id != '' OR jobs.source_turn_id IS NOT NULL)
		ORDER BY jobs.id LIMIT ?
	) UPDATE memory_formation_jobs SET extraction_payload = '', source_request_id = '', source_session_id = '', source_turn_id = NULL WHERE id IN (SELECT id FROM due)`, candidateCutoff, batch)
	if linkedJobsErr != nil {
		return counts, linkedJobsErr
	}
	counts.FormationJobsRedacted += linkedJobsRedacted
	if counts.CandidatesRedacted, err = execAffected(ctx, tx, `WITH due AS (SELECT id FROM memory_candidates WHERE julianday(created_at) <= julianday(?) AND state IN ('proposed','pending_confirmation','rejected') AND (statement != '' OR evidence_summary != '' OR source_request_id != '' OR source_session_id != '' OR source_turn_id IS NOT NULL OR extraction_model != '' OR explicit_tool_source != '' OR confirmation_session_id != '' OR confirmation_request_id != '') ORDER BY id LIMIT ?) UPDATE memory_candidates SET statement = '', statement_key = 'retained:' || id, evidence_summary = '', source_request_id = '', source_session_id = '', source_turn_id = NULL, extraction_model = '', explicit_tool_source = '', confirmation_session_id = '', confirmation_request_id = '', state = 'rejected', decision_reason = 'candidate_retention_expired', decided_at = COALESCE(decided_at, ?), decided_by = 'retention', updated_at = ? WHERE id IN (SELECT id FROM due)`, candidateCutoff, batch, nowText, nowText); err != nil {
		return counts, err
	}
	if counts.EvidenceRowsRedacted, err = execAffected(ctx, tx, `WITH due AS (SELECT evidence.id FROM memory_evidence evidence LEFT JOIN memory_candidates candidate ON candidate.id = evidence.candidate_id WHERE (evidence.content != '' OR evidence.source_request_id != '' OR evidence.source_session_id != '' OR evidence.source_turn_id IS NOT NULL) AND (julianday(evidence.created_at) <= julianday(?) OR (candidate.statement = '' AND candidate.state = 'rejected')) AND NOT EXISTS (SELECT 1 FROM memory_entries memory WHERE memory.canonical_user_id = evidence.canonical_user_id AND memory.status = 'active' AND memory.approval_state = 'approved' AND (memory.id = evidence.memory_id OR memory.candidate_id = evidence.candidate_id OR memory.id = candidate.published_memory_id)) ORDER BY evidence.id LIMIT ?) UPDATE memory_evidence SET content = '', source_request_id = '', source_session_id = '', source_turn_id = NULL WHERE id IN (SELECT id FROM due)`, contentCutoff, batch); err != nil {
		return counts, err
	}
	if counts.EventsRedacted, err = execAffected(ctx, tx, `WITH due AS (SELECT id FROM memory_events WHERE julianday(created_at) <= julianday(?) AND (metadata != '' OR request_id != '' OR session_id != '') ORDER BY id LIMIT ?) UPDATE memory_events SET metadata = '', request_id = '', session_id = '' WHERE id IN (SELECT id FROM due)`, contentCutoff, batch); err != nil {
		return counts, err
	}
	if counts.PrivacyChallengesExpired, err = execAffected(ctx, tx, `WITH due AS (SELECT operation_id FROM privacy_operations WHERE status = 'pending' AND julianday(challenge_expires_at) <= julianday(?) ORDER BY julianday(challenge_expires_at), operation_id LIMIT ?) UPDATE privacy_operations SET status = 'expired', target_user_id = NULL, challenge_hash = '', challenge_expires_at = NULL, completed_at = ?, updated_at = ?, last_error_code = 'expired' WHERE operation_id IN (SELECT operation_id FROM due)`, nowText, batch, nowText, nowText); err != nil {
		return counts, err
	}

	if counts.EventTombstones, err = execAffected(ctx, tx, `DELETE FROM memory_events WHERE id IN (SELECT id FROM memory_events WHERE redacted_at IS NOT NULL AND julianday(redacted_at) <= julianday(?) AND metadata = '' AND request_id = '' AND session_id = '' ORDER BY id LIMIT ?)`, tombstoneCutoff, batch); err != nil {
		return counts, err
	}
	if counts.AuditTombstones, err = execAffected(ctx, tx, `DELETE FROM memory_formation_audit WHERE id IN (SELECT id FROM memory_formation_audit WHERE redacted_at IS NOT NULL AND julianday(redacted_at) <= julianday(?) ORDER BY id LIMIT ?)`, tombstoneCutoff, batch); err != nil {
		return counts, err
	}
	if counts.CandidateTombstones, err = execAffected(ctx, tx, `DELETE FROM memory_candidates WHERE id IN (SELECT candidate.id FROM memory_candidates candidate WHERE candidate.statement = '' AND julianday(candidate.updated_at) <= julianday(?) AND NOT EXISTS (SELECT 1 FROM memory_entries memory WHERE memory.candidate_id = candidate.id) AND NOT EXISTS (SELECT 1 FROM memory_formation_audit audit WHERE audit.candidate_id = candidate.id) ORDER BY candidate.id LIMIT ?)`, tombstoneCutoff, batch); err != nil {
		return counts, err
	}
	if counts.MemoryTombstonesDeleted, err = execAffected(ctx, tx, `DELETE FROM memory_entries WHERE id IN (SELECT memory.id FROM memory_entries memory WHERE memory.status IN ('deleted','expired') AND memory.statement = '' AND julianday(memory.updated_at) <= julianday(?) AND NOT EXISTS (SELECT 1 FROM memory_candidates candidate WHERE candidate.published_memory_id = memory.id OR candidate.supersedes_memory_id = memory.id) AND NOT EXISTS (SELECT 1 FROM memory_formation_audit audit WHERE audit.memory_id = memory.id) ORDER BY memory.id LIMIT ?)`, tombstoneCutoff, batch); err != nil {
		return counts, err
	}
	if counts.PrivacyTombstones, err = execAffected(ctx, tx, `DELETE FROM privacy_operations WHERE operation_id IN (SELECT operation_id FROM privacy_operations WHERE status IN ('completed','failed','expired') AND julianday(updated_at) <= julianday(?) ORDER BY julianday(updated_at), operation_id LIMIT ?)`, tombstoneCutoff, batch); err != nil {
		return counts, err
	}
	if counts.InvalidationTombstones, err = execAffected(ctx, tx, `DELETE FROM privacy_invalidation_events WHERE id IN (SELECT id FROM privacy_invalidation_events WHERE state = 'completed' AND external_identities = '[]' AND session_ids = '[]' AND julianday(completed_at) <= julianday(?) ORDER BY julianday(completed_at), id LIMIT ?)`, tombstoneCutoff, batch); err != nil {
		return counts, err
	}
	if counts.FormationJobsDeleted, err = execAffected(ctx, tx, `DELETE FROM memory_formation_jobs WHERE id IN (SELECT id FROM memory_formation_jobs WHERE ((state IN ('succeeded','skipped') AND julianday(completed_at) <= julianday(?)) OR (state = 'dead' AND julianday(completed_at) <= julianday(?))) AND extraction_payload = '' AND source_request_id = '' AND source_session_id = '' AND source_turn_id IS NULL ORDER BY id LIMIT ?)`, formatTime(now.Add(-policy.SuccessfulJobRetention)), formatTime(now.Add(-policy.DeadJobRetention)), batch); err != nil {
		return counts, err
	}
	if counts.CompactionJobsDeleted, err = execAffected(ctx, tx, `DELETE FROM session_compaction_jobs WHERE id IN (SELECT id FROM session_compaction_jobs WHERE ((state IN ('succeeded','skipped') AND julianday(completed_at) <= julianday(?)) OR (state = 'dead' AND julianday(completed_at) <= julianday(?))) AND artifact_payload = '' AND last_error_message = '' ORDER BY id LIMIT ?)`, formatTime(now.Add(-policy.SuccessfulJobRetention)), formatTime(now.Add(-policy.DeadJobRetention)), batch); err != nil {
		return counts, err
	}
	if counts.ChallengesDeleted, err = execAffected(ctx, tx, `DELETE FROM account_link_challenges WHERE id IN (SELECT id FROM account_link_challenges WHERE julianday(expires_at) <= julianday(?) ORDER BY julianday(expires_at), id LIMIT ?)`, formatTime(now.Add(-policy.AccountChallengeGrace)), batch); err != nil {
		return counts, err
	}
	if err := tx.Commit(); err != nil {
		return counts, fmt.Errorf("commit maintenance retention: %w", err)
	}
	// Privacy and expiry mutations are already durable. Wake the rebuild worker
	// before optional index/database hygiene so a later degraded step cannot
	// strand committed canonical deletions.
	s.signalDerivedIndex()

	indexCounts, indexErr := s.MaintainDerivedIndexes(ctx, now, policy.RetiredIndexRetention, policy.BatchSize)
	counts.IndexRowsDeleted = indexCounts.RowsDeleted
	counts.IndexRevisionsDegraded = indexCounts.RevisionsDegraded
	counts.IndexTablesDropped = indexCounts.TablesDropped
	if indexErr != nil {
		return counts, indexErr
	}
	if err := s.ReconcileDerivedIndexChanges(ctx); err != nil {
		return counts, fmt.Errorf("reconcile derived index outbox: %w", err)
	}
	if err := s.databaseHygiene(ctx, now, policy, &counts); err != nil {
		return counts, err
	}
	s.signalDerivedIndex()
	return counts, nil
}

func normalizedMaintenancePolicy(policy config.RetentionPolicy) config.RetentionPolicy {
	defaults := config.RetentionPolicy{ForgottenContentGrace: 30 * 24 * time.Hour, ContentBearingAuditJobRetention: 30 * 24 * time.Hour, ContentFreeTombstoneRetention: 365 * 24 * time.Hour, RetiredIndexRetention: 7 * 24 * time.Hour, SessionInactivity: 24 * time.Hour, CandidateContentRetention: 30 * 24 * time.Hour, SuccessfulJobRetention: 7 * 24 * time.Hour, DeadJobRetention: 30 * 24 * time.Hour, AccountChallengeGrace: 24 * time.Hour, MaintenanceInterval: time.Hour, DatabaseOptimizeInterval: 24 * time.Hour, BatchSize: 100}
	values := []*time.Duration{&policy.ForgottenContentGrace, &policy.ContentBearingAuditJobRetention, &policy.ContentFreeTombstoneRetention, &policy.RetiredIndexRetention, &policy.SessionInactivity, &policy.CandidateContentRetention, &policy.SuccessfulJobRetention, &policy.DeadJobRetention, &policy.AccountChallengeGrace, &policy.MaintenanceInterval, &policy.DatabaseOptimizeInterval}
	defaultValues := []time.Duration{defaults.ForgottenContentGrace, defaults.ContentBearingAuditJobRetention, defaults.ContentFreeTombstoneRetention, defaults.RetiredIndexRetention, defaults.SessionInactivity, defaults.CandidateContentRetention, defaults.SuccessfulJobRetention, defaults.DeadJobRetention, defaults.AccountChallengeGrace, defaults.MaintenanceInterval, defaults.DatabaseOptimizeInterval}
	for i := range values {
		if *values[i] <= 0 {
			*values[i] = defaultValues[i]
		}
	}
	if policy.BatchSize <= 0 {
		policy.BatchSize = defaults.BatchSize
	}
	return policy
}

func execAffected(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func maintenanceForeignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("maintenance foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("maintenance foreign key check failed")
	}
	return rows.Err()
}

func maintenanceForeignKeyCheckDB(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("maintenance foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("maintenance foreign key check failed")
	}
	return rows.Err()
}

func (s *Store) startMaintenanceRun(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.sql.ExecContext(ctx, `INSERT INTO maintenance_runs(run_type, state, started_at) VALUES ('periodic', 'running', ?)`, formatTime(now))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) finishMaintenanceRun(ctx context.Context, id int64, state, code string, counts MaintenanceCounts, completed time.Time) error {
	metadata, _ := json.Marshal(counts)
	_, err := s.sql.ExecContext(ctx, `UPDATE maintenance_runs SET state = ?, completed_at = ?, rows_changed = ?, last_error_code = ?, metadata = ? WHERE id = ?`, state, formatTime(completed), counts.Changed(), code, string(metadata), id)
	return err
}

func (s *Store) databaseHygiene(ctx context.Context, now time.Time, policy config.RetentionPolicy, counts *MaintenanceCounts) error {
	if _, err := s.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		return fmt.Errorf("passive WAL checkpoint: %w", err)
	}
	var autoVacuum int
	if err := s.sql.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		return fmt.Errorf("read auto vacuum mode: %w", err)
	}
	if autoVacuum == 2 {
		if _, err := s.sql.ExecContext(ctx, `PRAGMA incremental_vacuum(100)`); err != nil {
			return fmt.Errorf("incremental vacuum: %w", err)
		}
	}
	var lastOptimize sql.NullString
	err := s.sql.QueryRowContext(ctx, `SELECT MAX(completed_at) FROM maintenance_runs WHERE run_type = 'database_optimize' AND state = 'completed'`).Scan(&lastOptimize)
	if err != nil {
		return err
	}
	if lastOptimize.Valid && parseTime(lastOptimize.String).After(now.Add(-policy.DatabaseOptimizeInterval)) {
		return nil
	}
	result, err := s.sql.ExecContext(ctx, `INSERT INTO maintenance_runs(run_type, state, started_at) VALUES ('database_optimize', 'running', ?)`, formatTime(now))
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	if _, err := s.sql.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		_, _ = s.sql.ExecContext(context.Background(), `UPDATE maintenance_runs SET state = 'failed', completed_at = ?, last_error_code = 'optimize_failed' WHERE id = ?`, formatTime(time.Now().UTC()), id)
		return fmt.Errorf("optimize database: %w", err)
	}
	_, err = s.sql.ExecContext(ctx, `UPDATE maintenance_runs SET state = 'completed', completed_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), id)
	counts.OptimizeRun = err == nil
	return err
}
