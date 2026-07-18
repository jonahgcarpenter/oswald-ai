package usermemory

import (
	"context"
	"fmt"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// SessionCleanupCounts reports rows removed by one session cleanup transaction.
type SessionCleanupCounts struct {
	SessionTurnsDeleted    int64
	TenantSessionsDeleted  int64
	ProfileVersionsDeleted int64
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
DELETE FROM session_turns
WHERE (expires_at IS NOT NULL AND expires_at <= ?)
   OR EXISTS (
	SELECT 1
	FROM tenant_sessions sessions
	WHERE sessions.canonical_user_id = session_turns.canonical_user_id
	  AND sessions.session_id = session_turns.session_id
	  AND sessions.generation = session_turns.session_generation
	  AND sessions.expires_at <= ?
   )
`, nowText, nowText)
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
				config.F("duration_ms", time.Since(started).Milliseconds()),
				config.F("status", "ok"),
			}
			if counts.SessionTurnsDeleted+counts.TenantSessionsDeleted+counts.ProfileVersionsDeleted == 0 {
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
