package usermemory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// SessionProfile is the immutable tenant profile bound to one session generation.
type SessionProfile struct {
	VersionID       int64
	Version         int
	LatestVersion   int
	LatestFactCount int
	LatestBytes     int
	Generation      int
	SpeakerIntro    string
	Content         string
	FactCount       int
	Bytes           int
	IsNewVersion    bool
	IsNewSession    bool
}

// ResetSessionContext clears and refreshes one session using the standard TTL.
func (s *Store) ResetSessionContext(ctx context.Context, userID, sessionID string) error {
	_, err := s.ResetSession(ctx, userID, sessionID, s.sessionTTL(24*time.Hour))
	return err
}

// ResolveSessionProfile returns the frozen profile for a session, creating a
// new profile version or session generation when required.
func (s *Store) ResolveSessionProfile(ctx context.Context, userID, sessionID string, ttl time.Duration) (SessionProfile, error) {
	if err := s.ensureAccountUser(userID); err != nil {
		return SessionProfile{}, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return SessionProfile{}, fmt.Errorf("tenant profile: session id is required")
	}
	ttl = s.sessionTTL(ttl)
	now := time.Now().UTC()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return SessionProfile{}, fmt.Errorf("begin tenant profile resolution: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	current, err := refreshProfileTx(ctx, tx, userID, now)
	if err != nil {
		return SessionProfile{}, err
	}
	var generation int
	var boundVersionID int64
	var expiresRaw string
	err = tx.QueryRowContext(ctx, `SELECT generation, profile_version_id, expires_at FROM tenant_sessions WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID).Scan(&generation, &boundVersionID, &expiresRaw)
	if err != nil && err != sql.ErrNoRows {
		return SessionProfile{}, fmt.Errorf("read tenant session profile: %w", err)
	}
	isNewSession := err == sql.ErrNoRows
	if err == nil {
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, expiresRaw)
		if parseErr == nil && expiresAt.After(now) {
			profile, loadErr := loadProfileVersionTx(ctx, tx, userID, boundVersionID)
			if loadErr != nil {
				return SessionProfile{}, loadErr
			}
			if _, updateErr := tx.ExecContext(ctx, `UPDATE tenant_sessions SET last_seen_at = ?, expires_at = ? WHERE canonical_user_id = ? AND session_id = ?`, formatTime(now), formatTime(now.Add(ttl)), userID, sessionID); updateErr != nil {
				return SessionProfile{}, fmt.Errorf("refresh tenant session expiry: %w", updateErr)
			}
			if err := upsertSessionGenerationTx(ctx, tx, userID, sessionID, generation); err != nil {
				return SessionProfile{}, err
			}
			profile.Generation = generation
			profile.IsNewVersion = current.IsNewVersion
			profile.LatestVersion = current.Version
			profile.LatestFactCount = current.FactCount
			profile.LatestBytes = current.Bytes
			if err := pruneProfileVersionsTx(ctx, tx, userID, current.VersionID, now); err != nil {
				return SessionProfile{}, err
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return SessionProfile{}, fmt.Errorf("commit tenant profile resolution: %w", commitErr)
			}
			return profile, nil
		}
		generation++
		isNewSession = true
	} else {
		err := tx.QueryRowContext(ctx, `SELECT generation + 1 FROM tenant_session_generations WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID).Scan(&generation)
		if err == sql.ErrNoRows {
			if err := tx.QueryRowContext(ctx, `SELECT MAX(COALESCE((SELECT MAX(session_generation) FROM session_turns WHERE canonical_user_id = ? AND session_id = ?), 0), 1)`, userID, sessionID).Scan(&generation); err != nil {
				return SessionProfile{}, fmt.Errorf("resolve tenant session generation: %w", err)
			}
		} else if err != nil {
			return SessionProfile{}, fmt.Errorf("read tenant session generation: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO tenant_sessions (canonical_user_id, session_id, generation, profile_version_id, started_at, last_seen_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_user_id, session_id) DO UPDATE SET
	generation = excluded.generation,
	profile_version_id = excluded.profile_version_id,
	started_at = excluded.started_at,
	last_seen_at = excluded.last_seen_at,
	expires_at = excluded.expires_at
	`, userID, sessionID, generation, current.VersionID, formatTime(now), formatTime(now), formatTime(now.Add(ttl))); err != nil {
		return SessionProfile{}, fmt.Errorf("bind tenant session profile: %w", err)
	}
	if err := upsertSessionGenerationTx(ctx, tx, userID, sessionID, generation); err != nil {
		return SessionProfile{}, err
	}
	current.Generation = generation
	current.LatestVersion = current.Version
	current.LatestFactCount = current.FactCount
	current.LatestBytes = current.Bytes
	current.IsNewSession = isNewSession
	if err := pruneProfileVersionsTx(ctx, tx, userID, current.VersionID, now); err != nil {
		return SessionProfile{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionProfile{}, fmt.Errorf("commit tenant profile binding: %w", err)
	}
	return current, nil
}

// ResetSession clears one tenant's conversation history and binds the latest profile.
func (s *Store) ResetSession(ctx context.Context, userID, sessionID string, ttl time.Duration) (SessionProfile, error) {
	if err := s.ensureAccountUser(userID); err != nil {
		return SessionProfile{}, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return SessionProfile{}, fmt.Errorf("tenant profile: session id is required")
	}
	ttl = s.sessionTTL(ttl)
	now := time.Now().UTC()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return SessionProfile{}, fmt.Errorf("begin tenant session reset: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	current, err := refreshProfileTx(ctx, tx, userID, now)
	if err != nil {
		return SessionProfile{}, err
	}
	var generation int
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(generation), 0) + 1 FROM (
	SELECT generation FROM tenant_session_generations WHERE canonical_user_id = ? AND session_id = ?
	UNION ALL
	SELECT generation FROM tenant_sessions WHERE canonical_user_id = ? AND session_id = ?
	UNION ALL
	SELECT session_generation FROM session_turns WHERE canonical_user_id = ? AND session_id = ?
)
`, userID, sessionID, userID, sessionID, userID, sessionID).Scan(&generation); err != nil {
		return SessionProfile{}, fmt.Errorf("resolve reset session generation: %w", err)
	}
	if generation <= 0 {
		generation = 1
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_compaction_jobs WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID); err != nil {
		return SessionProfile{}, fmt.Errorf("clear reset session compaction jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_summaries WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID); err != nil {
		return SessionProfile{}, fmt.Errorf("clear reset session summaries: %w", err)
	}
	turnIDs, err := memoryIDsTx(tx, `SELECT id FROM session_turns WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID)
	if err != nil {
		return SessionProfile{}, fmt.Errorf("enumerate reset session turns: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_turns WHERE canonical_user_id = ? AND session_id = ?`, userID, sessionID); err != nil {
		return SessionProfile{}, fmt.Errorf("clear reset session turns: %w", err)
	}
	for _, id := range turnIDs {
		if err := enqueueDerivedChangeTx(ctx, tx, userID, "session_turn", id, "delete", "reset:"+formatTime(now)); err != nil {
			return SessionProfile{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tenant_sessions (canonical_user_id, session_id, generation, profile_version_id, started_at, last_seen_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_user_id, session_id) DO UPDATE SET
	generation = excluded.generation,
	profile_version_id = excluded.profile_version_id,
	started_at = excluded.started_at,
	last_seen_at = excluded.last_seen_at,
	expires_at = excluded.expires_at
	`, userID, sessionID, generation, current.VersionID, formatTime(now), formatTime(now), formatTime(now.Add(ttl))); err != nil {
		return SessionProfile{}, fmt.Errorf("bind reset tenant profile: %w", err)
	}
	if err := upsertSessionGenerationTx(ctx, tx, userID, sessionID, generation); err != nil {
		return SessionProfile{}, err
	}
	current.Generation = generation
	current.LatestVersion = current.Version
	current.LatestFactCount = current.FactCount
	current.LatestBytes = current.Bytes
	current.IsNewSession = true
	if err := pruneProfileVersionsTx(ctx, tx, userID, current.VersionID, now); err != nil {
		return SessionProfile{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionProfile{}, fmt.Errorf("commit tenant session reset: %w", err)
	}
	s.signalDerivedIndex()
	return current, nil
}

func upsertSessionGenerationTx(ctx context.Context, tx *sql.Tx, userID, sessionID string, generation int) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tenant_session_generations (canonical_user_id, session_id, generation)
VALUES (?, ?, ?)
ON CONFLICT(canonical_user_id, session_id) DO UPDATE SET generation = MAX(generation, excluded.generation)
`, userID, sessionID, generation); err != nil {
		return fmt.Errorf("persist tenant session generation: %w", err)
	}
	return nil
}

func pruneProfileVersionsTx(ctx context.Context, tx *sql.Tx, userID string, keepVersionID int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM tenant_sessions WHERE canonical_user_id = ? AND julianday(expires_at) <= julianday(?)`, userID, formatTime(now)); err != nil {
		return fmt.Errorf("prune expired tenant sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM tenant_profile_versions
WHERE canonical_user_id = ? AND id != ? AND NOT EXISTS (
	SELECT 1 FROM tenant_sessions sessions WHERE sessions.profile_version_id = tenant_profile_versions.id
)
`, userID, keepVersionID); err != nil {
		return fmt.Errorf("prune unreferenced tenant profiles: %w", err)
	}
	return nil
}

func refreshProfileTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (SessionProfile, error) {
	var intro string
	err := tx.QueryRowContext(ctx, `SELECT intro FROM user_memory_profiles WHERE canonical_user_id = ?`, userID).Scan(&intro)
	if err != nil && err != sql.ErrNoRows {
		return SessionProfile{}, fmt.Errorf("read tenant profile intro: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT id, category, statement, scope, status, profile_approved != 0, confidence, importance, expires_at, provenance_type, source_authority
FROM memory_entries
WHERE canonical_user_id = ? AND approval_state = 'approved'
`, userID)
	if err != nil {
		return SessionProfile{}, fmt.Errorf("read tenant profile candidates: %w", err)
	}
	var candidates []ProfileCandidate
	for rows.Next() {
		var candidate ProfileCandidate
		var expires sql.NullString
		if err := rows.Scan(&candidate.MemoryID, &candidate.Category, &candidate.Statement, &candidate.Scope, &candidate.Status, &candidate.Approved, &candidate.Confidence, &candidate.Importance, &expires, &candidate.FormationProvenance, &candidate.SourceAuthority); err != nil {
			rows.Close() // nolint:errcheck
			return SessionProfile{}, fmt.Errorf("scan tenant profile candidate: %w", err)
		}
		if expires.Valid {
			parsed, err := time.Parse(time.RFC3339Nano, expires.String)
			if err != nil {
				candidate.ExpiresAt = now
			} else {
				candidate.ExpiresAt = parsed
			}
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Close(); err != nil {
		return SessionProfile{}, fmt.Errorf("close tenant profile candidates: %w", err)
	}
	compiled := CompileTenantProfile(intro, candidates, now)
	var profile SessionProfile
	var latestDigest, latestRenderer string
	err = tx.QueryRowContext(ctx, `
SELECT id, version, speaker_intro, rendered_content, fact_count, profile_bytes, source_digest, renderer_version
FROM tenant_profile_versions
WHERE canonical_user_id = ? ORDER BY version DESC LIMIT 1
`, userID).Scan(&profile.VersionID, &profile.Version, &profile.SpeakerIntro, &profile.Content, &profile.FactCount, &profile.Bytes, &latestDigest, &latestRenderer)
	if err == nil {
		if latestDigest == compiled.SourceDigest && latestRenderer == ProfileRendererVersion {
			return profile, nil
		}
		profile = SessionProfile{}
	}
	if err != nil && err != sql.ErrNoRows {
		return SessionProfile{}, fmt.Errorf("read current tenant profile version: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
INSERT INTO tenant_profile_version_counters (canonical_user_id, version)
VALUES (?, (SELECT COALESCE(MAX(version), 0) + 1 FROM tenant_profile_versions WHERE canonical_user_id = ?))
ON CONFLICT(canonical_user_id) DO UPDATE SET version = version + 1
RETURNING version
`, userID, userID).Scan(&profile.Version); err != nil {
		return SessionProfile{}, fmt.Errorf("allocate tenant profile version: %w", err)
	}
	profile.SpeakerIntro = normalizeProfileText(intro)
	profile.Content = compiled.Content
	profile.FactCount = compiled.SelectedCount
	profile.Bytes = compiled.Bytes
	result, err := tx.ExecContext(ctx, `
INSERT INTO tenant_profile_versions (canonical_user_id, version, renderer_version, source_digest, speaker_intro, rendered_content, fact_count, profile_bytes, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
`, userID, profile.Version, ProfileRendererVersion, compiled.SourceDigest, profile.SpeakerIntro, profile.Content, profile.FactCount, profile.Bytes, formatTime(now))
	if err != nil {
		return SessionProfile{}, fmt.Errorf("create tenant profile version: %w", err)
	}
	profile.VersionID, err = result.LastInsertId()
	if err != nil {
		return SessionProfile{}, fmt.Errorf("read tenant profile version id: %w", err)
	}
	for ordinal, fact := range compiled.SelectedFacts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tenant_profile_version_facts (profile_version_id, ordinal, source_memory_id, category, statement) VALUES (?, ?, ?, ?, ?)`, profile.VersionID, ordinal, fact.MemoryID, fact.Category, fact.Statement); err != nil {
			return SessionProfile{}, fmt.Errorf("snapshot tenant profile fact: %w", err)
		}
	}
	profile.IsNewVersion = true
	return profile, nil
}

func loadProfileVersionTx(ctx context.Context, tx *sql.Tx, userID string, versionID int64) (SessionProfile, error) {
	var profile SessionProfile
	err := tx.QueryRowContext(ctx, `
SELECT id, version, speaker_intro, rendered_content, fact_count, profile_bytes
FROM tenant_profile_versions WHERE canonical_user_id = ? AND id = ?
`, userID, versionID).Scan(&profile.VersionID, &profile.Version, &profile.SpeakerIntro, &profile.Content, &profile.FactCount, &profile.Bytes)
	if err != nil {
		return SessionProfile{}, fmt.Errorf("load tenant profile version: %w", err)
	}
	return profile, nil
}
