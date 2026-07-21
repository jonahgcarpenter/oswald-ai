package usermemory

import (
	"context"
	"fmt"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// SessionCleanupCounts reports rows removed by one session cleanup transaction.
type SessionCleanupCounts struct {
	SessionTurnsDeleted     int64
	TenantSessionsDeleted   int64
	ProfileVersionsDeleted  int64
	MemoryEntriesExpired    int64
	CandidatesErased        int64
	FormationJobsDeleted    int64
	SessionSummariesDeleted int64
	CompactionJobsDeleted   int64
}

// SessionCleaner removes expired session state.
type SessionCleaner interface {
	CleanupExpiredSessions(context.Context, time.Time) (SessionCleanupCounts, error)
}

// CleanupExpiredSessions removes expired session artifacts while retaining each
// session row as the generation high-water record.
func (s *Store) CleanupExpiredSessions(ctx context.Context, now time.Time) (SessionCleanupCounts, error) {
	return s.cleanupExpiredSessions(ctx, now, config.RetentionPolicy{
		CandidateContentRetention: 30 * 24 * time.Hour,
		SuccessfulJobRetention:    7 * 24 * time.Hour,
		DeadJobRetention:          30 * 24 * time.Hour,
	}, false)
}

func (s *Store) cleanupExpiredSessions(ctx context.Context, now time.Time, policy config.RetentionPolicy, preserveCompactionJobs bool) (SessionCleanupCounts, error) {
	var counts SessionCleanupCounts
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	batch := policy.BatchSize
	if batch <= 0 {
		batch = 100
	}
	nowText := formatTime(now)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return counts, fmt.Errorf("begin expired session cleanup: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	expiringMemories, err := memoryIDsTx(tx, `SELECT id FROM memory_entries WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at <= ? ORDER BY expires_at, id LIMIT ?`, nowText, batch)
	if err != nil {
		return counts, fmt.Errorf("enumerate expiring memories: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
UPDATE memory_entries
SET status = 'expired', statement = '', statement_key = 'expired:' || id, claim_key = 'expired:' || id, claim_slot = '', claim_value = '', evidence = '',
	invalidated_at = ?, invalidation_reason = 'ttl_expired', erased_at = ?, erasure_reason = 'ttl_expired', updated_at = ?
WHERE id IN (SELECT id FROM memory_entries WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at <= ? ORDER BY expires_at, id LIMIT ?)
`, nowText, nowText, nowText, nowText, batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("expire durable memories: %w", err)
	}
	if counts.MemoryEntriesExpired, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count expired durable memories: %w", err)
	}
	for _, id := range expiringMemories {
		var userID string
		if err := tx.QueryRowContext(ctx, `SELECT canonical_user_id FROM memory_entries WHERE id = ?`, id).Scan(&userID); err != nil {
			return counts, err
		}
		if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", id, "delete", "expire:"+nowText); err != nil {
			return counts, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE durable_jobs
SET extraction_payload = '', source_request_id = '', source_session_id = '', source_turn_id = NULL
WHERE job_kind = 'memory_formation' AND id IN (SELECT id FROM durable_jobs WHERE job_kind = 'memory_formation' AND source_turn_id IN (
	SELECT source_turn_id FROM memory_candidates
	WHERE published_memory_id IN (SELECT id FROM memory_entries WHERE status = 'expired')
		AND source_turn_id IS NOT NULL
) AND (extraction_payload != '' OR source_request_id != '' OR source_session_id != '' OR source_turn_id IS NOT NULL) ORDER BY id LIMIT ?);
UPDATE memory_evidence SET content = '', correlation_key = '', source_request_id = '', source_session_id = '', source_turn_id = NULL
WHERE id IN (SELECT id FROM memory_evidence WHERE canonical_user_id IN (SELECT canonical_user_id FROM memory_entries WHERE status = 'expired')
	AND (memory_id IN (SELECT id FROM memory_entries WHERE status = 'expired')
		OR candidate_id IN (SELECT candidate_id FROM memory_entries WHERE status = 'expired' AND candidate_id IS NOT NULL))
	AND (content != '' OR source_request_id != '' OR source_session_id != '' OR source_turn_id IS NOT NULL) ORDER BY id LIMIT ?);
UPDATE memory_candidates
SET statement = '', statement_key = 'erased:' || id, claim_key = 'erased:' || id, claim_slot = '', claim_value = '', evidence_summary = '', state = 'rejected',
	source_request_id = '', source_session_id = '', source_turn_id = NULL, extraction_model = '', explicit_tool_source = '',
	confirmation_session_id = '', confirmation_request_id = '', decision_reason = 'published_memory_expired',
	decided_at = ?, decided_by = 'retention', updated_at = ?
WHERE id IN (SELECT id FROM memory_candidates WHERE published_memory_id IN (SELECT id FROM memory_entries WHERE status = 'expired')
	AND (statement != '' OR evidence_summary != '' OR source_request_id != '' OR source_session_id != '' OR source_turn_id IS NOT NULL OR extraction_model != '' OR explicit_tool_source != '' OR confirmation_session_id != '' OR confirmation_request_id != '') ORDER BY id LIMIT ?);
`, batch, batch, nowText, nowText, batch); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("erase expired published memory provenance: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET extraction_payload = '', source_request_id = '', source_session_id = '', source_turn_id = NULL WHERE job_kind = 'memory_formation' AND id IN (
		SELECT jobs.id FROM durable_jobs jobs JOIN memory_candidates candidate ON candidate.source_turn_id = jobs.source_turn_id AND candidate.canonical_user_id = jobs.canonical_user_id
		WHERE jobs.job_kind = 'memory_formation' AND (candidate.state IN ('proposed', 'pending_confirmation', 'rejected') OR (candidate.state = 'approved' AND candidate.published_memory_id IS NULL))
			AND ((candidate.expires_at IS NOT NULL AND candidate.expires_at <= ?) OR candidate.created_at <= ?)
			AND (jobs.extraction_payload != '' OR jobs.source_request_id != '' OR jobs.source_session_id != '' OR jobs.source_turn_id IS NOT NULL)
		ORDER BY jobs.id LIMIT ?)`, nowText, formatTime(now.Add(-policy.CandidateContentRetention)), batch); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("erase expiring candidate formation artifacts: %w", err)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE memory_candidates
SET statement = '', statement_key = 'erased:' || id, claim_key = 'erased:' || id, claim_slot = '', claim_value = '', evidence_summary = '', state = 'rejected',
	source_request_id = '', source_session_id = '', source_turn_id = NULL, extraction_model = '', explicit_tool_source = '',
	confirmation_session_id = '', confirmation_request_id = '', decision_reason = 'candidate_retention_expired',
	decided_at = ?, decided_by = 'retention', updated_at = ?
WHERE (state IN ('proposed', 'pending_confirmation', 'rejected') OR (state = 'approved' AND published_memory_id IS NULL))
	AND ((expires_at IS NOT NULL AND expires_at <= ?) OR created_at <= ?)
	AND id IN (SELECT id FROM memory_candidates WHERE (state IN ('proposed', 'pending_confirmation', 'rejected') OR (state = 'approved' AND published_memory_id IS NULL)) AND ((expires_at IS NOT NULL AND expires_at <= ?) OR created_at <= ?)
		AND (statement != '' OR evidence_summary != '' OR source_request_id != '' OR source_session_id != '' OR source_turn_id IS NOT NULL OR extraction_model != '' OR explicit_tool_source != '' OR confirmation_session_id != '' OR confirmation_request_id != '') ORDER BY id LIMIT ?)
`, nowText, nowText, nowText, formatTime(now.Add(-policy.CandidateContentRetention)), nowText, formatTime(now.Add(-policy.CandidateContentRetention)), batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("erase expired memory candidates: %w", err)
	}
	if counts.CandidatesErased, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count erased memory candidates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence SET content = '', correlation_key = '', source_request_id = '', source_session_id = '', source_turn_id = NULL WHERE id IN (SELECT evidence.id FROM memory_evidence evidence JOIN memory_candidates candidate ON candidate.id = evidence.candidate_id WHERE candidate.statement = '' AND candidate.state = 'rejected' AND (evidence.content != '' OR evidence.correlation_key != '' OR evidence.source_request_id != '' OR evidence.source_session_id != '' OR evidence.source_turn_id IS NOT NULL) ORDER BY evidence.id LIMIT ?)`, batch); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("erase expired candidate evidence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET extraction_payload = '', source_request_id = '', source_session_id = '', source_turn_id = NULL WHERE job_kind = 'memory_formation' AND id IN (SELECT jobs.id FROM durable_jobs jobs JOIN memory_candidates candidate ON candidate.source_turn_id = jobs.source_turn_id AND candidate.canonical_user_id = jobs.canonical_user_id WHERE jobs.job_kind = 'memory_formation' AND candidate.statement = '' AND candidate.state = 'rejected' AND (jobs.extraction_payload != '' OR jobs.source_request_id != '' OR jobs.source_session_id != '' OR jobs.source_turn_id IS NOT NULL) ORDER BY jobs.id LIMIT ?)`, batch); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("erase retained formation artifacts: %w", err)
	}
	result, err = tx.ExecContext(ctx, `
DELETE FROM durable_jobs
WHERE job_kind = 'memory_formation' AND id IN (SELECT id FROM durable_jobs WHERE job_kind = 'memory_formation' AND ((state IN ('succeeded', 'skipped') AND completed_at <= ?)
	OR (state = 'dead' AND completed_at <= ?))
	AND extraction_payload = '' AND source_request_id = '' AND source_session_id = '' AND source_turn_id IS NULL
	ORDER BY id LIMIT ?)
`, formatTime(now.Add(-policy.SuccessfulJobRetention)), formatTime(now.Add(-policy.DeadJobRetention)), batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete retained memory formation jobs: %w", err)
	}
	if counts.FormationJobsDeleted, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count deleted memory formation jobs: %w", err)
	}

	compactionMutation := `
DELETE FROM durable_jobs WHERE job_kind = 'session_compaction' AND id IN (SELECT id FROM durable_jobs jobs
WHERE jobs.job_kind = 'session_compaction' AND NOT EXISTS (
	SELECT 1 FROM sessions active
	WHERE active.canonical_user_id = jobs.canonical_user_id
		AND active.session_id = jobs.session_id
		AND active.generation = jobs.session_generation
		AND active.is_active = 1
		AND active.expires_at > ?
) ORDER BY id LIMIT ?)
`
	if preserveCompactionJobs {
		compactionMutation = `
UPDATE durable_jobs
SET state = CASE WHEN state IN ('queued','running','retry') THEN 'skipped' ELSE state END,
	artifact_summary_id = NULL, lease_owner = '', lease_until = NULL,
	completed_at = CASE WHEN state IN ('queued','running','retry') THEN COALESCE(completed_at, ?) ELSE completed_at END,
	updated_at = CASE WHEN state IN ('queued','running','retry') THEN ? ELSE updated_at END
WHERE job_kind = 'session_compaction' AND id IN (SELECT id FROM durable_jobs jobs WHERE jobs.job_kind = 'session_compaction' AND (jobs.state IN ('queued','running','retry') OR jobs.artifact_summary_id IS NOT NULL) AND NOT EXISTS (
	SELECT 1 FROM sessions active
	WHERE active.canonical_user_id = jobs.canonical_user_id
		AND active.session_id = jobs.session_id
		AND active.generation = jobs.session_generation
		AND active.is_active = 1
		AND active.expires_at > ?
) ORDER BY id LIMIT ?)
`
		result, err = tx.ExecContext(ctx, compactionMutation, nowText, nowText, nowText, batch)
	} else {
		result, err = tx.ExecContext(ctx, compactionMutation, nowText, batch)
	}
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete inactive session compaction jobs: %w", err)
	}
	counts.CompactionJobsDeleted, _ = result.RowsAffected()
	result, err = tx.ExecContext(ctx, `
DELETE FROM session_summaries WHERE id IN (SELECT id FROM session_summaries summaries
WHERE NOT EXISTS (
	SELECT 1 FROM sessions active
	WHERE active.canonical_user_id = summaries.canonical_user_id
		AND active.session_id = summaries.session_id
		AND active.generation = summaries.session_generation
		AND active.is_active = 1
		AND active.expires_at > ?
) ORDER BY id LIMIT ?)
`, nowText, batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete inactive session summaries: %w", err)
	}
	counts.SessionSummariesDeleted, _ = result.RowsAffected()

	turnRows, err := tx.QueryContext(ctx, `SELECT id, canonical_user_id FROM session_turns WHERE ((expires_at IS NOT NULL AND expires_at <= ?) AND NOT EXISTS (SELECT 1 FROM sessions active WHERE active.canonical_user_id = session_turns.canonical_user_id AND active.session_id = session_turns.session_id AND active.generation = session_turns.session_generation AND active.is_active = 1 AND active.expires_at > ?)) OR EXISTS (SELECT 1 FROM sessions WHERE sessions.canonical_user_id = session_turns.canonical_user_id AND sessions.session_id = session_turns.session_id AND sessions.generation = session_turns.session_generation AND (sessions.is_active = 0 OR sessions.expires_at <= ?)) ORDER BY id LIMIT ?`, nowText, nowText, nowText, batch)
	if err != nil {
		return counts, err
	}
	type turnOwner struct {
		id     int64
		userID string
	}
	var deletedTurns []turnOwner
	for turnRows.Next() {
		var turn turnOwner
		if err := turnRows.Scan(&turn.id, &turn.userID); err != nil {
			turnRows.Close()
			return counts, err
		}
		deletedTurns = append(deletedTurns, turn)
	}
	if err := turnRows.Close(); err != nil {
		return counts, err
	}
	for _, turn := range deletedTurns {
		result, err = tx.ExecContext(ctx, `DELETE FROM session_turns WHERE id = ? AND canonical_user_id = ?`, turn.id, turn.userID)
		if err != nil {
			return SessionCleanupCounts{}, fmt.Errorf("delete expired session turn: %w", err)
		}
		changed, _ := result.RowsAffected()
		counts.SessionTurnsDeleted += changed
	}
	for _, turn := range deletedTurns {
		if err := enqueueDerivedChangeTx(ctx, tx, turn.userID, "session_turn", turn.id, "delete", "cleanup:"+nowText); err != nil {
			return counts, err
		}
	}

	result, err = tx.ExecContext(ctx, `UPDATE sessions SET is_active = 0 WHERE rowid IN (SELECT rowid FROM sessions WHERE is_active = 1 AND expires_at <= ? ORDER BY expires_at, rowid LIMIT ?)`, nowText, batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("deactivate expired tenant sessions: %w", err)
	}
	if counts.TenantSessionsDeleted, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count deleted tenant sessions: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("commit expired session cleanup: %w", err)
	}
	s.signalDerivedIndex()
	return counts, nil
}

