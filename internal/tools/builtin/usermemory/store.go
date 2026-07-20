package usermemory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

const (
	ScopeShortTerm = "short_term"
	ScopeLongTerm  = "long_term"

	StatusActive     = "active"
	StatusExpired    = "expired"
	StatusSuperseded = "superseded"
	StatusDeleted    = "deleted"

	DefaultShortTermTTL = 30 * 24 * time.Hour
)

// ValidCategories lists supported memory categories in display order.
var ValidCategories = []string{"identity", "communication_preferences", "durable_preferences", "projects", "relationships", "environment", "notes"}

// MemoryEntry is a single short-term or long-term user memory.
type MemoryEntry struct {
	ID              int64
	UserID          string
	Scope           string
	Category        string
	Statement       string
	Evidence        string
	Confidence      float64
	Importance      int
	Status          string
	SourceSessionID string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastUsedAt      time.Time
	ExpiresAt       time.Time
	SupersedesID    int64
	EmbeddingModel  string
	EmbeddingDim    int
	ProvenanceType  string
	SourceAuthority string
	ApprovalState   string
	Sensitivity     string
	ClaimKey        string
	ClaimSlot       string
	ClaimValue      string
	EvidenceCount   int
	Score           float64
}

// SessionTurn is a completed exchange stored for session continuity.
type SessionTurn struct {
	ID            int64
	SessionID     string
	UserID        string
	Generation    int
	UserText      string
	AssistantText string
	ToolNames     []string
	Importance    int
	TopicTags     []string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	Score         float64
}

// ContextOptions controls request-time memory retrieval.
type ContextOptions struct {
	RecentTurns int
	Generation  int

	ContextBudgetChars int
}

// RetrievedContext contains the memory block selected for a request.
type RetrievedContext struct {
	Block           string
	RecentTurnCount int
	RecentToolNames []string
}

// SaveRequest describes a user memory write.
type SaveRequest struct {
	Scope           string
	Category        string
	Statement       string
	Evidence        string
	Confidence      float64
	Importance      int
	SourceSessionID string
	TTL             time.Duration
	Supersedes      string
	Embedding       []float64
}

// Store manages speaker profiles, user memories, and session memory in SQLite.
type Store struct {
	dbPath      string
	db          *database.DB
	sql         *sql.DB
	log         *config.Logger
	embedder    llm.Embedder
	embedModel  string
	indexNotify func()
	retention   config.RetentionPolicy
	mutationMu  sync.Mutex
	userLocks   map[string]*sync.Mutex

	formationFailpoint func(string) error
	indexWriteHook     func(string)

	speakerLineResolver func(string) (string, error)
}

// NewStore creates a SQLite-backed Store. The argument is treated as a database path.
func NewStore(dbPath string, log *config.Logger) *Store {
	store, err := NewSQLiteStore(dbPath, nil, "", log)
	if err != nil {
		panic(err)
	}
	return store
}

// NewSQLiteStore creates a fresh-schema SQLite-backed Store.
func NewSQLiteStore(dbPath string, embedder llm.Embedder, embeddingModel string, log *config.Logger) (*Store, error) {
	db, err := database.Open(dbPath, log)
	if err != nil {
		return nil, err
	}
	return &Store{
		dbPath:     dbPath,
		db:         db,
		sql:        db.SQL(),
		log:        log,
		embedder:   embedder,
		embedModel: strings.TrimSpace(embeddingModel),
	}, nil
}

// Close closes the store database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SetSpeakerLineResolver configures how speaker intro lines are derived.
func (s *Store) SetSpeakerLineResolver(resolver func(string) (string, error)) {
	s.speakerLineResolver = resolver
}

// SetDerivedIndexNotifier installs a nonblocking wake-up callback for the
// durable derived-index worker. Correctness never depends on the callback.
func (s *Store) SetDerivedIndexNotifier(notify func()) {
	s.indexNotify = notify
}

// SetRetentionPolicy applies configured lifecycle durations to session writes.
// Tests and embedders that do not call it retain their explicitly supplied TTLs.
func (s *Store) SetRetentionPolicy(policy config.RetentionPolicy) {
	s.retention = policy
}

func (s *Store) sessionTTL(fallback time.Duration) time.Duration {
	if s.retention.SessionInactivity > 0 {
		return s.retention.SessionInactivity
	}
	if fallback > 0 {
		return fallback
	}
	return 24 * time.Hour
}

func (s *Store) signalDerivedIndex() {
	if s.indexNotify != nil {
		s.indexNotify()
	}
}

// SyncSpeakerIntro creates or updates the account-derived speaker intro.
func (s *Store) SyncSpeakerIntro(userID, intro string) error {
	if err := s.ensureAccountUser(userID); err != nil {
		return err
	}
	now := formatTime(time.Now())
	_, err := s.sql.Exec(`
INSERT INTO user_memory_profiles (canonical_user_id, intro, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(canonical_user_id) DO UPDATE SET intro = excluded.intro, updated_at = excluded.updated_at
`, userID, strings.TrimSpace(intro), now, now)
	if err != nil {
		return fmt.Errorf("failed to sync user memory intro for %q: %w", userID, err)
	}
	return nil
}

// ReadIntro returns the current speaker intro for a user.
func (s *Store) ReadIntro(userID string) (string, error) {
	var intro string
	err := s.sql.QueryRow(`SELECT intro FROM user_memory_profiles WHERE canonical_user_id = ?`, userID).Scan(&intro)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read user memory intro for %q: %w", userID, err)
	}
	intro = strings.TrimSpace(intro)
	if !strings.HasPrefix(intro, "You are speaking with ") {
		return "", nil
	}
	return intro, nil
}

