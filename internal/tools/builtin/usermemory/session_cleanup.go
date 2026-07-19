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

// CleanupExpiredSessions removes expired session state without relying on a
// subsequent session read. Session generation counters are intentionally kept.
func (s *Store) CleanupExpiredSessions(ctx context.Context, now time.Time) (SessionCleanupCounts, error) {
	var counts SessionCleanupCounts
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	nowText := formatTime(now)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return counts, fmt.Errorf("begin expired session cleanup: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	result, err := tx.ExecContext(ctx, `
UPDATE memory_entries
SET status = 'expired', statement = '', statement_key = 'expired:' || id, evidence = '',
	invalidated_at = ?, invalidation_reason = 'ttl_expired', erased_at = ?, erasure_reason = 'ttl_expired', updated_at = ?
WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at <= ?
`, nowText, nowText, nowText, nowText)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("expire durable memories: %w", err)
	}
	if counts.MemoryEntriesExpired, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count expired durable memories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE memory_formation_jobs
SET extraction_payload = ''
WHERE source_turn_id IN (
	SELECT source_turn_id FROM memory_candidates
	WHERE published_memory_id IN (SELECT id FROM memory_entries WHERE status = 'expired')
		AND source_turn_id IS NOT NULL
);
UPDATE memory_evidence SET content = ''
WHERE canonical_user_id IN (SELECT canonical_user_id FROM memory_entries WHERE status = 'expired')
	AND (memory_id IN (SELECT id FROM memory_entries WHERE status = 'expired')
		OR candidate_id IN (SELECT candidate_id FROM memory_entries WHERE status = 'expired' AND candidate_id IS NOT NULL));
UPDATE memory_candidates
SET statement = '', statement_key = 'erased:' || id, evidence_summary = '', state = 'rejected',
	decision_reason = 'published_memory_expired', decided_at = ?, decided_by = 'retention', updated_at = ?
WHERE published_memory_id IN (SELECT id FROM memory_entries WHERE status = 'expired');
`, nowText, nowText); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("erase expired published memory provenance: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM tenant_profile_versions
WHERE id IN (
	SELECT facts.profile_version_id
	FROM tenant_profile_version_facts facts
	JOIN memory_entries entries ON entries.id = facts.source_memory_id
	WHERE entries.status = 'expired'
)
`); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete expired memory profile snapshots: %w", err)
	}
	var vectorsExist int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'memory_entry_vectors_v2'`).Scan(&vectorsExist); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("inspect memory vectors during cleanup: %w", err)
	}
	if vectorsExist != 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_entry_vectors_v2 WHERE rowid IN (SELECT id FROM memory_entries WHERE status != 'active')`); err != nil {
			return SessionCleanupCounts{}, fmt.Errorf("delete inactive memory vectors: %w", err)
		}
	}
	result, err = tx.ExecContext(ctx, `
UPDATE memory_candidates
SET statement = '', statement_key = 'erased:' || id, evidence_summary = '', state = 'rejected',
	decision_reason = 'candidate_retention_expired', decided_at = ?, decided_by = 'retention', updated_at = ?
WHERE (state IN ('proposed', 'pending_confirmation', 'rejected') OR (state = 'approved' AND published_memory_id IS NULL))
	AND ((expires_at IS NOT NULL AND expires_at <= ?) OR created_at <= ?)