// RunSessionCleanup immediately sweeps expired sessions, then repeats at the
// configured interval until ctx is canceled. Sweeps run synchronously so they
// can never overlap.
func RunSessionCleanup(ctx context.Context, cleaner SessionCleaner, interval time.Duration, logger *config.Logger) {
	if cleaner == nil {
		return
	}
	if logger != nil {
		logger = logger.Server("session_memory.cleanup")
	}
	sweep := func() {
		started := time.Now()
		counts, err := cleaner.CleanupExpiredSessions(ctx, started.UTC())
		if err != nil {
			if logger != nil && ctx.Err() == nil {
				logger.Warn("session_memory.cleanup.failed", "session-memory cleanup failed", config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", "degraded"), config.ErrorField(err))
			}
			return
		}
		if logger != nil {
			fields := []config.Field{
				config.F("session_turn_count", counts.SessionTurnsDeleted),
				config.F("tenant_session_count", counts.TenantSessionsDeleted),
				config.F("profile_version_count", counts.ProfileVersionsDeleted),
				config.F("memory_expired_count", counts.MemoryEntriesExpired),
				config.F("candidate_erased_count", counts.CandidatesErased),
				config.F("formation_job_deleted_count", counts.FormationJobsDeleted),
				config.F("session_summary_deleted_count", counts.SessionSummariesDeleted),
				config.F("compaction_job_deleted_count", counts.CompactionJobsDeleted),
				config.F("duration_ms", time.Since(started).Milliseconds()),
				config.F("status", "ok"),
			}
			if counts.SessionTurnsDeleted+counts.TenantSessionsDeleted+counts.ProfileVersionsDeleted+counts.MemoryEntriesExpired+counts.CandidatesErased+counts.FormationJobsDeleted+counts.SessionSummariesDeleted+counts.CompactionJobsDeleted == 0 {
				logger.Debug("session_memory.cleanup.complete", "session-memory cleanup completed", fields...)
			} else {
				logger.Info("session_memory.cleanup.complete", "session-memory cleanup completed", fields...)
			}
		}
	}

	if ctx.Err() != nil {
		return
	}
	sweep()
	if ctx.Err() != nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