// MergeUsers moves memory/session ownership from loserUserID into winnerUserID.
func (s *Store) MergeUsers(winnerUserID, loserUserID string) error {
	if winnerUserID == "" || loserUserID == "" || winnerUserID == loserUserID {
		return nil
	}
	unlock := s.lockUsers(winnerUserID, loserUserID)
	defer unlock()
	if err := s.ensureAccountUser(winnerUserID); err != nil {
		return err
	}
	intro, err := s.ReadIntro(winnerUserID)
	if err != nil {
		return err
	}
	if s.speakerLineResolver != nil {
		intro, err = s.speakerLineResolver(winnerUserID)
		if err != nil {
			return fmt.Errorf("failed to resolve merged user intro: %w", err)
		}
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin memory merge: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	if err := MergeUsersTx(context.Background(), tx, winnerUserID, loserUserID, intro); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.signalDerivedIndex()
	return nil
}

// MergeUsersTx moves loser-owned memory data to the winner using the supplied transaction.
// It does not commit or roll back tx.
func MergeUsersTx(ctx context.Context, tx *sql.Tx, winnerID, loserID, intro string) error {
	if tx == nil {
		return fmt.Errorf("user memory merge: transaction is required")
	}
	winnerID = strings.TrimSpace(winnerID)
	loserID = strings.TrimSpace(loserID)
	if winnerID == "" || loserID == "" {
		return fmt.Errorf("user memory merge: winner and loser ids are required")
	}
	if winnerID == loserID {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("defer memory merge foreign keys: %w", err)
	}

	// Compaction checkpoints are immutable and turns with checkpoint references cannot
	// change tenant or generation. Snapshot the graph, remove its references, move the
	// turns, then restore the same checkpoint and job IDs.
	if _, err := tx.ExecContext(ctx, `
DROP TABLE IF EXISTS temp.merge_session_generation_map;
CREATE TEMP TABLE merge_session_generation_map AS
WITH loser_generations AS (
	SELECT session_id, session_generation AS generation FROM session_turns WHERE canonical_user_id = ?
	UNION SELECT session_id, session_generation FROM session_summaries WHERE canonical_user_id = ?
	UNION SELECT session_id, session_generation FROM session_compaction_jobs WHERE canonical_user_id = ?
	UNION SELECT session_id, generation FROM tenant_sessions WHERE canonical_user_id = ?
), numbered AS (
	SELECT session_id, generation,
		ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY generation) AS ordinal
	FROM loser_generations
)
SELECT numbered.session_id, numbered.generation AS old_generation,
	CASE WHEN EXISTS (
		SELECT 1 FROM (
			SELECT session_id, session_generation AS generation FROM session_turns WHERE canonical_user_id = ?
			UNION SELECT session_id, session_generation FROM session_summaries WHERE canonical_user_id = ?
			UNION SELECT session_id, session_generation FROM session_compaction_jobs WHERE canonical_user_id = ?
			UNION SELECT session_id, generation FROM tenant_sessions WHERE canonical_user_id = ?
		) winner_state
		WHERE winner_state.session_id = numbered.session_id AND winner_state.generation = numbered.generation
	) THEN COALESCE((
		SELECT MAX(generation) FROM (
			SELECT session_generation AS generation FROM session_turns WHERE canonical_user_id IN (?, ?) AND session_id = numbered.session_id
			UNION ALL SELECT session_generation FROM session_summaries WHERE canonical_user_id IN (?, ?) AND session_id = numbered.session_id
			UNION ALL SELECT session_generation FROM session_compaction_jobs WHERE canonical_user_id IN (?, ?) AND session_id = numbered.session_id
			UNION ALL SELECT generation FROM tenant_sessions WHERE canonical_user_id IN (?, ?) AND session_id = numbered.session_id
			UNION ALL SELECT generation FROM tenant_session_generations WHERE canonical_user_id IN (?, ?) AND session_id = numbered.session_id
		)
	), 0) + numbered.ordinal
	ELSE numbered.generation END AS new_generation
FROM numbered;

DROP TABLE IF EXISTS temp.merge_session_summaries;
CREATE TEMP TABLE merge_session_summaries AS SELECT * FROM session_summaries WHERE canonical_user_id = ?;
DROP TABLE IF EXISTS temp.merge_summary_sources;
CREATE TEMP TABLE merge_summary_sources AS SELECT * FROM session_summary_sources WHERE canonical_user_id = ?;
DROP TABLE IF EXISTS temp.merge_compaction_jobs;
CREATE TEMP TABLE merge_compaction_jobs AS SELECT * FROM session_compaction_jobs WHERE canonical_user_id = ?;
DROP TABLE IF EXISTS temp.merge_tenant_sessions;
CREATE TEMP TABLE merge_tenant_sessions AS
SELECT sessions.session_id, COALESCE(map.new_generation, sessions.generation) AS generation,
	sessions.profile_version_id, sessions.started_at, sessions.last_seen_at, sessions.expires_at
FROM tenant_sessions sessions
LEFT JOIN merge_session_generation_map map
	ON map.session_id = sessions.session_id AND map.old_generation = sessions.generation
WHERE sessions.canonical_user_id = ?;

DELETE FROM session_compaction_jobs WHERE canonical_user_id = ?;
DELETE FROM session_summaries WHERE canonical_user_id = ?;
DELETE FROM tenant_sessions WHERE canonical_user_id = ?;

UPDATE session_turns
SET canonical_user_id = ?,
	session_generation = COALESCE((SELECT new_generation FROM merge_session_generation_map map WHERE map.session_id = session_turns.session_id AND map.old_generation = session_turns.session_generation), session_generation)
WHERE canonical_user_id = ?;
`, loserID, loserID, loserID, loserID,
		winnerID, winnerID, winnerID, winnerID,
		winnerID, loserID, winnerID, loserID, winnerID, loserID, winnerID, loserID, winnerID, loserID,
		loserID, loserID, loserID, loserID,
		loserID, loserID, loserID, winnerID, loserID); err != nil {
		return fmt.Errorf("snapshot and move merged sessions: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO session_summaries (
	id, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	narrative, open_tasks, commitments, entities, decisions, topic_tags, generation_model,
	generator_version, source_digest, created_at, expires_at
)
SELECT summary.id, ?, summary.session_id, COALESCE(map.new_generation, summary.session_generation),
	summary.covered_from_turn_id, summary.covered_through_turn_id, summary.narrative, summary.open_tasks,
	summary.commitments, summary.entities, summary.decisions, summary.topic_tags, summary.generation_model,
	summary.generator_version, summary.source_digest, summary.created_at, summary.expires_at
FROM merge_session_summaries summary
LEFT JOIN merge_session_generation_map map
	ON map.session_id = summary.session_id AND map.old_generation = summary.session_generation;

INSERT INTO session_summary_sources (summary_id, canonical_user_id, session_id, session_generation, turn_id, ordinal)
SELECT source.summary_id, ?, source.session_id, COALESCE(map.new_generation, source.session_generation), source.turn_id, source.ordinal
FROM merge_summary_sources source
LEFT JOIN merge_session_generation_map map
	ON map.session_id = source.session_id AND map.old_generation = source.session_generation;

INSERT INTO session_compaction_jobs (
	id, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	state, artifact_payload, artifact_summary_id, generation_model, generator_version, attempt_count,
	redrive_count, available_at, lease_owner, lease_until, started_at, completed_at,
	last_error_code, last_error_message, created_at, updated_at
)
SELECT job.id, ?, job.session_id, COALESCE(map.new_generation, job.session_generation),
	job.covered_from_turn_id, job.covered_through_turn_id,
	CASE WHEN job.state = 'running' THEN 'retry' ELSE job.state END,
	job.artifact_payload, job.artifact_summary_id, job.generation_model, job.generator_version,
	job.attempt_count, job.redrive_count, job.available_at,
	CASE WHEN job.state = 'running' THEN '' ELSE job.lease_owner END,
	CASE WHEN job.state = 'running' THEN NULL ELSE job.lease_until END,
	job.started_at, job.completed_at, job.last_error_code, job.last_error_message, job.created_at, job.updated_at
FROM merge_compaction_jobs job
LEFT JOIN merge_session_generation_map map
	ON map.session_id = job.session_id AND map.old_generation = job.session_generation;

UPDATE session_compaction_jobs
SET state = 'retry', lease_owner = '', lease_until = NULL
WHERE canonical_user_id = ? AND state = 'running';
`, winnerID, winnerID, winnerID, winnerID); err != nil {
		return fmt.Errorf("restore merged session compaction state: %w", err)
	}

	mergeTurnNow := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `
INSERT INTO derived_index_changes(idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, created_at, updated_at)
SELECT 'merge:turn:' || ? || ':' || id || ':' || created_at, ?, 'session_turn', CAST(id AS TEXT), 'upsert', ?, ?, ?
FROM session_turns WHERE canonical_user_id = ?
ON CONFLICT(idempotency_key) DO UPDATE SET canonical_user_id = excluded.canonical_user_id,
	entity_kind = excluded.entity_kind, entity_id = excluded.entity_id, operation = excluded.operation,
	state = 'pending', available_at = excluded.available_at, lease_owner = '', lease_until = NULL,
	completed_at = NULL, last_error_code = '', updated_at = excluded.updated_at`, loserID, winnerID, mergeTurnNow, mergeTurnNow, mergeTurnNow, winnerID); err != nil {
		return fmt.Errorf("enqueue merged transcript indexes: %w", err)
	}

	// Version numbers are tenant-local. Keep immutable profile IDs and frozen
	// bindings, but place loser versions after the winner's current sequence.
	if _, err := tx.ExecContext(ctx, `
UPDATE tenant_profile_versions
SET version = version + (SELECT COALESCE(MAX(version), 0) FROM tenant_profile_versions WHERE canonical_user_id = ?),
	canonical_user_id = ?
WHERE canonical_user_id = ?;
INSERT INTO tenant_profile_version_counters (canonical_user_id, version)
SELECT ?, COALESCE(MAX(version), 0) FROM tenant_profile_versions WHERE canonical_user_id = ?
ON CONFLICT(canonical_user_id) DO UPDATE SET version = MAX(version, excluded.version);
DELETE FROM tenant_profile_version_counters WHERE canonical_user_id = ?;
`, winnerID, winnerID, loserID, winnerID, winnerID, loserID); err != nil {
		return fmt.Errorf("move merged tenant profile versions: %w", err)
	}

	// Tenant-scoped idempotency keys become colliding only after ownership moves.
	// Re-key just those collisions, including the row ID so the mapping is stable.
	for _, table := range []string{"memory_candidates", "memory_evidence", "memory_relations", "memory_formation_jobs"} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` AS loser SET idempotency_key = 'merge:' || ? || ':' || loser.idempotency_key || ':' || loser.id WHERE loser.canonical_user_id = ? AND EXISTS (SELECT 1 FROM `+table+` winner WHERE winner.canonical_user_id = ? AND winner.idempotency_key = loser.idempotency_key)`, loserID, loserID, winnerID); err != nil {
			return fmt.Errorf("re-key merged %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE memory_candidates
SET source_session_generation = COALESCE((SELECT new_generation FROM merge_session_generation_map map WHERE map.session_id = memory_candidates.source_session_id AND map.old_generation = memory_candidates.source_session_generation), source_session_generation)
WHERE canonical_user_id = ?;
UPDATE memory_formation_jobs
SET source_session_generation = COALESCE((SELECT new_generation FROM merge_session_generation_map map WHERE map.session_id = memory_formation_jobs.source_session_id AND map.old_generation = memory_formation_jobs.source_session_generation), source_session_generation)
WHERE canonical_user_id = ?;
UPDATE memory_evidence
SET source_session_generation = COALESCE((SELECT new_generation FROM merge_session_generation_map map WHERE map.session_id = memory_evidence.source_session_id AND map.old_generation = memory_evidence.source_session_generation), source_session_generation)
WHERE canonical_user_id = ?;
UPDATE memory_confirmation_presentations
SET session_generation = COALESCE((SELECT new_generation FROM merge_session_generation_map map WHERE map.session_id = memory_confirmation_presentations.session_id AND map.old_generation = memory_confirmation_presentations.session_generation), session_generation)
WHERE canonical_user_id = ?;
`, loserID, loserID, loserID, loserID); err != nil {
		return fmt.Errorf("remap merged formation session generations: %w", err)
	}

	duplicateJoin := `
FROM memory_entries loser
JOIN memory_entries winner
	ON winner.canonical_user_id = ?
	AND winner.scope = loser.scope
		AND ((winner.claim_key != '' AND winner.claim_key NOT LIKE 'legacy:%' AND winner.claim_key = loser.claim_key) OR winner.statement_key = loser.statement_key)
WHERE loser.canonical_user_id = ?`
	duplicateIDs := `SELECT loser.id ` + duplicateJoin
	type mergedMemoryDuplicate struct {
		loserID, winnerID                       int64
		loserConfidence, winnerConfidence       float64
		loserEvidenceCount, winnerEvidenceCount int
		loserAuthority, winnerAuthority         string
		loserProvenance, winnerProvenance       string
		loserSensitivity, winnerSensitivity     string
		loserClaimKey, winnerClaimKey           string
		loserClaimSlot, winnerClaimSlot         string
		loserClaimValue, winnerClaimValue       string
		loserStatement, loserEvidence           string
		loserCategory                           string
	}
	duplicateRows, err := tx.QueryContext(ctx, `SELECT loser.id, winner.id, loser.confidence, winner.confidence, loser.evidence_count, winner.evidence_count, loser.source_authority, winner.source_authority, loser.provenance_type, winner.provenance_type, loser.sensitivity, winner.sensitivity, loser.claim_key, winner.claim_key, loser.claim_slot, winner.claim_slot, loser.claim_value, winner.claim_value, loser.statement, loser.evidence, loser.category `+duplicateJoin, winnerID, loserID)
	if err != nil {
		return fmt.Errorf("read merged confidence duplicates: %w", err)
	}
	var mergedDuplicates []mergedMemoryDuplicate
	for duplicateRows.Next() {
		var duplicate mergedMemoryDuplicate
		if err := duplicateRows.Scan(&duplicate.loserID, &duplicate.winnerID, &duplicate.loserConfidence, &duplicate.winnerConfidence, &duplicate.loserEvidenceCount, &duplicate.winnerEvidenceCount, &duplicate.loserAuthority, &duplicate.winnerAuthority, &duplicate.loserProvenance, &duplicate.winnerProvenance, &duplicate.loserSensitivity, &duplicate.winnerSensitivity, &duplicate.loserClaimKey, &duplicate.winnerClaimKey, &duplicate.loserClaimSlot, &duplicate.winnerClaimSlot, &duplicate.loserClaimValue, &duplicate.winnerClaimValue, &duplicate.loserStatement, &duplicate.loserEvidence, &duplicate.loserCategory); err != nil {
			duplicateRows.Close()
			return fmt.Errorf("scan merged confidence duplicate: %w", err)
		}
		mergedDuplicates = append(mergedDuplicates, duplicate)
	}
	if err := duplicateRows.Close(); err != nil {
		return fmt.Errorf("close merged confidence duplicates: %w", err)
	}
	for _, duplicate := range mergedDuplicates {
		authority, provenance := strongestMemorySource(duplicate.winnerAuthority, duplicate.winnerProvenance, duplicate.loserAuthority, duplicate.loserProvenance)
		claimKey, claimSlot, claimValue := duplicate.winnerClaimKey, duplicate.winnerClaimSlot, duplicate.winnerClaimValue
		if strings.HasPrefix(claimKey, "legacy:") && duplicate.loserClaimKey != "" && !strings.HasPrefix(duplicate.loserClaimKey, "legacy:") {
			claimKey, claimSlot, claimValue = duplicate.loserClaimKey, duplicate.loserClaimSlot, duplicate.loserClaimValue
		}
		useLoser := sourceAuthorityRank(duplicate.loserAuthority) > sourceAuthorityRank(duplicate.winnerAuthority)
		statement, evidence, category := "", "", ""
		if useLoser {
			statement, evidence, category = duplicate.loserStatement, duplicate.loserEvidence, duplicate.loserCategory
		}
		profileApproved := 1
		if authority == string(memoryformation.AuthorityModel) || provenance == string(memoryformation.ProvenanceModelInference) {
			profileApproved = 0
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET confidence = ?, evidence_count = ?, source_authority = ?, provenance_type = ?, sensitivity = ?, profile_approved = ?, claim_key = ?, claim_slot = ?, claim_value = ?, statement = CASE WHEN ? = '' THEN statement ELSE ? END, statement_key = CASE WHEN ? = '' THEN statement_key ELSE ? END, evidence = CASE WHEN ? = '' THEN evidence ELSE ? END, category = CASE WHEN ? = '' THEN category ELSE ? END WHERE id = ? AND canonical_user_id = ?`, aggregateConfidence(duplicate.winnerConfidence, duplicate.loserConfidence), duplicate.winnerEvidenceCount+duplicate.loserEvidenceCount, authority, provenance, strongestSensitivity(duplicate.winnerSensitivity, duplicate.loserSensitivity), profileApproved, claimKey, claimSlot, claimValue, statement, statement, statement, statementKey(statement), evidence, evidence, category, category, duplicate.winnerID, winnerID); err != nil {
			return fmt.Errorf("merge confidence evidence metadata: %w", err)
		}
		if err := enqueueDerivedChangeTx(ctx, tx, winnerID, "memory", duplicate.winnerID, "upsert", "account-merge-confidence:"+mergeTurnNow); err != nil {
			return err
		}
	}
	winnerForDuplicate := `
SELECT winner.id ` + duplicateJoin + ` AND loser.id = memory_events.memory_id`
	if _, err := tx.ExecContext(ctx, `UPDATE memory_events SET canonical_user_id = ?, memory_id = (`+winnerForDuplicate+`) WHERE canonical_user_id = ? AND memory_id IN (`+duplicateIDs+`)`, winnerID, winnerID, loserID, loserID, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to redirect merged memory events: %w", err)
	}
	winnerForSuperseded := `
SELECT winner.id ` + duplicateJoin + ` AND loser.id = memory_entries.supersedes_id`
	if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET supersedes_id = (`+winnerForSuperseded+`) WHERE supersedes_id IN (`+duplicateIDs+`)`, winnerID, loserID, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to redirect merged supersedes references: %w", err)
	}
	winnerForCandidatePublished := `SELECT winner.id ` + duplicateJoin + ` AND loser.id = memory_candidates.published_memory_id`
	winnerForCandidateSupersedes := `SELECT winner.id ` + duplicateJoin + ` AND loser.id = memory_candidates.supersedes_memory_id`
	if _, err := tx.ExecContext(ctx, `
DROP TABLE IF EXISTS temp.merge_candidate_links;
DROP TABLE IF EXISTS temp.merge_memory_links;
CREATE TEMP TABLE merge_candidate_links AS
	SELECT id,
		CASE WHEN published_memory_id IN (`+duplicateIDs+`) THEN (`+winnerForCandidatePublished+`) ELSE published_memory_id END AS published_memory_id,
		CASE WHEN supersedes_memory_id IN (`+duplicateIDs+`) THEN (`+winnerForCandidateSupersedes+`) ELSE supersedes_memory_id END AS supersedes_memory_id,
		source_turn_id
	FROM memory_candidates WHERE canonical_user_id = ?;
CREATE TEMP TABLE merge_memory_links AS
	SELECT id, candidate_id, source_turn_id FROM memory_entries WHERE canonical_user_id = ?;
UPDATE memory_candidates SET published_memory_id = NULL, supersedes_memory_id = NULL, source_turn_id = NULL WHERE canonical_user_id = ?;
UPDATE memory_entries SET candidate_id = NULL, source_turn_id = NULL WHERE canonical_user_id = ?;
`, winnerID, loserID, winnerID, loserID, winnerID, loserID, winnerID, loserID, loserID, loserID, loserID, loserID); err != nil {
		return fmt.Errorf("snapshot merged formation relationships: %w", err)
	}
	winnerForEvidence := `SELECT winner.id ` + duplicateJoin + ` AND loser.id = memory_evidence.memory_id`
	if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence SET memory_id = (`+winnerForEvidence+`) WHERE memory_id IN (`+duplicateIDs+`)`, winnerID, loserID, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to redirect merged memory evidence: %w", err)
	}
	for _, column := range []string{"source_memory_id", "target_memory_id"} {
		winnerForRelation := `SELECT winner.id ` + duplicateJoin + ` AND loser.id = memory_relations.` + column
		if _, err := tx.ExecContext(ctx, `UPDATE memory_relations SET `+column+` = (`+winnerForRelation+`) WHERE `+column+` IN (`+duplicateIDs+`)`, winnerID, loserID, winnerID, loserID); err != nil {
			return fmt.Errorf("failed to redirect merged memory relation %s: %w", column, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_profile_version_facts SET source_memory_id = (SELECT winner.id `+duplicateJoin+` AND loser.id = tenant_profile_version_facts.source_memory_id) WHERE source_memory_id IN (`+duplicateIDs+`)`, winnerID, loserID, winnerID, loserID); err != nil {
		return fmt.Errorf("redirect merged profile facts: %w", err)
	}
	winnerForFormationAudit := `SELECT winner.id ` + duplicateJoin + ` AND loser.id = memory_formation_audit.memory_id`
	if _, err := tx.ExecContext(ctx, `
DROP TABLE IF EXISTS temp.merge_audit_memory_links;
CREATE TEMP TABLE merge_audit_memory_links AS
SELECT id AS audit_id, (`+winnerForFormationAudit+`) AS replacement_memory_id
FROM memory_formation_audit WHERE canonical_user_id = ? AND memory_id IN (`+duplicateIDs+`)
`, winnerID, loserID, loserID, winnerID, loserID); err != nil {
		return fmt.Errorf("snapshot merged formation audit memory: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.merge_duplicate_memory_ids; CREATE TEMP TABLE merge_duplicate_memory_ids AS SELECT id FROM memory_entries WHERE id IN (`+duplicateIDs+`)`, winnerID, loserID); err != nil {
		return fmt.Errorf("snapshot duplicate merged memories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_entries WHERE id IN (SELECT id FROM merge_duplicate_memory_ids)`); err != nil {
		return fmt.Errorf("failed to delete duplicate memories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET canonical_user_id = ? WHERE canonical_user_id = ?`, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to move merged memory candidates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET canonical_user_id = ? WHERE canonical_user_id = ?`, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to move merged memories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_events SET canonical_user_id = ? WHERE canonical_user_id = ?`, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to move merged memory events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence SET canonical_user_id = ? WHERE canonical_user_id = ?`, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to move merged memory evidence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_relations SET canonical_user_id = ? WHERE canonical_user_id = ?`, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to move merged memory relations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_formation_jobs SET canonical_user_id = ? WHERE canonical_user_id = ?`, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to move merged memory formation jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_formation_jobs SET state = 'retry', lease_until = NULL WHERE canonical_user_id = ? AND state = 'running'`, winnerID); err != nil {
		return fmt.Errorf("reset merged memory formation leases: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_confirmation_presentations SET canonical_user_id = ? WHERE canonical_user_id = ?`, winnerID, loserID); err != nil {
		return fmt.Errorf("move merged confirmation presentations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_formation_audit (
	canonical_user_id, idempotency_key, event_type, candidate_id, memory_id, job_id,
	request_id, session_id, turn_id, actor_type, actor_id, created_at, metadata,
	content_expires_at, redacted_at
)
SELECT ?, CASE WHEN EXISTS (SELECT 1 FROM memory_formation_audit winner WHERE winner.canonical_user_id = ? AND winner.idempotency_key = audit.idempotency_key)
	THEN 'merge:' || ? || ':' || audit.idempotency_key || ':' || audit.id ELSE audit.idempotency_key END,
	audit.event_type, audit.candidate_id,
	COALESCE(links.replacement_memory_id, audit.memory_id), audit.job_id,
	audit.request_id, audit.session_id, audit.turn_id, audit.actor_type, audit.actor_id,
	audit.created_at, audit.metadata, audit.content_expires_at, audit.redacted_at
FROM memory_formation_audit audit
LEFT JOIN merge_audit_memory_links links ON links.audit_id = audit.id
WHERE audit.canonical_user_id = ?;
DROP TABLE merge_audit_memory_links;
`, winnerID, winnerID, loserID, loserID); err != nil {
		return fmt.Errorf("copy merged memory formation audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE memory_candidates
SET published_memory_id = (SELECT published_memory_id FROM merge_candidate_links links WHERE links.id = memory_candidates.id),
	supersedes_memory_id = (SELECT supersedes_memory_id FROM merge_candidate_links links WHERE links.id = memory_candidates.id),
	source_turn_id = (SELECT source_turn_id FROM merge_candidate_links links WHERE links.id = memory_candidates.id)
WHERE canonical_user_id = ? AND id IN (SELECT id FROM merge_candidate_links);
UPDATE memory_entries
SET candidate_id = (SELECT candidate_id FROM merge_memory_links links WHERE links.id = memory_entries.id),
	source_turn_id = (SELECT source_turn_id FROM merge_memory_links links WHERE links.id = memory_entries.id)
WHERE canonical_user_id = ? AND id IN (SELECT id FROM merge_memory_links);
DROP TABLE merge_candidate_links;
DROP TABLE merge_memory_links;
`, winnerID, winnerID); err != nil {
		return fmt.Errorf("restore merged formation relationships: %w", err)
	}
	mergeNow := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `
INSERT INTO derived_index_changes(idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, created_at, updated_at)
SELECT 'merge:memory:' || ? || ':' || id || ':' || updated_at, ?, 'memory', CAST(id AS TEXT), 'upsert', ?, ?, ?
FROM memory_entries WHERE canonical_user_id = ?
ON CONFLICT(idempotency_key) DO UPDATE SET canonical_user_id = excluded.canonical_user_id,
	entity_kind = excluded.entity_kind, entity_id = excluded.entity_id, operation = excluded.operation,
	state = 'pending', available_at = excluded.available_at, lease_owner = '', lease_until = NULL,
	completed_at = NULL, last_error_code = '', updated_at = excluded.updated_at`, loserID, winnerID, mergeNow, mergeNow, mergeNow, winnerID); err != nil {
		return fmt.Errorf("enqueue merged memory indexes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO derived_index_changes(idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, created_at, updated_at)
SELECT 'merge:memory-delete:' || ? || ':' || id, ?, 'memory', CAST(id AS TEXT), 'delete', ?, ?, ?
FROM merge_duplicate_memory_ids
WHERE 1
ON CONFLICT(idempotency_key) DO UPDATE SET canonical_user_id = excluded.canonical_user_id,
	entity_kind = excluded.entity_kind, entity_id = excluded.entity_id, operation = excluded.operation,
	state = 'pending', available_at = excluded.available_at, lease_owner = '', lease_until = NULL,
	completed_at = NULL, last_error_code = '', updated_at = excluded.updated_at;
DROP TABLE merge_duplicate_memory_ids`, loserID, winnerID, mergeNow, mergeNow, mergeNow); err != nil {
		return fmt.Errorf("enqueue duplicate merged memory index deletion: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tenant_session_generations (canonical_user_id, session_id, generation)
SELECT ?, session_id, MAX(generation)
FROM (
	SELECT session_id, generation FROM tenant_session_generations WHERE canonical_user_id IN (?, ?)
	UNION ALL SELECT session_id, session_generation FROM session_turns WHERE canonical_user_id = ?
	UNION ALL SELECT session_id, session_generation FROM session_summaries WHERE canonical_user_id = ?
	UNION ALL SELECT session_id, session_generation FROM session_compaction_jobs WHERE canonical_user_id = ?
	UNION ALL SELECT session_id, generation FROM merge_tenant_sessions
)
GROUP BY session_id
ON CONFLICT(canonical_user_id, session_id) DO UPDATE SET generation = MAX(generation, excluded.generation)
`, winnerID, winnerID, loserID, winnerID, winnerID, winnerID); err != nil {
		return fmt.Errorf("failed to preserve merged session generations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tenant_session_generations WHERE canonical_user_id = ?`, loserID); err != nil {
		return fmt.Errorf("failed to remove merged session generations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tenant_sessions (canonical_user_id, session_id, generation, profile_version_id, started_at, last_seen_at, expires_at)
SELECT ?, session_id, generation, profile_version_id, started_at, last_seen_at, expires_at
FROM merge_tenant_sessions
WHERE 1
ON CONFLICT(canonical_user_id, session_id) DO UPDATE SET
	generation = CASE WHEN excluded.generation > tenant_sessions.generation THEN excluded.generation ELSE tenant_sessions.generation END,
	profile_version_id = CASE WHEN excluded.generation > tenant_sessions.generation THEN excluded.profile_version_id ELSE tenant_sessions.profile_version_id END,
	started_at = CASE WHEN excluded.generation > tenant_sessions.generation THEN excluded.started_at ELSE tenant_sessions.started_at END,
	last_seen_at = MAX(tenant_sessions.last_seen_at, excluded.last_seen_at),
	expires_at = MAX(tenant_sessions.expires_at, excluded.expires_at);
`, winnerID); err != nil {
		return fmt.Errorf("restore merged tenant sessions: %w", err)
	}

	now := formatTime(time.Now())
	createdAt := now
	err = tx.QueryRowContext(ctx, `
SELECT created_at FROM user_memory_profiles
WHERE canonical_user_id IN (?, ?)
ORDER BY CASE canonical_user_id WHEN ? THEN 0 ELSE 1 END
LIMIT 1`, winnerID, loserID, winnerID).Scan(&createdAt)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to read merged memory profile: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_memory_profiles WHERE canonical_user_id = ?`, loserID); err != nil {
		return fmt.Errorf("failed to remove merged memory profile: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO user_memory_profiles (canonical_user_id, intro, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(canonical_user_id) DO UPDATE SET intro = excluded.intro, updated_at = excluded.updated_at
`, winnerID, strings.TrimSpace(intro), createdAt, now); err != nil {
		return fmt.Errorf("failed to upsert merged memory profile: %w", err)
	}
	if _, err := refreshProfileTx(ctx, tx, winnerID, time.Now().UTC()); err != nil {
		return fmt.Errorf("publish unified merged tenant profile: %w", err)
	}

	// Existing outbox work remains useful after a merge. Move it and make any
	// in-flight lease retryable under the winner before queuing reconciliation work.
	if _, err := tx.ExecContext(ctx, `
UPDATE derived_index_changes
SET canonical_user_id = ?,
	state = CASE WHEN state = 'processing' THEN 'failed' ELSE state END,
	lease_owner = CASE WHEN state = 'processing' THEN '' ELSE lease_owner END,
	lease_until = CASE WHEN state = 'processing' THEN NULL ELSE lease_until END,
	available_at = CASE WHEN state = 'processing' THEN ? ELSE available_at END,
	updated_at = CASE WHEN state = 'processing' THEN ? ELSE updated_at END
WHERE canonical_user_id = ?;
`, winnerID, mergeNow, mergeNow, loserID); err != nil {
		return fmt.Errorf("move merged derived index changes: %w", err)
	}

	// Preserve completed privacy history under the surviving tenant, but never
	// allow a challenge created before the merge to authorize deletion of the
	// winner's newly combined data.
	if _, err := tx.ExecContext(ctx, `
UPDATE privacy_operations
SET target_user_id = ?,
	status = CASE WHEN status = 'pending' THEN 'expired' WHEN status = 'running' THEN 'failed' ELSE status END,
	challenge_hash = '', challenge_expires_at = NULL,
	completed_at = CASE WHEN status IN ('pending', 'running') THEN COALESCE(completed_at, ?) ELSE completed_at END,
	updated_at = CASE WHEN status IN ('pending', 'running') THEN ? ELSE updated_at END,
	last_error_code = CASE WHEN status IN ('pending', 'running') THEN 'account_merged' ELSE last_error_code END
WHERE target_user_id = ?;
`, winnerID, mergeNow, mergeNow, loserID); err != nil {
		return fmt.Errorf("move merged privacy operations: %w", err)
	}

	// Audit rows cannot change tenant. Copying above preserves every field; marking
	// the losing account as erasing permits removal of only those copied rows.
	if _, err := tx.ExecContext(ctx, `UPDATE account_users SET lifecycle_state = 'erasing' WHERE canonical_user_id = ?; DELETE FROM memory_formation_audit WHERE canonical_user_id = ?`, loserID, loserID); err != nil {
		return fmt.Errorf("retire merged formation audit ownership: %w", err)
	}

	var remaining int
	if err := tx.QueryRowContext(ctx, `
SELECT SUM(row_count) FROM (
	SELECT COUNT(*) row_count FROM user_memory_profiles WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM memory_entries WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM memory_events WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM tenant_profile_versions WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM tenant_profile_version_counters WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM tenant_sessions WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM tenant_session_generations WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM memory_confirmation_presentations WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM memory_evidence WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM memory_relations WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM memory_formation_jobs WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM memory_formation_audit WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM session_summaries WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM session_summary_sources WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM session_compaction_jobs WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM derived_index_changes WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM privacy_operations WHERE target_user_id = ?
)
`, loserID, loserID, loserID, loserID, loserID, loserID, loserID, loserID, loserID,
		loserID, loserID, loserID, loserID, loserID, loserID, loserID, loserID, loserID, loserID).Scan(&remaining); err != nil {
		return fmt.Errorf("verify merged tenant ownership: %w", err)
	}
	if remaining != 0 {
		return fmt.Errorf("verify merged tenant ownership: %d loser-owned rows remain", remaining)
	}
	return nil
}

// MergeUsersTx moves user memory through a caller-owned transaction.
func (s *Store) MergeUsersTx(ctx context.Context, tx *sql.Tx, winnerID, loserID, intro string) error {
	return MergeUsersTx(ctx, tx, winnerID, loserID, intro)
}

// SaveMemory creates or updates a scoped memory entry.
func (s *Store) SaveMemory(ctx context.Context, userID string, req SaveRequest) (MemoryEntry, error) {
	if err := s.ensureAccountUser(userID); err != nil {
		return MemoryEntry{}, err
	}
	unlock := s.lockUsers(userID)
	defer unlock()
	statement := strings.TrimSpace(req.Statement)
	if statement == "" {
		return MemoryEntry{}, fmt.Errorf("memory statement is required")
	}
	evidence := strings.TrimSpace(req.Evidence)
	if evidence == "" {
		evidence = "Stored from user interaction"
	}
	scope := normalizeScope(req.Scope)
	category := normalizeCategory(req.Category)
	importance := clampInt(req.Importance, 1, 5, 3)
	confidence := req.Confidence
	if confidence <= 0 || confidence > 1 {
		confidence = 0.8
	}
	now := time.Now().UTC()
	var expiresAt *time.Time
	if scope == ScopeShortTerm {
		ttl := req.TTL
		if ttl <= 0 {
			ttl = DefaultShortTermTTL
		}
		exp := now.Add(ttl).UTC()
		expiresAt = &exp
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("begin legacy memory publication: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	var supersedesID int64
	if strings.TrimSpace(req.Supersedes) != "" {
		err := tx.QueryRowContext(ctx, `SELECT id FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND statement_key = ? AND status = 'active'`, userID, scope, statementKey(req.Supersedes)).Scan(&supersedesID)
		if err != nil && err != sql.ErrNoRows {
			return MemoryEntry{}, fmt.Errorf("resolve superseded memory: %w", err)
		}
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO memory_entries (canonical_user_id, scope, category, statement, statement_key, evidence, confidence, importance, status, source_session_id, created_at, updated_at, expires_at, supersedes_id, embedding_model, embedding_dim, profile_approved, provenance_type, source_authority, formation_mode, sensitivity, approval_state, approved_at, approved_by, valid_from, valid_until)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?, 1, 'legacy_import', 'unknown', 'legacy_direct_save', 'unknown', 'approved', ?, 'legacy_api', ?, ?)
ON CONFLICT(canonical_user_id, scope, statement_key) DO UPDATE SET
	category = excluded.category,
	statement = excluded.statement,
	evidence = excluded.evidence,
	confidence = excluded.confidence,
	importance = excluded.importance,
	status = 'active',
	source_session_id = excluded.source_session_id,
	updated_at = excluded.updated_at,
	expires_at = excluded.expires_at,
	supersedes_id = excluded.supersedes_id,
	embedding_model = excluded.embedding_model,
	embedding_dim = excluded.embedding_dim,
	profile_approved = 1,
	approval_state = 'approved',
	approved_at = excluded.approved_at,
	approved_by = excluded.approved_by,
	valid_from = excluded.valid_from,
	valid_until = excluded.valid_until
RETURNING id
	`, userID, scope, category, statement, statementKey(statement), evidence, confidence, importance,
		strings.TrimSpace(req.SourceSessionID), formatTime(now), formatTime(now), nullableTime(expiresAt), nullableID(supersedesID), "", 0,
		formatTime(now), formatTime(now), nullableTime(expiresAt)).Scan(&id)
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("failed to save memory for %q: %w", userID, err)
	}
	if supersedesID == id && supersedesID > 0 {
		return MemoryEntry{}, fmt.Errorf("memory cannot supersede itself")
	}
	if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", id, "upsert", "save:"+formatTime(now)); err != nil {
		return MemoryEntry{}, err
	}
	if supersedesID > 0 {
		result, err := tx.ExecContext(ctx, `UPDATE memory_entries SET status = 'superseded', invalidated_at = ?, invalidation_reason = 'legacy_replacement', updated_at = ? WHERE id = ? AND canonical_user_id = ? AND status = 'active'`, formatTime(now), formatTime(now), supersedesID, userID)
		if err != nil {
			return MemoryEntry{}, fmt.Errorf("supersede legacy memory: %w", err)
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return MemoryEntry{}, fmt.Errorf("superseded legacy memory is no longer active")
		}
		if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", supersedesID, "delete", "supersede:"+formatTime(now)); err != nil {
			return MemoryEntry{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_relations(canonical_user_id, idempotency_key, relation_type, source_memory_id, target_memory_id, created_at) VALUES (?, ?, 'supersedes', ?, ?, ?) ON CONFLICT(canonical_user_id, idempotency_key) DO NOTHING`, userID, formationKey("legacy-supersedes", id, supersedesID), id, supersedesID, formatTime(now)); err != nil {
			return MemoryEntry{}, err
		}
	}
	meta := requestctx.MetadataFromContext(ctx)
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_events(canonical_user_id, memory_id, event_type, request_id, session_id, created_at, metadata) VALUES (?, ?, 'updated', ?, ?, ?, '{"source":"legacy_api"}')`, userID, id, meta.RequestID, firstNonEmptyFormation(meta.SessionID, req.SourceSessionID), formatTime(now)); err != nil {
		return MemoryEntry{}, fmt.Errorf("record legacy memory event: %w", err)
	}
	if err := insertFormationAuditTx(ctx, tx, userID, formationKey("legacy-save", id, formatTime(now)), "memory.legacy_saved", 0, id, 0, FormationSource{RequestID: meta.RequestID, SessionID: firstNonEmptyFormation(meta.SessionID, req.SourceSessionID)}, "legacy_api", "compatibility save"); err != nil {
		return MemoryEntry{}, err
	}
	if _, err := refreshProfileTx(ctx, tx, userID, now); err != nil {
		return MemoryEntry{}, fmt.Errorf("advance profile after legacy save: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MemoryEntry{}, fmt.Errorf("commit legacy memory publication: %w", err)
	}
	s.signalDerivedIndex()
	entry, err := s.EntryByID(id)
	if err != nil {
		return MemoryEntry{}, err
	}
	if entry.UserID != userID {
		return MemoryEntry{}, fmt.Errorf("saved memory ownership mismatch")
	}
	return entry, nil
}

// EntryByID reads a memory entry by ID.
func (s *Store) EntryByID(id int64) (MemoryEntry, error) {
	rows, err := s.sql.Query(`SELECT id, canonical_user_id, scope, category, statement, evidence, confidence, importance, status, source_session_id, created_at, updated_at, last_used_at, expires_at, COALESCE(supersedes_id, 0), embedding_model, embedding_dim, provenance_type, source_authority, approval_state, sensitivity, claim_key, claim_slot, claim_value, evidence_count FROM memory_entries WHERE id = ?`, id)
	if err != nil {
		return MemoryEntry{}, err
	}
	defer rows.Close()
	if rows.Next() {
		return scanMemoryEntry(rows)
	}
	return MemoryEntry{}, sql.ErrNoRows
}

// Search returns active memories matching the requested filters.
func (s *Store) Search(ctx context.Context, userID, scope, category, query string, limit int) ([]MemoryEntry, error) {
	query = strings.TrimSpace(query)
	if query != "" {
		results, stats := s.Recall(ctx, userID, query, RecallRequest{Scope: scope, Category: category, TopK: limit, MinRelevance: defaultRecallMinRelevance, ExplicitSearch: true})
		if !stats.LexicalAvailable && !stats.SemanticAvailable {
			return nil, fmt.Errorf("durable memory retrieval indexes unavailable")
		}
		s.RecordRecallUsage(ctx, userID, results)
		return recallResultsToEntries(results), nil
	}
	return s.listActiveMemories(userID, scope, category, limit)
}

func (s *Store) listActiveMemories(userID, scope, category string, limit int) ([]MemoryEntry, error) {
	if limit <= 0 {
		limit = 8
	}
	if limit > 25 {
		limit = 25
	}
	if err := s.expireOldMemories(); err != nil {
		return nil, err
	}
	normalizedScope := normalizeOptionalScope(scope)
	normalizedCategory := normalizeOptionalCategory(category)
	entries, err := s.activeEntries(userID, normalizedScope, normalizedCategory)
	if err != nil || len(entries) == 0 {
		return entries, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Importance == entries[j].Importance {
			return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
		}
		return entries[i].Importance > entries[j].Importance
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	ids := make([]any, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
		s.recordEvent(entry.UserID, entry.ID, "retrieved", "", "", "")
	}
	if len(ids) > 0 {
		now := formatTime(time.Now())
		for _, id := range ids {
			_, _ = s.sql.Exec(`UPDATE memory_entries SET last_used_at = ? WHERE id = ?`, now, id)
		}
	}
	return entries, nil
}

// ListMemories returns active memories without semantic ranking.
func (s *Store) ListMemories(userID, scope, category string, limit int) ([]MemoryEntry, error) {
	return s.Search(context.Background(), userID, scope, category, "", limit)
}

// Forget marks one or more user memories deleted.
func (s *Store) Forget(userID, target, scope string) (int64, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return 0, fmt.Errorf("memory target is required")
	}
	unlock := s.lockUsers(userID)
	defer unlock()
	tx, err := s.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin memory deletion: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	if strings.EqualFold(target, "all") {
		now := formatTime(time.Now())
		ids, err := memoryIDsTx(tx, `SELECT id FROM memory_entries WHERE canonical_user_id = ? AND status != 'deleted'`, userID)
		if err != nil {
			return 0, err
		}
		res, err := tx.Exec(`UPDATE memory_entries SET status = 'deleted', statement = '', statement_key = 'erased:' || id, claim_key = 'erased:' || id, claim_slot = '', claim_value = '', evidence = '', erased_at = ?, erasure_reason = 'user_forget', updated_at = ? WHERE canonical_user_id = ? AND status != 'deleted'`, now, now, userID)
		if err != nil {
			return 0, fmt.Errorf("failed to delete memories for %q: %w", userID, err)
		}
		count, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if err := deleteForgottenProfileVersionsTx(tx, userID); err != nil {
			return 0, err
		}
		for _, id := range ids {
			if err := enqueueDerivedChangeTx(context.Background(), tx, userID, "memory", id, "delete", "forget:"+now); err != nil {
				return 0, err
			}
		}
		if err := eraseForgottenFormationTx(tx, userID, true); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		s.signalDerivedIndex()
		return count, nil
	}
	now := formatTime(time.Now())
	stmt := `UPDATE memory_entries SET status = 'deleted', statement = '', statement_key = 'erased:' || id, claim_key = 'erased:' || id, claim_slot = '', claim_value = '', evidence = '', erased_at = ?, erasure_reason = 'user_forget', updated_at = ? WHERE canonical_user_id = ? AND statement_key = ? AND status != 'deleted'`
	args := []any{now, now, userID, statementKey(target)}
	selectStmt := `SELECT id FROM memory_entries WHERE canonical_user_id = ? AND statement_key = ? AND status != 'deleted'`
	selectArgs := []any{userID, statementKey(target)}
	if normalizeOptionalScope(scope) != "" {
		stmt += ` AND scope = ?`
		args = append(args, normalizeScope(scope))
		selectStmt += ` AND scope = ?`
		selectArgs = append(selectArgs, normalizeScope(scope))
	}
	ids, err := memoryIDsTx(tx, selectStmt, selectArgs...)
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to delete memory for %q: %w", userID, err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := deleteForgottenProfileVersionsTx(tx, userID); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := enqueueDerivedChangeTx(context.Background(), tx, userID, "memory", id, "delete", "forget:"+now); err != nil {
			return 0, err
		}
	}
	if err := eraseForgottenFormationTx(tx, userID, false); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.signalDerivedIndex()
	return count, nil
}

func memoryIDsTx(tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.Query(query, args...)
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

func eraseForgottenFormationTx(tx *sql.Tx, userID string, eraseAll bool) error {
	now := formatTime(time.Now().UTC())
	if eraseAll {
		if _, err := tx.Exec(`
UPDATE memory_evidence SET content = '', correlation_key = '' WHERE canonical_user_id = ?;
UPDATE memory_formation_jobs SET extraction_payload = '' WHERE canonical_user_id = ?;
UPDATE memory_candidates
SET statement = '', statement_key = 'erased:' || id, claim_key = 'erased:' || id, claim_slot = '', claim_value = '', evidence_summary = '', state = 'rejected',
	decision_reason = 'user_forget_all', decided_at = ?, decided_by = 'user', updated_at = ?
WHERE canonical_user_id = ?;
`, userID, userID, now, now, userID); err != nil {
			return fmt.Errorf("erase all tenant memory formation content: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(`
UPDATE memory_evidence
SET content = ''
WHERE canonical_user_id = ? AND (
	memory_id IN (SELECT id FROM memory_entries WHERE canonical_user_id = ? AND status = 'deleted')
	OR candidate_id IN (
		SELECT candidate_id FROM memory_entries
		WHERE canonical_user_id = ? AND status = 'deleted' AND candidate_id IS NOT NULL
	)
);

UPDATE memory_formation_jobs
SET extraction_payload = ''
WHERE canonical_user_id = ? AND source_turn_id IN (
	SELECT source_turn_id FROM memory_candidates
	WHERE canonical_user_id = ? AND published_memory_id IN (
		SELECT id FROM memory_entries WHERE canonical_user_id = ? AND status = 'deleted'
	) AND source_turn_id IS NOT NULL
);

UPDATE memory_candidates
SET statement = '', statement_key = 'erased:' || id, claim_key = 'erased:' || id, claim_slot = '', claim_value = '', evidence_summary = '', state = 'rejected',
	decision_reason = 'user_forget', decided_at = ?, decided_by = 'user', updated_at = ?
WHERE canonical_user_id = ? AND published_memory_id IN (
	SELECT id FROM memory_entries WHERE canonical_user_id = ? AND status = 'deleted'
);
`, userID, userID, userID, userID, userID, userID, now, now, userID, userID); err != nil {
		return fmt.Errorf("erase forgotten memory formation content: %w", err)
	}
	return nil
}

func deleteForgottenProfileVersionsTx(tx *sql.Tx, userID string) error {
	if _, err := tx.Exec(`
DELETE FROM tenant_profile_versions
WHERE canonical_user_id = ? AND id IN (
	SELECT facts.profile_version_id
	FROM tenant_profile_version_facts facts
	JOIN memory_entries entries ON entries.id = facts.source_memory_id
	WHERE entries.canonical_user_id = ? AND entries.status IN ('deleted', 'expired')
)
`, userID, userID); err != nil {
		return fmt.Errorf("failed to remove forgotten profile snapshots: %w", err)
	}
	return nil
}

func (s *Store) lockUsers(userIDs ...string) func() {
	ids := uniqueStrings(userIDs)
	sort.Strings(ids)
	s.mutationMu.Lock()
	if s.userLocks == nil {
		s.userLocks = make(map[string]*sync.Mutex)
	}
	locks := make([]*sync.Mutex, 0, len(ids))
	for _, userID := range ids {
		lock := s.userLocks[userID]
		if lock == nil {
			lock = &sync.Mutex{}
			s.userLocks[userID] = lock
		}
		locks = append(locks, lock)
	}
	s.mutationMu.Unlock()
	for _, lock := range locks {
		lock.Lock()
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
}

// Read renders all active memories for compatibility with older callers/tests.
func (s *Store) Read(userID string) (string, error) {
	entries, err := s.ListMemories(userID, "", "", 100)
	if err != nil {
		return "", err
	}
	intro, _ := s.ReadIntro(userID)
	return RenderMemory(intro, entries), nil
}

// ReadCategory renders active memories for one category.
func (s *Store) ReadCategory(userID, category string) (string, error) {
	entries, err := s.ListMemories(userID, "", category, 100)
	if err != nil || len(entries) == 0 {
		return "", err
	}
	return RenderMemory("", entries), nil
}

// SetWithContext stores a long-term memory for legacy callers and migrations.
func (s *Store) SetWithContext(ctx context.Context, userID, statement, evidence, category string) error {
	_, err := s.SaveMemory(ctx, userID, SaveRequest{Scope: ScopeLongTerm, Category: category, Statement: statement, Evidence: evidence, Confidence: 0.9, Importance: 3})
	return err
}

// Delete marks a specific memory deleted.
func (s *Store) Delete(userID, statement string) error {
	_, err := s.Forget(userID, statement, "")
	return err
}

// DeleteAll marks all user memories deleted.
func (s *Store) DeleteAll(userID string) error {
	_, err := s.Forget(userID, "all", "")
	return err
}

// AppendSessionTurn stores a completed session exchange.
func (s *Store) AppendSessionTurn(ctx context.Context, sessionID, userID, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	_, err := s.appendSessionTurn(ctx, sessionID, userID, 1, userText, assistantText, toolNames, ttl, false, true)
	return err
}

// AppendSessionTurnForGeneration stores a completed exchange in one frozen session generation.
func (s *Store) AppendSessionTurnForGeneration(ctx context.Context, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	_, err := s.appendSessionTurn(ctx, sessionID, userID, generation, userText, assistantText, toolNames, ttl, true, true)
	return err
}

// AppendSessionTurnForGenerationResult stores a completed exchange and returns
// the authoritative inserted turn for post-response formation work.
func (s *Store) AppendSessionTurnForGenerationResult(ctx context.Context, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) (StoredSessionTurn, error) {
	return s.appendSessionTurn(ctx, sessionID, userID, generation, userText, assistantText, toolNames, ttl, true, false)
}

func (s *Store) appendSessionTurn(ctx context.Context, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration, validateGeneration, markDelivered bool) (StoredSessionTurn, error) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(assistantText) == "" {
		return StoredSessionTurn{}, nil
	}
	if generation <= 0 {
		generation = 1
	}
	ttl = s.sessionTTL(ttl)
	if err := s.ensureAccountUser(userID); err != nil {
		return StoredSessionTurn{}, err
	}
	now := time.Now().UTC()
	requestID := requestctx.MetadataFromContext(ctx).RequestID
	var deliveredAt any
	if markDelivered {
		deliveredAt = formatTime(now)
	}
	var expires *time.Time
	if ttl > 0 {
		exp := now.Add(ttl).UTC()
		expires = &exp
	}
	query := `
INSERT INTO session_turns (session_id, canonical_user_id, session_generation, user_text, assistant_text, tool_names, importance, created_at, expires_at, source_request_id, delivered_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	RETURNING id`
	args := []any{sessionID, userID, generation, strings.TrimSpace(userText), strings.TrimSpace(assistantText), strings.Join(uniqueStrings(toolNames), ","), 2, formatTime(now), nullableTime(expires), requestID, deliveredAt}
	if validateGeneration {
		query = `
INSERT INTO session_turns (session_id, canonical_user_id, session_generation, user_text, assistant_text, tool_names, importance, created_at, expires_at, source_request_id, delivered_at)
SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
WHERE EXISTS (
	SELECT 1 FROM tenant_sessions WHERE canonical_user_id = ? AND session_id = ? AND generation = ?
	)
	RETURNING id`
		args = append(args, userID, sessionID, generation)
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return StoredSessionTurn{}, fmt.Errorf("begin session turn write: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	var id int64
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		if validateGeneration && err == sql.ErrNoRows {
			return StoredSessionTurn{}, nil
		}
		return StoredSessionTurn{}, fmt.Errorf("failed to append session turn: %w", err)
	}
	if err := enqueueDerivedChangeTx(ctx, tx, userID, "session_turn", id, "upsert", "append:"+formatTime(now)); err != nil {
		return StoredSessionTurn{}, err
	}
	if err := tx.Commit(); err != nil {
		return StoredSessionTurn{}, fmt.Errorf("commit session turn write: %w", err)
	}
	s.signalDerivedIndex()
	return StoredSessionTurn{ID: id, UserID: userID, SessionID: sessionID, Generation: generation, UserText: strings.TrimSpace(userText)}, nil
}

// RecentSessionTurns returns a user's newest completed session exchanges, newest first.
func (s *Store) RecentSessionTurns(userID, sessionID string, offset int, count int) ([]SessionTurn, error) {
	return s.recentSessionTurns(context.Background(), userID, sessionID, 0, offset, count)
}

// RecentSessionTurnsForGeneration returns turns from exactly one session generation.
func (s *Store) RecentSessionTurnsForGeneration(userID, sessionID string, generation, offset, count int) ([]SessionTurn, error) {
	return s.recentSessionTurns(context.Background(), userID, sessionID, generation, offset, count)
}

// RecentCompletedExchanges returns complete newest-first exchanges for one tenant session generation.
func (s *Store) RecentCompletedExchanges(ctx context.Context, userID, sessionID string, generation, limit int) ([]SessionTurn, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" || generation <= 0 {
		return nil, fmt.Errorf("recent session exchanges require user, session, and generation")
	}
	return s.recentSessionTurns(ctx, userID, sessionID, generation, 1, limit)
}

func (s *Store) recentSessionTurns(ctx context.Context, userID, sessionID string, generation, offset int, count int) ([]SessionTurn, error) {
	if offset < 1 {
		offset = 1
	}
	if count < 1 {
		count = 1
	}
	if count > 100 {
		count = 100
	}
	query := `SELECT id, session_id, canonical_user_id, session_generation, user_text, assistant_text, tool_names, importance, topic_tags, created_at, expires_at FROM session_turns WHERE canonical_user_id = ? AND session_id = ? AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))`
	args := []any{userID, sessionID, formatTime(time.Now())}
	if generation > 0 {
		query += ` AND session_generation = ?`
		args = append(args, generation)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, count, offset-1)
	rows, err := s.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to read session turns: %w", err)
	}
	defer rows.Close()
	turns := []SessionTurn{}
	for rows.Next() {
		turn, err := scanSessionTurn(rows)
		if err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

// BuildContext retrieves and formats the legacy automatic session context block.
// Production prompt assembly retrieves session turns and durable recall separately.
func (s *Store) BuildContext(ctx context.Context, userID, sessionID, query string, opts ContextOptions) (RetrievedContext, error) {
	if strings.TrimSpace(userID) == "" {
		return RetrievedContext{}, nil
	}
	if opts.RecentTurns <= 0 {
		opts.RecentTurns = 4
	}
	if opts.ContextBudgetChars <= 0 {
		opts.ContextBudgetChars = 12000
	}

	var recent []SessionTurn
	var err error
	if opts.Generation > 0 {
		recent, err = s.RecentSessionTurnsForGeneration(userID, sessionID, opts.Generation, 1, opts.RecentTurns)
	} else {
		recent, err = s.RecentSessionTurns(userID, sessionID, 1, opts.RecentTurns)
	}
	if err != nil {
		return RetrievedContext{}, err
	}

	block := s.renderContextBlock(recent, opts.ContextBudgetChars)
	var toolNames []string
	for _, turn := range recent {
		toolNames = append(toolNames, turn.ToolNames...)
	}
	return RetrievedContext{Block: block, RecentTurnCount: len(recent), RecentToolNames: uniqueStrings(toolNames)}, nil
}

func (s *Store) renderContextBlock(recent []SessionTurn, maxChars int) string {
	var b strings.Builder
	b.WriteString("# Retrieved Memory\n")
	if len(recent) > 0 {
		b.WriteString("\n## Recent Exchanges\n")
		writeTurns(&b, recent)
	}
	text := strings.TrimSpace(b.String())
	if text == "# Retrieved Memory" {
		return ""
	}
	if len(text) > maxChars {
		text = text[:maxChars] + "..."
	}
	return text
}

func writeEntries(b *strings.Builder, entries []MemoryEntry) {
	for _, entry := range entries {
		fmt.Fprintf(b, "- [%s/%s, importance %d] %s\n", entry.Scope, entry.Category, entry.Importance, strings.TrimSpace(entry.Statement))
		if strings.TrimSpace(entry.Evidence) != "" {
			fmt.Fprintf(b, "  Evidence: %s\n", strings.TrimSpace(entry.Evidence))
		}
	}
}

func writeTurns(b *strings.Builder, turns []SessionTurn) {
	for i := len(turns) - 1; i >= 0; i-- {
		turn := turns[i]
		fmt.Fprintf(b, "User: %s\nAssistant: %s\n", strings.TrimSpace(turn.UserText), strings.TrimSpace(turn.AssistantText))
		if len(turn.ToolNames) > 0 {
			fmt.Fprintf(b, "Tools used: %s\n", strings.Join(turn.ToolNames, ", "))
		}
		b.WriteString("\n")
	}
}

func (s *Store) activeEntries(userID, scope, category string) ([]MemoryEntry, error) {
	query := `SELECT id, canonical_user_id, scope, category, statement, evidence, confidence, importance, status, source_session_id, created_at, updated_at, last_used_at, expires_at, COALESCE(supersedes_id, 0), embedding_model, embedding_dim, provenance_type, source_authority, approval_state, sensitivity, claim_key, claim_slot, claim_value, evidence_count FROM memory_entries WHERE canonical_user_id = ? AND status = 'active' AND approval_state = 'approved' AND (expires_at IS NULL OR expires_at > ?)`
	args := []any{userID, formatTime(time.Now().UTC())}
	if scope != "" {
		query += ` AND scope = ?`
		args = append(args, scope)
	}
	if category != "" {
		query += ` AND category = ?`
		args = append(args, category)
	}
	query += ` ORDER BY importance DESC, updated_at DESC`
	rows, err := s.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to read memories: %w", err)
	}
	defer rows.Close()
	entries := []MemoryEntry{}
	for rows.Next() {
		entry, err := scanMemoryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func scanMemoryEntry(rows interface{ Scan(...any) error }) (MemoryEntry, error) {
	var entry MemoryEntry
	var created, updated string
	var lastUsed, expires sql.NullString
	if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Scope, &entry.Category, &entry.Statement, &entry.Evidence, &entry.Confidence, &entry.Importance, &entry.Status, &entry.SourceSessionID, &created, &updated, &lastUsed, &expires, &entry.SupersedesID, &entry.EmbeddingModel, &entry.EmbeddingDim, &entry.ProvenanceType, &entry.SourceAuthority, &entry.ApprovalState, &entry.Sensitivity, &entry.ClaimKey, &entry.ClaimSlot, &entry.ClaimValue, &entry.EvidenceCount); err != nil {
		return MemoryEntry{}, fmt.Errorf("failed to scan memory entry: %w", err)
	}
	entry.CreatedAt = parseTime(created)
	entry.UpdatedAt = parseTime(updated)
	if lastUsed.Valid {
		entry.LastUsedAt = parseTime(lastUsed.String)
	}
	if expires.Valid {
		entry.ExpiresAt = parseTime(expires.String)
	}
	return entry, nil
}

func scanSessionTurn(rows interface{ Scan(...any) error }) (SessionTurn, error) {
	var turn SessionTurn
	var toolNames, topicTags, created string
	var expires sql.NullString
	if err := rows.Scan(&turn.ID, &turn.SessionID, &turn.UserID, &turn.Generation, &turn.UserText, &turn.AssistantText, &toolNames, &turn.Importance, &topicTags, &created, &expires); err != nil {
		return SessionTurn{}, fmt.Errorf("failed to scan session turn: %w", err)
	}
	turn.ToolNames = splitCSV(toolNames)
	turn.TopicTags = splitCSV(topicTags)
	turn.CreatedAt = parseTime(created)
	if expires.Valid {
		turn.ExpiresAt = parseTime(expires.String)
	}
	return turn, nil
}

func scanMemoryEntryWithDistance(rows interface{ Scan(...any) error }) (MemoryEntry, float64, error) {
	var entry MemoryEntry
	var created, updated string
	var lastUsed, expires sql.NullString
	var distance float64
	if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Scope, &entry.Category, &entry.Statement, &entry.Evidence, &entry.Confidence, &entry.Importance, &entry.Status, &entry.SourceSessionID, &created, &updated, &lastUsed, &expires, &entry.SupersedesID, &entry.EmbeddingModel, &entry.EmbeddingDim, &entry.ProvenanceType, &entry.SourceAuthority, &entry.ApprovalState, &entry.Sensitivity, &entry.ClaimKey, &entry.ClaimSlot, &entry.ClaimValue, &entry.EvidenceCount, &distance); err != nil {
		return MemoryEntry{}, 0, fmt.Errorf("failed to scan memory vector result: %w", err)
	}
	entry.CreatedAt = parseTime(created)
	entry.UpdatedAt = parseTime(updated)
	if lastUsed.Valid {
		entry.LastUsedAt = parseTime(lastUsed.String)
	}
	if expires.Valid {
		entry.ExpiresAt = parseTime(expires.String)
	}
	return entry, distance, nil
}

func (s *Store) vectorTableExists(name string) bool {
	_, ok := s.vectorTableDimension(name)
	return ok
}

func (s *Store) vectorTableDimension(name string) (int, bool) {
	var sqlText string
	err := s.sql.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&sqlText)
	if err != nil {
		return 0, false
	}
	return vectorDimensionFromSQL(sqlText)
}

func vectorDimensionFromSQL(sqlText string) (int, bool) {
	if !strings.Contains(sqlText, "float[") {
		return 0, false
	}
	start := strings.Index(sqlText, "float[") + len("float[")
	end := strings.Index(sqlText[start:], "]")
	if end < 0 {
		return 0, false
	}
	var dim int
	if _, err := fmt.Sscanf(sqlText[start:start+end], "%d", &dim); err != nil || dim <= 0 {
		return 0, false
	}
	return dim, true
}

func serializeVector(values []float64) ([]byte, error) {
	vector := make([]float32, 0, len(values))
	for _, value := range values {
		vector = append(vector, float32(value))
	}
	serialized, err := sqlite_vec.SerializeFloat32(vector)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize embedding vector: %w", err)
	}
	return serialized, nil
}

func distanceToSimilarity(distance float64) float64 {
	if distance < 0 {
		distance = 0
	}
	return 1 / (1 + distance)
}

func (s *Store) expireOldMemories() error {
	_, err := s.CleanupExpiredSessions(context.Background(), time.Now().UTC())
	return err
}

func (s *Store) recordEvent(userID string, memoryID int64, eventType, requestID, sessionID, metadata string) {
	_, _ = s.sql.Exec(`INSERT INTO memory_events (canonical_user_id, memory_id, event_type, request_id, session_id, created_at, metadata) VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, nullableID(memoryID), eventType, requestID, sessionID, formatTime(time.Now()), metadata)
}

func (s *Store) ensureAccountUser(userID string) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("user memory: user id is required")
	}
	var exists int
	err := s.sql.QueryRow(`SELECT 1 FROM account_users WHERE canonical_user_id = ?`, userID).Scan(&exists)
	if err == sql.ErrNoRows {
		return fmt.Errorf("user memory: account user %q does not exist", userID)
	}
	if err != nil {
		return fmt.Errorf("failed to check account user %q: %w", userID, err)
	}
	return nil
}

func (s *Store) embedBestEffort(ctx context.Context, text string) []float64 {
	vector, _ := s.embed(ctx, text)
	return vector
}

func (s *Store) embed(ctx context.Context, text string) ([]float64, error) {
	return s.embedWithModel(ctx, s.embedModel, text)
}

func (s *Store) embedWithModel(ctx context.Context, model, text string) ([]float64, error) {
	if s == nil || s.embedder == nil || strings.TrimSpace(model) == "" || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	resp, err := s.embedder.Embed(ctx, llm.EmbedRequest{Model: strings.TrimSpace(model), Input: strings.TrimSpace(text)})
	if err != nil {
		return nil, fmt.Errorf("embed durable memory query: %w", err)
	}
	if resp == nil || len(resp.Embeddings) == 0 || len(resp.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embed durable memory query: provider returned no vector")
	}
	return append([]float64(nil), resp.Embeddings[0]...), nil
}

func memoryEmbeddingText(scope, category, statement, evidence string) string {
	return strings.TrimSpace(scope + "\n" + category + "\n" + statement + "\nEvidence: " + evidence)
}

func normalizeScope(scope string) string {
	scope = strings.TrimSpace(strings.ToLower(scope))
	scope = strings.ReplaceAll(scope, "-", "_")
	scope = strings.ReplaceAll(scope, " ", "_")
	if scope == ScopeLongTerm || scope == "long" || scope == "persistent" {
		return ScopeLongTerm
	}
	return ScopeShortTerm
}

func normalizeOptionalScope(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return ""
	}
	return normalizeScope(scope)
}

func normalizeCategory(cat string) string {
	cat = strings.TrimSpace(strings.ToLower(cat))
	cat = strings.ReplaceAll(cat, "-", "_")
	cat = strings.ReplaceAll(cat, " ", "_")
	if cat == "preferences" {
		cat = "durable_preferences"
	}
	if cat == "system_rules" {
		cat = "communication_preferences"
	}
	for _, valid := range ValidCategories {
		if cat == valid {
			return cat
		}
	}
	return "notes"
}

func normalizeOptionalCategory(cat string) string {
	if strings.TrimSpace(cat) == "" {
		return ""
	}
	return normalizeCategory(cat)
}

func statementKey(statement string) string {
	return strings.ToLower(strings.Join(strings.Fields(statement), " "))
}

func clampInt(value, minValue, maxValue, fallback int) int {
	if value == 0 {
		return fallback
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	return t
}

func recencyScore(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	age := time.Since(t)
	if age <= 0 {
		return 1
	}
	return 1 / (1 + age.Hours()/168)
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	lines := strings.Split(value, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(strings.TrimPrefix(line, "-")); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// RenderMemory formats entries as compact Markdown for tools and stream payloads.
func RenderMemory(intro string, entries []MemoryEntry) string {
	var b strings.Builder
	if strings.TrimSpace(intro) != "" {
		b.WriteString(strings.TrimSpace(intro))
		b.WriteString("\n\n")
	}
	if len(entries) == 0 {
		return strings.TrimSpace(b.String())
	}
	b.WriteString("# User Memory\n")
	byHeading := map[string][]MemoryEntry{}
	for _, entry := range entries {
		heading := displayCategoryName(entry.Scope + " " + entry.Category)
		byHeading[heading] = append(byHeading[heading], entry)
	}
	headings := make([]string, 0, len(byHeading))
	for heading := range byHeading {
		headings = append(headings, heading)
	}
	sort.Strings(headings)
	for _, heading := range headings {
		b.WriteString("\n## ")
		b.WriteString(heading)
		b.WriteString("\n\n")
		for _, entry := range byHeading[heading] {
			b.WriteString("- Statement: ")
			b.WriteString(quoteProfileText(normalizeProfileText(entry.Statement)))
			b.WriteString("\n\n- Memory ID: ")
			b.WriteString(strconv.FormatInt(entry.ID, 10))
			b.WriteString("\n\n- Evidence: ")
			b.WriteString(quoteProfileText(normalizeProfileText(entry.Evidence)))
			b.WriteString("\n\n- Confidence: ")
			b.WriteString(strconv.FormatFloat(clampRecallScore(entry.Confidence), 'f', 4, 64))
			b.WriteString("\n\n- Formation provenance: ")
			b.WriteString(quoteProfileText(normalizeProfileToken(entry.ProvenanceType)))
			b.WriteString("\n\n- Source authority: ")
			b.WriteString(quoteProfileText(normalizeProfileToken(entry.SourceAuthority)))
			b.WriteString("\n\n- Epistemic status: ")
			b.WriteString(quoteProfileText(recallEpistemicStatus(recallAuthorityForEntry(entry))))
			b.WriteString("\n\n- Sensitivity: ")
			b.WriteString(quoteProfileText(normalizeProfileToken(entry.Sensitivity)))
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func displayCategoryName(value string) string {
	value = strings.ReplaceAll(value, "_", " ")
	parts := strings.Fields(value)
	for i, part := range parts {
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

// ParsedContent is a UI-friendly representation of rendered memory content.
type ParsedContent struct {
	Intro    string            `json:"intro,omitempty"`
	Sections map[string]string `json:"sections,omitempty"`
}

// ParseContent converts rendered memory Markdown into sections for streaming UIs.
func ParseContent(content string) ParsedContent {
	parsed := ParsedContent{Sections: map[string]string{}}
	content = strings.TrimSpace(content)
	if content == "" {
		return parsed
	}
	lines := strings.Split(content, "\n")
	var current string
	var b strings.Builder
	flush := func() {
		if current != "" {
			parsed.Sections[current] = strings.TrimSpace(b.String())
			b.Reset()
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "# User Memory") {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			flush()
			current = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		if current == "" && strings.HasPrefix(strings.TrimSpace(line), "You are speaking with ") {
			parsed.Intro = strings.TrimSpace(line)
			continue
		}
		if current != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	flush()
	if len(parsed.Sections) == 0 {
		parsed.Sections = nil
	}
	return parsed
}