`, nowText, nowText, nowText, formatTime(now.Add(-30*24*time.Hour)))
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("erase expired memory candidates: %w", err)
	}
	if counts.CandidatesErased, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count erased memory candidates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence SET content = '' WHERE candidate_id IN (SELECT id FROM memory_candidates WHERE statement = '' AND state = 'rejected')`); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("erase expired candidate evidence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_formation_jobs SET extraction_payload = '' WHERE source_turn_id IN (SELECT source_turn_id FROM memory_candidates WHERE statement = '' AND state = 'rejected' AND source_turn_id IS NOT NULL)`); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("erase retained formation artifacts: %w", err)
	}
	result, err = tx.ExecContext(ctx, `
DELETE FROM memory_formation_jobs
WHERE (state IN ('succeeded', 'skipped') AND completed_at <= ?)
	OR (state = 'dead' AND completed_at <= ?)
`, formatTime(now.Add(-7*24*time.Hour)), formatTime(now.Add(-30*24*time.Hour)))
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete retained memory formation jobs: %w", err)
	}
	if counts.FormationJobsDeleted, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count deleted memory formation jobs: %w", err)
	}

	result, err = tx.ExecContext(ctx, `
DELETE FROM session_compaction_jobs
WHERE NOT EXISTS (
	SELECT 1 FROM tenant_sessions active
	WHERE active.canonical_user_id = session_compaction_jobs.canonical_user_id
		AND active.session_id = session_compaction_jobs.session_id
		AND active.generation = session_compaction_jobs.session_generation
		AND active.expires_at > ?
)
`, nowText)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete inactive session compaction jobs: %w", err)
	}
	counts.CompactionJobsDeleted, _ = result.RowsAffected()
	result, err = tx.ExecContext(ctx, `
DELETE FROM session_summaries
WHERE NOT EXISTS (
	SELECT 1 FROM tenant_sessions active
	WHERE active.canonical_user_id = session_summaries.canonical_user_id
		AND active.session_id = session_summaries.session_id
		AND active.generation = session_summaries.session_generation
		AND active.expires_at > ?
)
`, nowText)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete inactive session summaries: %w", err)
	}
	counts.SessionSummariesDeleted, _ = result.RowsAffected()

	result, err = tx.ExecContext(ctx, `
DELETE FROM session_turns
WHERE ((expires_at IS NOT NULL AND expires_at <= ?) AND NOT EXISTS (
	SELECT 1 FROM tenant_sessions active
	WHERE active.canonical_user_id = session_turns.canonical_user_id
		AND active.session_id = session_turns.session_id
		AND active.generation = session_turns.session_generation
		AND active.expires_at > ?
))
   OR EXISTS (
	SELECT 1
	FROM tenant_sessions sessions
	WHERE sessions.canonical_user_id = session_turns.canonical_user_id
	  AND sessions.session_id = session_turns.session_id
	  AND sessions.generation = session_turns.session_generation
	  AND sessions.expires_at <= ?
   )
`, nowText, nowText, nowText)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete expired session turns: %w", err)
	}
	if counts.SessionTurnsDeleted, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count deleted session turns: %w", err)
	}

	result, err = tx.ExecContext(ctx, `DELETE FROM tenant_sessions WHERE expires_at <= ?`, nowText)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete expired tenant sessions: %w", err)
	}
	if counts.TenantSessionsDeleted, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count deleted tenant sessions: %w", err)
	}

	result, err = tx.ExecContext(ctx, `
DELETE FROM tenant_profile_versions
WHERE NOT EXISTS (
	SELECT 1 FROM tenant_sessions sessions
	WHERE sessions.profile_version_id = tenant_profile_versions.id
)
AND id != (
	SELECT latest.id
	FROM tenant_profile_versions latest
	WHERE latest.canonical_user_id = tenant_profile_versions.canonical_user_id
	ORDER BY latest.version DESC, latest.id DESC
	LIMIT 1
)
`)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("prune unreferenced tenant profiles: %w", err)
	}
	if counts.ProfileVersionsDeleted, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count pruned tenant profiles: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("commit expired session cleanup: %w", err)
	}
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
		logger = logger.Server("memory.session_cleanup")
	}
	sweep := func() {
		started := time.Now()
		counts, err := cleaner.CleanupExpiredSessions(ctx, started.UTC())
		if err != nil {
			if logger != nil && ctx.Err() == nil {
				logger.Warn("memory.session_cleanup.failed", "session cleanup failed", config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", "degraded"), config.ErrorField(err))
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
				logger.Debug("memory.session_cleanup.complete", "session cleanup completed", fields...)
			} else {
				logger.Info("memory.session_cleanup.complete", "session cleanup completed", fields...)
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
