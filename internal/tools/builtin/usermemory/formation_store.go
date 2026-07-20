package usermemory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
)

const FormationExtractorVersion = "formation-v2"

// FormationSource identifies the canonical turn and request that formed memory.
type FormationSource struct {
	RequestID         string
	SessionID         string
	SessionGeneration int
	TurnID            int64
	Model             string
	ExtractorVersion  string
	ToolName          string
}

// CandidateProposal is one validated policy result ready for canonical staging.
type CandidateProposal struct {
	Output              memoryformation.CandidateOutput
	Source              FormationSource
	IdempotencyKey      string
	SupersedesStatement string
	FormationJob        *FormationJob
	CompactionJob       *SessionCompactionJob
}

// FormationCandidate is a persisted memory proposal.
type FormationCandidate struct {
	ID                      int64
	UserID                  string
	State                   string
	Scope                   string
	Category                string
	Statement               string
	Evidence                string
	Confidence              float64
	Importance              int
	Provenance              string
	SourceAuthority         string
	Sensitivity             string
	FormationMode           string
	Context                 string
	PolicyDecision          string
	DecisionReason          string
	SourceRequestID         string
	SourceSessionID         string
	SourceGeneration        int
	SourceTurnID            int64
	ExtractionModel         string
	ExtractorVersion        string
	SupersedesMemoryID      int64
	SupersedesStatement     string
	PublishedMemoryID       int64
	ConfirmationSessionID   string
	ConfirmationRequestID   string
	ConfirmationPresentedAt time.Time
	FormationEligibleAt     time.Time
	ExpiresAt               time.Time
	ClaimKey                string
	ClaimSlot               string
	ClaimValue              string
}

// FormationJob is one leased post-turn extraction operation.
type FormationJob struct {
	ID                int64
	UserID            string
	RequestID         string
	SessionID         string
	SessionGeneration int
	TurnID            int64
	Model             string
	ExtractorVersion  string
	AttemptCount      int
	LeaseUntil        time.Time
}

// StoredSessionTurn identifies an exchange that was actually persisted.
type StoredSessionTurn struct {
	ID         int64
	UserID     string
	SessionID  string
	Generation int
	UserText   string
}

// ProposeCandidate persists a validated decision once per tenant/idempotency key.
func (s *Store) ProposeCandidate(ctx context.Context, userID string, proposal CandidateProposal) (FormationCandidate, bool, error) {
	if job := proposal.CompactionJob; job != nil {
		if userID != job.UserID || proposal.Source.SessionID != job.SessionID || proposal.Source.SessionGeneration != job.SessionGeneration || proposal.Source.TurnID < job.CoveredFromTurnID || proposal.Source.TurnID > job.CoveredThroughTurnID {
			return FormationCandidate{}, false, fmt.Errorf("pre-compaction candidate scope does not match fenced job")
		}
	}
	if err := s.ensureAccountUser(userID); err != nil {
		return FormationCandidate{}, false, err
	}
	key := strings.TrimSpace(proposal.IdempotencyKey)
	if key == "" {
		key = formationKey(proposal.Source.RequestID, proposal.Source.TurnID, proposal.Output.Statement, string(proposal.Output.Mode), proposal.Source.ExtractorVersion)
	}
	state := "proposed"
	decisionReason := proposal.Output.Reason
	switch proposal.Output.Decision {
	case memoryformation.DecisionAutomatic, memoryformation.DecisionInferredActive, memoryformation.DecisionShortTerm:
		state = "approved"
	case memoryformation.DecisionDisallowed:
		state = "rejected"
	}
	now := time.Now().UTC()
	var expires any
	if proposal.Output.TTL > 0 {
		expires = formatTime(now.Add(proposal.Output.TTL))
	}
	var formationEligible any
	if proposal.Source.TurnID > 0 {
		var eligible sql.NullString
		err := s.sql.QueryRowContext(ctx, `SELECT formation_eligible_at FROM session_turns WHERE id = ? AND canonical_user_id = ?`, proposal.Source.TurnID, userID).Scan(&eligible)
		if err != nil && err != sql.ErrNoRows {
			return FormationCandidate{}, false, fmt.Errorf("read candidate delivery eligibility: %w", err)
		}
		if eligible.Valid {
			formationEligible = eligible.String
		}
	}
	var supersedesID int64
	if target := strings.TrimSpace(proposal.SupersedesStatement); target != "" {
		var id int64
		var existingConfidence float64
		var existingAuthority string
		err := s.sql.QueryRowContext(ctx, `SELECT id, confidence, source_authority FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND statement_key = ? AND status = 'active'`, userID, proposal.Output.Scope, statementKey(target)).Scan(&id, &existingConfidence, &existingAuthority)
		if err != nil && err != sql.ErrNoRows {
			return FormationCandidate{}, false, fmt.Errorf("resolve candidate supersession: %w", err)
		}
		if err == nil {
			if proposal.Output.Mode == memoryformation.ModeExplicitRemember || candidateEvidenceStronger(string(proposal.Output.SourceAuthority), proposal.Output.Confidence, existingAuthority, existingConfidence) {
				supersedesID = id
			} else {
				state = "proposed"
				decisionReason = "replacement evidence is weaker than the active memory"
			}
		}
	}
	if supersedesID == 0 && state == "approved" && proposal.Output.ClaimSlot != "" && !strings.HasSuffix(proposal.Output.ClaimSlot, ".fact") {
		var id int64
		var existingConfidence float64
		var existingAuthority string
		err := s.sql.QueryRowContext(ctx, `SELECT id, confidence, source_authority FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND claim_slot = ? AND claim_key != ? AND status = 'active' ORDER BY CASE source_authority WHEN 'user_direct' THEN 3 WHEN 'verified' THEN 4 WHEN 'model' THEN 2 ELSE 1 END DESC, confidence DESC, updated_at DESC, id DESC LIMIT 1`, userID, proposal.Output.Scope, proposal.Output.ClaimSlot, proposal.Output.ClaimKey).Scan(&id, &existingConfidence, &existingAuthority)
		if err != nil {
			if err != sql.ErrNoRows {
				return FormationCandidate{}, false, fmt.Errorf("resolve conflicting memory claim: %w", err)
			}
		}
		if id > 0 {
			if candidateEvidenceStronger(string(proposal.Output.SourceAuthority), proposal.Output.Confidence, existingAuthority, existingConfidence) {
				supersedesID = id
				decisionReason = "stronger evidence supersedes a conflicting claim"
			} else {
				state = "proposed"
				decisionReason = "conflicting evidence is weaker than the active claim"
			}
		}
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return FormationCandidate{}, false, fmt.Errorf("begin memory candidate proposal: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	if job := proposal.FormationJob; job != nil {
		if userID != job.UserID || proposal.Source.RequestID != job.RequestID || proposal.Source.SessionID != job.SessionID || proposal.Source.SessionGeneration != job.SessionGeneration || proposal.Source.TurnID != job.TurnID || job.LeaseUntil.IsZero() {
			return FormationCandidate{}, false, fmt.Errorf("formation candidate scope does not match fenced job")
		}
		fenced, err := tx.ExecContext(ctx, `
UPDATE memory_formation_jobs SET updated_at = updated_at
WHERE id = ? AND canonical_user_id = ? AND state = 'running'
	AND lease_until = ? AND julianday(lease_until) > julianday(?)
	AND EXISTS (
		SELECT 1 FROM account_users active
		WHERE active.canonical_user_id = memory_formation_jobs.canonical_user_id
			AND active.lifecycle_state = 'active'
	)`, job.ID, job.UserID, formatTime(job.LeaseUntil), formatTime(now))
		if err != nil {
			return FormationCandidate{}, false, fmt.Errorf("fence formation candidate: %w", err)
		}
		if count, _ := fenced.RowsAffected(); count != 1 {
			return FormationCandidate{}, false, fmt.Errorf("fence formation candidate: stale or cancelled job lease")
		}
	}
	if job := proposal.CompactionJob; job != nil {
		fenced, err := tx.ExecContext(ctx, `
UPDATE session_compaction_jobs SET updated_at = updated_at
WHERE id = ? AND canonical_user_id = ? AND session_id = ? AND session_generation = ?
	AND covered_from_turn_id = ? AND covered_through_turn_id = ?
	AND state = 'running' AND lease_owner = ? AND julianday(lease_until) > julianday(?)
	AND EXISTS (
		SELECT 1 FROM tenant_sessions active
		WHERE active.canonical_user_id = session_compaction_jobs.canonical_user_id
			AND active.session_id = session_compaction_jobs.session_id
			AND active.generation = session_compaction_jobs.session_generation
			AND julianday(active.expires_at) > julianday(?)
	)
	AND ? BETWEEN covered_from_turn_id AND covered_through_turn_id
	AND EXISTS (
		SELECT 1 FROM session_turns source
		WHERE source.id = ? AND source.canonical_user_id = session_compaction_jobs.canonical_user_id
			AND source.session_id = session_compaction_jobs.session_id
			AND source.session_generation = session_compaction_jobs.session_generation
			AND source.delivered_at IS NOT NULL
	)`,
			job.ID, job.UserID, job.SessionID, job.SessionGeneration,
			job.CoveredFromTurnID, job.CoveredThroughTurnID, job.LeaseOwner,
			formatTime(now), formatTime(now), proposal.Source.TurnID, proposal.Source.TurnID)
		if err != nil {
			return FormationCandidate{}, false, fmt.Errorf("fence pre-compaction candidate: %w", err)
		}
		if count, _ := fenced.RowsAffected(); count != 1 {
			return FormationCandidate{}, false, fmt.Errorf("fence pre-compaction candidate: stale job lease or generation")
		}
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO memory_candidates (
	canonical_user_id, idempotency_key, state, scope, category, statement, statement_key,
	evidence_summary, confidence, importance, provenance_type, source_authority,
	source_request_id, source_session_id, source_session_generation, source_turn_id,
	extraction_model, extractor_version, explicit_tool_source, formation_mode, sensitivity,
	content_context, policy_decision, supersedes_memory_id, created_at, updated_at, expires_at, formation_eligible_at,
	decided_at, decided_by, decision_reason, claim_key, claim_slot, claim_value
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_user_id, idempotency_key) DO NOTHING
`, userID, key, state, proposal.Output.Scope, proposal.Output.Category, proposal.Output.Statement,
		statementKey(proposal.Output.Statement), proposal.Output.Evidence, proposal.Output.Confidence,
		proposal.Output.Importance, proposal.Output.Provenance, proposal.Output.SourceAuthority,
		proposal.Source.RequestID, proposal.Source.SessionID, proposal.Source.SessionGeneration,
		nullableID(proposal.Source.TurnID), proposal.Source.Model, firstNonEmptyFormation(proposal.Source.ExtractorVersion, FormationExtractorVersion),
		proposal.Source.ToolName, proposal.Output.Mode, proposal.Output.Sensitivity, proposal.Output.Context,
		proposal.Output.Decision, nullableID(supersedesID), formatTime(now), formatTime(now), expires, formationEligible,
		decisionTime(state, now), decisionActor(state, proposal.Output.Mode), decisionReason,
		proposal.Output.ClaimKey, proposal.Output.ClaimSlot, proposal.Output.ClaimValue)
	if err != nil {
		return FormationCandidate{}, false, fmt.Errorf("insert memory candidate: %w", err)
	}
	created, err := result.RowsAffected()
	if err != nil {
		return FormationCandidate{}, false, fmt.Errorf("check memory candidate insert: %w", err)
	}
	candidate, err := loadCandidateByKeyTx(ctx, tx, userID, key)
	if err != nil {
		return FormationCandidate{}, false, err
	}
	if created == 0 && (candidate.Statement != proposal.Output.Statement || candidate.Evidence != proposal.Output.Evidence || candidate.Scope != string(proposal.Output.Scope) || candidate.Category != string(proposal.Output.Category) || candidate.Provenance != string(proposal.Output.Provenance) || candidate.SourceAuthority != string(proposal.Output.SourceAuthority) || candidate.Sensitivity != string(proposal.Output.Sensitivity) || candidate.FormationMode != string(proposal.Output.Mode) || candidate.Context != string(proposal.Output.Context) || candidate.PolicyDecision != string(proposal.Output.Decision) || candidate.Confidence != proposal.Output.Confidence || candidate.Importance != proposal.Output.Importance || candidate.SourceRequestID != proposal.Source.RequestID || candidate.SourceSessionID != proposal.Source.SessionID || candidate.SourceGeneration != proposal.Source.SessionGeneration || candidate.SourceTurnID != proposal.Source.TurnID || candidate.ExtractionModel != proposal.Source.Model || candidate.ExtractorVersion != firstNonEmptyFormation(proposal.Source.ExtractorVersion, FormationExtractorVersion) || candidate.SupersedesMemoryID != supersedesID || candidate.ClaimKey != proposal.Output.ClaimKey || candidate.ClaimSlot != proposal.Output.ClaimSlot || candidate.ClaimValue != proposal.Output.ClaimValue) {
		return FormationCandidate{}, false, fmt.Errorf("memory candidate idempotency payload mismatch")
	}
	if created == 1 {
		evidenceKey := key + ":candidate-evidence"
		if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_evidence (canonical_user_id, candidate_id, idempotency_key, evidence_type, content, source_authority, source_request_id, source_session_id, source_turn_id, created_at, provenance_type, relation_type, confidence_contribution, extraction_model, extractor_version, source_session_generation, correlation_key)
VALUES (?, ?, ?, 'exact_user_quote', ?, ?, ?, ?, ?, ?, ?, 'supports', ?, ?, ?, ?, ?)
`, userID, candidate.ID, evidenceKey, proposal.Output.Evidence, proposal.Output.SourceAuthority,
			proposal.Source.RequestID, proposal.Source.SessionID, nullableID(proposal.Source.TurnID), formatTime(now),
			proposal.Output.Provenance, proposal.Output.Confidence, proposal.Source.Model,
			firstNonEmptyFormation(proposal.Source.ExtractorVersion, FormationExtractorVersion), proposal.Source.SessionGeneration, evidenceKey); err != nil {
			return FormationCandidate{}, false, fmt.Errorf("insert candidate evidence: %w", err)
		}
		if err := insertFormationAuditTx(ctx, tx, userID, key+":proposed", "candidate."+state, candidate.ID, 0, 0, proposal.Source, "policy", decisionReason); err != nil {
			return FormationCandidate{}, false, err
		}
		if candidate.SupersedesMemoryID > 0 {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_relations (canonical_user_id, idempotency_key, relation_type, source_candidate_id, target_memory_id, created_at, metadata)
VALUES (?, ?, 'contradicts', ?, ?, ?, '{"resolution":"automatic_supersession"}')
ON CONFLICT(canonical_user_id, idempotency_key) DO NOTHING
`, userID, key+":contradicts", candidate.ID, candidate.SupersedesMemoryID, formatTime(now)); err != nil {
				return FormationCandidate{}, false, fmt.Errorf("record candidate contradiction: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return FormationCandidate{}, false, fmt.Errorf("commit memory candidate proposal: %w", err)
	}
	return candidate, created == 1, nil
}

// PublishCandidate atomically publishes one approved candidate or attaches its
// evidence to an existing exact duplicate.
func (s *Store) PublishCandidate(ctx context.Context, userID string, candidateID int64) (MemoryEntry, error) {
	unlock := s.lockUsers(userID)
	defer unlock()
	candidate, err := s.LoadCandidate(ctx, userID, candidateID)
	if err != nil {
		return MemoryEntry{}, err
	}
	if candidate.PublishedMemoryID > 0 {
		return s.EntryByID(candidate.PublishedMemoryID)
	}
	if candidate.State != "approved" {
		return MemoryEntry{}, fmt.Errorf("memory candidate %d is not approved", candidateID)
	}
	if !candidate.ExpiresAt.IsZero() && !candidate.ExpiresAt.After(time.Now().UTC()) {
		_, _ = s.sql.ExecContext(ctx, `UPDATE memory_candidates SET state = 'rejected', decision_reason = 'candidate_expired_before_publication', decided_at = ?, decided_by = 'retention', updated_at = ? WHERE id = ? AND canonical_user_id = ? AND state = 'approved' AND published_memory_id IS NULL`, formatTime(time.Now().UTC()), formatTime(time.Now().UTC()), candidateID, userID)
		return MemoryEntry{}, fmt.Errorf("memory candidate %d expired before publication", candidateID)
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("begin memory publication: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	candidate, err = loadCandidateTx(ctx, tx, userID, candidateID)
	if err != nil {
		return MemoryEntry{}, err
	}
	if candidate.PublishedMemoryID > 0 {
		if err := tx.Commit(); err != nil {
			return MemoryEntry{}, err
		}
		return s.EntryByID(candidate.PublishedMemoryID)
	}
	if candidate.State != "approved" {
		return MemoryEntry{}, fmt.Errorf("memory candidate %d changed state", candidateID)
	}
	if !candidate.ExpiresAt.IsZero() && !candidate.ExpiresAt.After(time.Now().UTC()) {
		return MemoryEntry{}, fmt.Errorf("memory candidate %d expired before transactional publication", candidateID)
	}
	if err := s.formationStage("validated"); err != nil {
		return MemoryEntry{}, err
	}
	now := time.Now().UTC()
	var duplicateID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND status = 'active' AND ((? != '' AND claim_key = ?) OR statement_key = ?) ORDER BY CASE WHEN claim_key = ? THEN 0 ELSE 1 END, id LIMIT 1`, userID, candidate.Scope, candidate.ClaimKey, candidate.ClaimKey, statementKey(candidate.Statement), candidate.ClaimKey).Scan(&duplicateID)
	if err != nil && err != sql.ErrNoRows {
		return MemoryEntry{}, fmt.Errorf("read duplicate active memory: %w", err)
	}
	if err == nil {
		if candidate.SupersedesMemoryID > 0 && candidate.SupersedesMemoryID != duplicateID {
			if err := s.supersedeActiveMemoryTx(ctx, tx, userID, candidate.SupersedesMemoryID, duplicateID, now); err != nil {
				return MemoryEntry{}, err
			}
			if _, err := refreshProfileTx(ctx, tx, userID, now); err != nil {
				return MemoryEntry{}, fmt.Errorf("advance profile after duplicate correction: %w", err)
			}
		}
		if err := attachPublishedEvidenceTx(ctx, tx, candidate, duplicateID); err != nil {
			return MemoryEntry{}, err
		}
		var oldConfidence float64
		var oldAuthority, oldProvenance, oldSensitivity string
		var evidenceCount int
		if err := tx.QueryRowContext(ctx, `SELECT confidence, source_authority, provenance_type, sensitivity, evidence_count FROM memory_entries WHERE id = ? AND canonical_user_id = ?`, duplicateID, userID).Scan(&oldConfidence, &oldAuthority, &oldProvenance, &oldSensitivity, &evidenceCount); err != nil {
			return MemoryEntry{}, fmt.Errorf("read memory confidence for reinforcement: %w", err)
		}
		contribution := candidate.Confidence
		if candidate.SourceAuthority == string(memoryformation.AuthorityModel) && candidate.SourceSessionID != "" {
			var correlated int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_evidence WHERE canonical_user_id = ? AND memory_id = ? AND source_session_id = ? AND source_authority = ? AND idempotency_key != ?`, userID, duplicateID, candidate.SourceSessionID, string(memoryformation.AuthorityModel), formationKey("memory-evidence", candidate.ID, duplicateID)).Scan(&correlated); err != nil {
				return MemoryEntry{}, fmt.Errorf("inspect correlated memory evidence: %w", err)
			}
			if correlated > 0 {
				contribution *= 0.25
			}
		}
		confidence := aggregateConfidence(oldConfidence, contribution)
		authority, provenance := strongestMemorySource(oldAuthority, oldProvenance, candidate.SourceAuthority, candidate.Provenance)
		statement, evidence := "", ""
		if sourceAuthorityRank(candidate.SourceAuthority) > sourceAuthorityRank(oldAuthority) {
			statement, evidence = candidate.Statement, candidate.Evidence
		}
		profileApproved := 1
		if authority == string(memoryformation.AuthorityModel) || provenance == string(memoryformation.ProvenanceModelInference) {
			profileApproved = 0
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET confidence = ?, importance = MAX(importance, ?), evidence_count = ?, source_authority = ?, provenance_type = ?, sensitivity = ?, profile_approved = ?, statement = CASE WHEN ? = '' THEN statement ELSE ? END, statement_key = CASE WHEN ? = '' THEN statement_key ELSE ? END, evidence = CASE WHEN ? = '' THEN evidence ELSE ? END, category = CASE WHEN ? = '' THEN category ELSE ? END, source_session_id = CASE WHEN ? = '' THEN source_session_id ELSE ? END, source_request_id = CASE WHEN ? = '' THEN source_request_id ELSE ? END, source_turn_id = CASE WHEN ? = 0 THEN source_turn_id ELSE ? END, formation_mode = CASE WHEN ? = '' THEN formation_mode ELSE ? END, claim_key = ?, claim_slot = ?, claim_value = ?, updated_at = ? WHERE id = ? AND canonical_user_id = ?`,
			confidence, candidate.Importance, evidenceCount+1, authority, provenance, strongestSensitivity(oldSensitivity, candidate.Sensitivity), profileApproved,
			statement, statement, statement, statementKey(statement), evidence, evidence, statement, candidate.Category,
			statement, candidate.SourceSessionID, statement, candidate.SourceRequestID,
			conditionalTurnID(statement, candidate.SourceTurnID), nullableID(candidate.SourceTurnID), statement, candidate.FormationMode,
			candidate.ClaimKey, candidate.ClaimSlot, candidate.ClaimValue, formatTime(now), duplicateID, userID); err != nil {
			return MemoryEntry{}, fmt.Errorf("reinforce active memory: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_relations (canonical_user_id, idempotency_key, relation_type, source_candidate_id, target_memory_id, created_at, metadata) VALUES (?, ?, 'duplicate', ?, ?, ?, '{"resolution":"reinforced"}') ON CONFLICT(canonical_user_id, idempotency_key) DO NOTHING`, userID, formationKey("reinforces", candidate.ID, duplicateID), candidate.ID, duplicateID, formatTime(now)); err != nil {
			return MemoryEntry{}, fmt.Errorf("record memory reinforcement: %w", err)
		}
		if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", duplicateID, "upsert", "reinforce:"+formatTime(now)); err != nil {
			return MemoryEntry{}, err
		}
		if _, err := refreshProfileTx(ctx, tx, userID, now); err != nil {
			return MemoryEntry{}, fmt.Errorf("advance profile after memory reinforcement: %w", err)
		}
		if err := markCandidatePublishedTx(ctx, tx, candidate, duplicateID); err != nil {
			return MemoryEntry{}, err
		}
		if err := insertFormationAuditTx(ctx, tx, userID, formationKey("duplicate", candidate.ID, duplicateID), "memory.duplicate_observed", candidate.ID, duplicateID, 0, FormationSource{RequestID: candidate.SourceRequestID, SessionID: candidate.SourceSessionID, TurnID: candidate.SourceTurnID}, "formation", "exact duplicate"); err != nil {
			return MemoryEntry{}, err
		}
		if err := tx.Commit(); err != nil {
			return MemoryEntry{}, fmt.Errorf("commit duplicate memory evidence: %w", err)
		}
		s.signalDerivedIndex()
		return s.EntryByID(duplicateID)
	}
	var memoryID int64
	var inactiveID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND status IN ('expired', 'superseded') AND ((? != '' AND claim_key = ?) OR statement_key = ?) ORDER BY CASE WHEN claim_key = ? THEN 0 ELSE 1 END, updated_at DESC, id DESC LIMIT 1`, userID, candidate.Scope, candidate.ClaimKey, candidate.ClaimKey, statementKey(candidate.Statement), candidate.ClaimKey).Scan(&inactiveID)
	if err != nil && err != sql.ErrNoRows {
		return MemoryEntry{}, fmt.Errorf("read inactive duplicate memory: %w", err)
	}
	if err == nil {
		if candidate.SupersedesMemoryID == inactiveID {
			candidate.SupersedesMemoryID = 0
		}
		err = tx.QueryRowContext(ctx, `
UPDATE memory_entries SET category = ?, statement = ?, statement_key = ?, evidence = ?, confidence = ?, importance = ?,
	status = 'active', source_session_id = ?, updated_at = ?, expires_at = ?, supersedes_id = ?,
		embedding_model = ?, embedding_dim = ?, profile_approved = ?, candidate_id = ?, provenance_type = ?,
	source_authority = ?, source_request_id = ?, source_turn_id = ?, formation_mode = ?, sensitivity = ?,
	approval_state = 'approved', approved_at = ?, approved_by = ?, valid_from = ?, valid_until = ?,
	invalidated_at = NULL, invalidation_reason = '', erased_at = NULL, erasure_reason = '',
		forgotten_at = NULL, hard_delete_after = NULL, lifecycle_request_id = '', claim_key = ?, claim_slot = ?, claim_value = ?, evidence_count = 1
WHERE id = ? AND canonical_user_id = ?
RETURNING id
`, candidate.Category, candidate.Statement, statementKey(candidate.Statement), candidate.Evidence,
			candidate.Confidence, candidate.Importance, candidate.SourceSessionID, formatTime(now), nullableFormationTime(candidate.ExpiresAt), nullableID(candidate.SupersedesMemoryID),
			"", 0, profileApprovedForCandidate(candidate), candidate.ID, candidate.Provenance, candidate.SourceAuthority,
			candidate.SourceRequestID, nullableID(candidate.SourceTurnID), candidate.FormationMode, candidate.Sensitivity,
			formatTime(now), candidateDecisionActor(candidate), formatTime(now), nullableFormationTime(candidate.ExpiresAt), candidate.ClaimKey, candidate.ClaimSlot, candidate.ClaimValue, inactiveID, userID).Scan(&memoryID)
	} else {
		err = tx.QueryRowContext(ctx, `
INSERT INTO memory_entries (
	canonical_user_id, scope, category, statement, statement_key, evidence, confidence,
	importance, status, source_session_id, created_at, updated_at, expires_at,
	embedding_model, embedding_dim, profile_approved, candidate_id, provenance_type,
		source_authority, source_request_id, source_turn_id, formation_mode, sensitivity,
		approval_state, approved_at, approved_by, valid_from, valid_until, claim_key, claim_slot, claim_value, evidence_count
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'approved', ?, ?, ?, ?, ?, ?, ?, 1)
RETURNING id
`, userID, candidate.Scope, candidate.Category, candidate.Statement, statementKey(candidate.Statement),
			candidate.Evidence, candidate.Confidence, candidate.Importance, candidate.SourceSessionID,
			formatTime(now), formatTime(now), nullableFormationTime(candidate.ExpiresAt), "", 0, profileApprovedForCandidate(candidate),
			candidate.ID, candidate.Provenance, candidate.SourceAuthority, candidate.SourceRequestID,
			nullableID(candidate.SourceTurnID), candidate.FormationMode, candidate.Sensitivity,
			formatTime(now), candidateDecisionActor(candidate), formatTime(now), nullableFormationTime(candidate.ExpiresAt), candidate.ClaimKey, candidate.ClaimSlot, candidate.ClaimValue).Scan(&memoryID)
	}
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("insert active memory: %w", err)
	}
	if err := s.formationStage("canonical_written"); err != nil {
		return MemoryEntry{}, err
	}
	if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", memoryID, "upsert", "publish:"+formatTime(now)); err != nil {
		return MemoryEntry{}, err
	}
	if err := s.formationStage("vector_written"); err != nil {
		return MemoryEntry{}, err
	}
	if candidate.SupersedesMemoryID > 0 {
		if err := s.supersedeActiveMemoryTx(ctx, tx, userID, candidate.SupersedesMemoryID, memoryID, now); err != nil {
			return MemoryEntry{}, err
		}
	}
	if err := s.formationStage("supersession_written"); err != nil {
		return MemoryEntry{}, err
	}
	if err := attachPublishedEvidenceTx(ctx, tx, candidate, memoryID); err != nil {
		return MemoryEntry{}, err
	}
	if err := insertFormationAuditTx(ctx, tx, userID, formationKey("published", candidate.ID, memoryID), "memory.published", candidate.ID, memoryID, 0, FormationSource{RequestID: candidate.SourceRequestID, SessionID: candidate.SourceSessionID, TurnID: candidate.SourceTurnID}, "formation", candidate.PolicyDecision); err != nil {
		return MemoryEntry{}, err
	}
	if err := s.formationStage("audit_written"); err != nil {
		return MemoryEntry{}, err
	}
	if _, err := refreshProfileTx(ctx, tx, userID, now); err != nil {
		return MemoryEntry{}, fmt.Errorf("advance profile after memory publication: %w", err)
	}
	if err := s.formationStage("profile_written"); err != nil {
		return MemoryEntry{}, err
	}
	if err := markCandidatePublishedTx(ctx, tx, candidate, memoryID); err != nil {
		return MemoryEntry{}, err
	}
	if err := s.formationStage("candidate_published"); err != nil {
		return MemoryEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return MemoryEntry{}, fmt.Errorf("commit memory publication: %w", err)
	}
	s.signalDerivedIndex()
	return s.EntryByID(memoryID)
}

// LoadCandidate returns one tenant-owned candidate.
func (s *Store) LoadCandidate(ctx context.Context, userID string, candidateID int64) (FormationCandidate, error) {
	return loadCandidateSQL(ctx, s.sql, userID, candidateID)
}

// ApprovedUnpublishedCandidates returns durable approvals awaiting publication.
func (s *Store) ApprovedUnpublishedCandidates(ctx context.Context, limit int) ([]FormationCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.sql.QueryContext(ctx, candidateSelect+` WHERE state = 'approved' AND published_memory_id IS NULL AND updated_at <= ? AND formation_eligible_at IS NOT NULL ORDER BY updated_at, id LIMIT ?`, formatTime(time.Now().UTC().Add(-time.Minute)), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []FormationCandidate
	for rows.Next() {
		candidate, err := scanFormationCandidate(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// DeferCandidatePublication records a failed retry without storing error text.
func (s *Store) DeferCandidatePublication(ctx context.Context, userID string, candidateID int64) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE memory_candidates SET updated_at = ? WHERE id = ? AND canonical_user_id = ? AND state = 'approved' AND published_memory_id IS NULL`, formatTime(time.Now().UTC()), candidateID, userID)
	return err
}

// AttachRequestCandidates links explicit tool candidates to their persisted turn.
func (s *Store) AttachRequestCandidates(ctx context.Context, userID, requestID string, turnID int64) ([]int64, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // nolint:errcheck
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM session_turns WHERE id = ? AND canonical_user_id = ?`, turnID, userID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("validate candidate source turn: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET source_turn_id = ?, formation_eligible_at = (SELECT formation_eligible_at FROM session_turns WHERE id = ? AND canonical_user_id = ?), updated_at = ? WHERE canonical_user_id = ? AND source_request_id = ? AND source_turn_id IS NULL`, turnID, turnID, userID, formatTime(time.Now().UTC()), userID, requestID); err != nil {
		return nil, fmt.Errorf("attach candidate source turn: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence SET source_turn_id = ? WHERE canonical_user_id = ? AND source_request_id = ? AND source_turn_id IS NULL`, turnID, userID, requestID); err != nil {
		return nil, fmt.Errorf("attach evidence source turn: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM memory_candidates WHERE canonical_user_id = ? AND source_request_id = ? AND state = 'approved' AND published_memory_id IS NULL ORDER BY id`, userID, requestID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close() // nolint:errcheck
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// EnqueueFormationJob records one replay-safe extraction job per source turn/version.
func (s *Store) EnqueueFormationJob(ctx context.Context, source FormationSource, userID string) (int64, error) {
	if source.TurnID <= 0 || strings.TrimSpace(userID) == "" {
		return 0, fmt.Errorf("formation job requires tenant and source turn")
	}
	version := firstNonEmptyFormation(source.ExtractorVersion, FormationExtractorVersion)
	key := fmt.Sprintf("turn:%d:%s", source.TurnID, version)
	now := time.Now().UTC()
	if _, err := s.sql.ExecContext(ctx, `
INSERT INTO memory_formation_jobs (
	canonical_user_id, idempotency_key, job_type, state, source_request_id,
	source_session_id, source_session_generation, source_turn_id, extraction_model,
	extractor_version, available_at, created_at, updated_at
)
VALUES (?, ?, 'post_turn_extract', 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_user_id, idempotency_key) DO NOTHING
`, userID, key, source.RequestID, source.SessionID, source.SessionGeneration, source.TurnID,
		source.Model, version, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return 0, fmt.Errorf("enqueue memory formation job: %w", err)
	}
	var id int64
	if err := s.sql.QueryRowContext(ctx, `SELECT id FROM memory_formation_jobs WHERE canonical_user_id = ? AND idempotency_key = ?`, userID, key).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// MarkFormationEligible records successful response delivery before enqueue.
func (s *Store) MarkFormationEligible(ctx context.Context, userID string, turnID int64) error {
	now := formatTime(time.Now().UTC())
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark turn eligible: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	result, err := tx.ExecContext(ctx, `UPDATE session_turns SET formation_eligible_at = COALESCE(formation_eligible_at, ?), delivered_at = COALESCE(delivered_at, ?) WHERE id = ? AND canonical_user_id = ?`, now, now, turnID, userID)
	if err != nil {
		return fmt.Errorf("mark turn eligible for memory formation: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return sql.ErrNoRows
	}
	if err := enqueueDerivedChangeTx(ctx, tx, userID, "session_turn", turnID, "upsert", "formation-eligible:"+now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mark turn eligible: %w", err)
	}
	s.signalDerivedIndex()
	return nil
}

// ReconcileFormationJobs restores jobs for recent completed turns whose
// post-delivery enqueue was interrupted.
func (s *Store) ReconcileFormationJobs(ctx context.Context, model, version string) (int64, error) {
	version = firstNonEmptyFormation(version, FormationExtractorVersion)
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `
INSERT INTO memory_formation_jobs (
	canonical_user_id, idempotency_key, job_type, state, source_request_id,
	source_session_id, source_session_generation, source_turn_id, extraction_model,
	extractor_version, available_at, created_at, updated_at
)
SELECT turns.canonical_user_id, 'turn:' || turns.id || ':' || ?, 'post_turn_extract', 'queued',
	turns.source_request_id, turns.session_id, turns.session_generation, turns.id, ?, ?, ?, ?, ?
FROM session_turns turns
WHERE turns.created_at >= ? AND turns.formation_eligible_at IS NOT NULL
	AND NOT EXISTS (
		SELECT 1 FROM memory_formation_jobs jobs
		WHERE jobs.canonical_user_id = turns.canonical_user_id
			AND jobs.idempotency_key = 'turn:' || turns.id || ':' || ?
	)
`, version, model, version, formatTime(now), formatTime(now), formatTime(now), formatTime(now.Add(-24*time.Hour)), version)
	if err != nil {
		return 0, fmt.Errorf("reconcile memory formation jobs: %w", err)
	}
	return result.RowsAffected()
}

// RedriveDeadFormationJobs periodically retries jobs after prolonged outages.
func (s *Store) RedriveDeadFormationJobs(ctx context.Context, delay time.Duration) (int64, error) {
	if delay <= 0 {
		delay = 5 * time.Minute
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `
UPDATE memory_formation_jobs
SET state = 'retry', attempt_count = 0, available_at = ?, completed_at = NULL,
	redrive_count = redrive_count + 1, updated_at = ?
WHERE state = 'dead' AND redrive_count < 3 AND last_error_code LIKE 'transient_%'
	AND ((redrive_count = 0 AND updated_at <= ?)
		OR (redrive_count = 1 AND updated_at <= ?)
		OR (redrive_count = 2 AND updated_at <= ?))
`, formatTime(now), formatTime(now), formatTime(now.Add(-delay)), formatTime(now.Add(-2*delay)), formatTime(now.Add(-4*delay)))
	if err != nil {
		return 0, fmt.Errorf("redrive dead memory formation jobs: %w", err)
	}
	return result.RowsAffected()
}

// ClaimFormationJob leases the oldest ready job.
func (s *Store) ClaimFormationJob(ctx context.Context, lease time.Duration) (FormationJob, error) {
	if lease <= 0 {
		lease = time.Minute
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return FormationJob{}, err
	}
	defer tx.Rollback() // nolint:errcheck
	var job FormationJob
	err = tx.QueryRowContext(ctx, `
SELECT id, canonical_user_id, source_request_id, source_session_id,
	source_session_generation, COALESCE(source_turn_id, 0), extraction_model,
	extractor_version, attempt_count
FROM memory_formation_jobs
WHERE ((state IN ('queued', 'retry') AND available_at <= ?)
	OR (state = 'running' AND lease_until <= ?))
ORDER BY available_at, id LIMIT 1
`, formatTime(now), formatTime(now)).Scan(&job.ID, &job.UserID, &job.RequestID, &job.SessionID,
		&job.SessionGeneration, &job.TurnID, &job.Model, &job.ExtractorVersion, &job.AttemptCount)
	if err != nil {
		return FormationJob{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_formation_jobs SET state = 'running', attempt_count = attempt_count + 1, started_at = ?, lease_until = ?, updated_at = ? WHERE id = ? AND canonical_user_id = ?`, formatTime(now), formatTime(leaseUntil), formatTime(now), job.ID, job.UserID)
	if err != nil {
		return FormationJob{}, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return FormationJob{}, sql.ErrNoRows
	}
	job.AttemptCount++
	job.LeaseUntil = leaseUntil
	if err := tx.Commit(); err != nil {
		return FormationJob{}, err
	}
	return job, nil
}

// FormationJobArtifact returns the first persisted extractor result for replay.
func (s *Store) FormationJobArtifact(ctx context.Context, job FormationJob) (string, error) {
	var payload string
	err := s.sql.QueryRowContext(ctx, `SELECT extraction_payload FROM memory_formation_jobs WHERE id = ? AND canonical_user_id = ?`, job.ID, job.UserID).Scan(&payload)
	return payload, err
}

// SaveFormationJobArtifact persists the first extractor result and never revises it.
func (s *Store) SaveFormationJobArtifact(ctx context.Context, job FormationJob, payload string) error {
	if strings.TrimSpace(payload) == "" {
		payload = "[]"
	}
	_, err := s.sql.ExecContext(ctx, `UPDATE memory_formation_jobs SET extraction_payload = CASE WHEN extraction_payload = '' THEN ? ELSE extraction_payload END, updated_at = ? WHERE id = ? AND canonical_user_id = ? AND state = 'running'`, payload, formatTime(time.Now().UTC()), job.ID, job.UserID)
	return err
}

// CompleteFormationJob records a terminal successful or skipped state.
func (s *Store) CompleteFormationJob(ctx context.Context, job FormationJob, skipped bool) error {
	state := "succeeded"
	if skipped {
		state = "skipped"
	}
	now := time.Now().UTC()
	_, err := s.sql.ExecContext(ctx, `UPDATE memory_formation_jobs SET state = ?, completed_at = ?, lease_until = NULL, updated_at = ?, last_error_code = '' WHERE id = ? AND canonical_user_id = ? AND state = 'running'`, state, formatTime(now), formatTime(now), job.ID, job.UserID)
	return err
}

// SkipFormationJob terminally skips a running job that cannot succeed by retrying.
func (s *Store) SkipFormationJob(ctx context.Context, job FormationJob, code string) error {
	now := time.Now().UTC()
	_, err := s.sql.ExecContext(ctx, `UPDATE memory_formation_jobs SET state = 'skipped', completed_at = ?, lease_until = NULL, last_error_code = ?, updated_at = ? WHERE id = ? AND canonical_user_id = ? AND state = 'running'`, formatTime(now), safeErrorCode(code), formatTime(now), job.ID, job.UserID)
	return err
}

// RetryFormationJob releases a failed lease with bounded exponential backoff.
func (s *Store) RetryFormationJob(ctx context.Context, job FormationJob, code string, maxAttempts int) error {
	now := time.Now().UTC()
	state := "retry"
	if maxAttempts > 0 && job.AttemptCount >= maxAttempts {
		state = "dead"
	}
	delay := time.Duration(1<<min(job.AttemptCount, 6)) * time.Second
	_, err := s.sql.ExecContext(ctx, `UPDATE memory_formation_jobs SET state = ?, available_at = ?, lease_until = NULL, completed_at = CASE WHEN ? = 'dead' THEN ? ELSE NULL END, last_error_code = ?, updated_at = ? WHERE id = ? AND canonical_user_id = ? AND state = 'running'`, state, formatTime(now.Add(delay)), state, formatTime(now), safeErrorCode(code), formatTime(now), job.ID, job.UserID)
	return err
}

// FormationJobState returns one tenant-owned job state for observability/tests.
func (s *Store) FormationJobState(ctx context.Context, userID string, jobID int64) (string, error) {
	var state string
	err := s.sql.QueryRowContext(ctx, `SELECT state FROM memory_formation_jobs WHERE id = ? AND canonical_user_id = ?`, jobID, userID).Scan(&state)
	return state, err
}

// SessionTurnByID reloads a source turn under canonical tenant scope.
func (s *Store) SessionTurnByID(ctx context.Context, userID string, turnID int64) (StoredSessionTurn, error) {
	var turn StoredSessionTurn
	err := s.sql.QueryRowContext(ctx, `SELECT id, canonical_user_id, session_id, session_generation, user_text FROM session_turns WHERE id = ? AND canonical_user_id = ?`, turnID, userID).Scan(&turn.ID, &turn.UserID, &turn.SessionID, &turn.Generation, &turn.UserText)
	return turn, err
}

const candidateSelect = `SELECT id, canonical_user_id, state, scope, category, statement, evidence_summary, confidence, importance, provenance_type, source_authority, sensitivity, formation_mode, content_context, policy_decision, decision_reason, source_request_id, source_session_id, source_session_generation, COALESCE(source_turn_id, 0), extraction_model, extractor_version, COALESCE(supersedes_memory_id, 0), COALESCE((SELECT statement FROM memory_entries WHERE id = memory_candidates.supersedes_memory_id), ''), COALESCE(published_memory_id, 0), confirmation_session_id, confirmation_request_id, confirmation_presented_at, formation_eligible_at, expires_at, claim_key, claim_slot, claim_value FROM memory_candidates`

func loadCandidateByKeyTx(ctx context.Context, tx *sql.Tx, userID, key string) (FormationCandidate, error) {
	return scanFormationCandidate(tx.QueryRowContext(ctx, candidateSelect+` WHERE canonical_user_id = ? AND idempotency_key = ?`, userID, key))
}

func loadCandidateTx(ctx context.Context, tx *sql.Tx, userID string, id int64) (FormationCandidate, error) {
	return scanFormationCandidate(tx.QueryRowContext(ctx, candidateSelect+` WHERE canonical_user_id = ? AND id = ?`, userID, id))
}

func loadCandidateSQL(ctx context.Context, db *sql.DB, userID string, id int64) (FormationCandidate, error) {
	return scanFormationCandidate(db.QueryRowContext(ctx, candidateSelect+` WHERE canonical_user_id = ? AND id = ?`, userID, id))
}

func scanFormationCandidate(row interface{ Scan(...any) error }) (FormationCandidate, error) {
	var candidate FormationCandidate
	var presented, eligible, expires sql.NullString
	err := row.Scan(&candidate.ID, &candidate.UserID, &candidate.State, &candidate.Scope, &candidate.Category,
		&candidate.Statement, &candidate.Evidence, &candidate.Confidence, &candidate.Importance,
		&candidate.Provenance, &candidate.SourceAuthority, &candidate.Sensitivity,
		&candidate.FormationMode, &candidate.Context, &candidate.PolicyDecision, &candidate.DecisionReason,
		&candidate.SourceRequestID, &candidate.SourceSessionID, &candidate.SourceGeneration,
		&candidate.SourceTurnID, &candidate.ExtractionModel, &candidate.ExtractorVersion,
		&candidate.SupersedesMemoryID, &candidate.SupersedesStatement, &candidate.PublishedMemoryID, &candidate.ConfirmationSessionID,
		&candidate.ConfirmationRequestID, &presented, &eligible, &expires, &candidate.ClaimKey, &candidate.ClaimSlot, &candidate.ClaimValue)
	if err != nil {
		return FormationCandidate{}, err
	}
	if expires.Valid {
		candidate.ExpiresAt = parseTime(expires.String)
	}
	if presented.Valid {
		candidate.ConfirmationPresentedAt = parseTime(presented.String)
	}
	if eligible.Valid {
		candidate.FormationEligibleAt = parseTime(eligible.String)
	}
	return candidate, nil
}

func attachPublishedEvidenceTx(ctx context.Context, tx *sql.Tx, candidate FormationCandidate, memoryID int64) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO memory_evidence (canonical_user_id, memory_id, idempotency_key, evidence_type, content, source_authority, source_request_id, source_session_id, source_turn_id, created_at, provenance_type, relation_type, confidence_contribution, extraction_model, extractor_version, source_session_generation, correlation_key)
VALUES (?, ?, ?, 'exact_user_quote', ?, ?, ?, ?, ?, ?, ?, 'supports', ?, ?, ?, ?, ?)
ON CONFLICT(canonical_user_id, idempotency_key) DO NOTHING
`, candidate.UserID, memoryID, formationKey("memory-evidence", candidate.ID, memoryID), candidate.Evidence,
		candidate.SourceAuthority, candidate.SourceRequestID, candidate.SourceSessionID,
		nullableID(candidate.SourceTurnID), formatTime(time.Now().UTC()), candidate.Provenance,
		candidate.Confidence, candidate.ExtractionModel, candidate.ExtractorVersion,
		candidate.SourceGeneration, formationKey("correlation", candidate.SourceTurnID, candidate.ClaimKey))
	if err != nil {
		return fmt.Errorf("attach published memory evidence: %w", err)
	}
	return nil
}

func aggregateConfidence(current, contribution float64) float64 {
	combined := 1 - (1-current)*(1-contribution)
	if combined > 1 {
		return 1
	}
	if combined < 0 {
		return 0
	}
	return combined
}

func sourceAuthorityRank(authority string) int {
	switch authority {
	case "verified":
		return 4
	case string(memoryformation.AuthorityUserDirect):
		return 3
	case string(memoryformation.AuthorityModel):
		return 2
	default:
		return 1
	}
}

func candidateEvidenceStronger(newAuthority string, newConfidence float64, oldAuthority string, oldConfidence float64) bool {
	newRank, oldRank := sourceAuthorityRank(newAuthority), sourceAuthorityRank(oldAuthority)
	return newRank > oldRank || (newRank == oldRank && newConfidence > oldConfidence)
}

func strongestMemorySource(oldAuthority, oldProvenance, newAuthority, newProvenance string) (string, string) {
	if sourceAuthorityRank(newAuthority) > sourceAuthorityRank(oldAuthority) {
		return newAuthority, newProvenance
	}
	return oldAuthority, oldProvenance
}

func strongestSensitivity(oldSensitivity, newSensitivity string) string {
	rank := func(value string) int {
		switch value {
		case string(memoryformation.SensitivityHighImpactInteraction):
			return 3
		case string(memoryformation.SensitivityIdentityOrContact):
			return 2
		default:
			return 1
		}
	}
	if rank(newSensitivity) > rank(oldSensitivity) {
		return newSensitivity
	}
	return oldSensitivity
}

func profileApprovedForCandidate(candidate FormationCandidate) int {
	if candidate.SourceAuthority == string(memoryformation.AuthorityModel) || candidate.Provenance == string(memoryformation.ProvenanceModelInference) {
		return 0
	}
	return 1
}

func conditionalTurnID(statement string, turnID int64) int64 {
	if statement == "" {
		return 0
	}
	return turnID
}

func (s *Store) supersedeActiveMemoryTx(ctx context.Context, tx *sql.Tx, userID string, oldMemoryID, replacementMemoryID int64, now time.Time) error {
	if oldMemoryID <= 0 || oldMemoryID == replacementMemoryID {
		return nil
	}
	result, err := tx.ExecContext(ctx, `
UPDATE memory_entries SET status = 'superseded', invalidated_at = ?, invalidation_reason = 'explicit_correction', updated_at = ?
WHERE id = ? AND canonical_user_id = ? AND status = 'active'
`, formatTime(now), formatTime(now), oldMemoryID, userID)
	if err != nil {
		return fmt.Errorf("supersede old memory: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("superseded memory is no longer active")
	}
	if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", oldMemoryID, "delete", "supersede:"+formatTime(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_relations (canonical_user_id, idempotency_key, relation_type, source_memory_id, target_memory_id, created_at)
VALUES (?, ?, 'supersedes', ?, ?, ?)
ON CONFLICT(canonical_user_id, idempotency_key) DO NOTHING
`, userID, formationKey("supersedes", replacementMemoryID, oldMemoryID), replacementMemoryID, oldMemoryID, formatTime(now)); err != nil {
		return fmt.Errorf("record memory supersession relation: %w", err)
	}
	return nil
}

func markCandidatePublishedTx(ctx context.Context, tx *sql.Tx, candidate FormationCandidate, memoryID int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET published_memory_id = ?, updated_at = ? WHERE id = ? AND canonical_user_id = ? AND state = 'approved' AND published_memory_id IS NULL`, memoryID, formatTime(time.Now().UTC()), candidate.ID, candidate.UserID)
	if err != nil {
		return fmt.Errorf("mark memory candidate published: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("memory candidate publication lost idempotency race")
	}
	return nil
}

func insertFormationAuditTx(ctx context.Context, tx *sql.Tx, userID, key, event string, candidateID, memoryID, jobID int64, source FormationSource, actor, reason string) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO memory_formation_audit (canonical_user_id, idempotency_key, event_type, candidate_id, memory_id, job_id, request_id, session_id, turn_id, actor_type, created_at, metadata)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(canonical_user_id, idempotency_key) DO NOTHING
`, userID, key, event, nullableID(candidateID), nullableID(memoryID), nullableID(jobID), source.RequestID,
		source.SessionID, nullableID(source.TurnID), actor, formatTime(time.Now().UTC()), `{"reason":`+quoteProfileText(reason)+`}`)
	if err != nil {
		return fmt.Errorf("append memory formation audit: %w", err)
	}
	return nil
}

func (s *Store) formationStage(stage string) error {
	if s.formationFailpoint != nil {
		return s.formationFailpoint(stage)
	}
	return nil
}

func formationKey(values ...any) string {
	var parts []string
	for _, value := range values {
		parts = append(parts, fmt.Sprint(value))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func decisionTime(state string, now time.Time) any {
	if state == "approved" || state == "rejected" {
		return formatTime(now)
	}
	return nil
}

func decisionActor(state string, mode memoryformation.FormationMode) string {
	if state == "approved" && mode == memoryformation.ModeExplicitRemember {
		return "explicit_user_request"
	}
	if state == "approved" {
		return "formation_policy"
	}
	if state == "rejected" {
		return "formation_policy"
	}
	return ""
}

func candidateDecisionActor(candidate FormationCandidate) string {
	if candidate.FormationMode == string(memoryformation.ModeExplicitRemember) {
		return "explicit_user_request"
	}
	return "formation_policy"
}

func nullableFormationTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return formatTime(value)
}

func firstNonEmptyFormation(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func safeErrorCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	if len(value) > 80 {
		value = value[:80]
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (s *Store) findLikelyContradiction(ctx context.Context, userID, scope, category, statement string) (int64, error) {
	if category == string(memoryformation.CategoryNotes) || category == string(memoryformation.CategoryRelationships) {
		return 0, nil
	}
	candidateTokens := contradictionTokens(statement)
	if len(candidateTokens) < 3 || !hasContradictionSlot(candidateTokens) {
		return 0, nil
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT id, statement FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND category = ? AND status = 'active' AND approval_state = 'approved' ORDER BY updated_at DESC, id DESC`, userID, scope, category)
	if err != nil {
		return 0, fmt.Errorf("read authoritative memories for contradiction: %w", err)
	}
	defer rows.Close()
	var bestID int64
	bestScore := 0.0
	for rows.Next() {
		var id int64
		var existing string
		if err := rows.Scan(&id, &existing); err != nil {
			return 0, err
		}
		if statementKey(existing) == statementKey(statement) {
			continue
		}
		existingTokens := contradictionTokens(existing)
		if len(existingTokens) < 3 || !hasContradictionSlot(existingTokens) {
			continue
		}
		intersection := 0
		for token := range candidateTokens {
			if _, ok := existingTokens[token]; ok {
				intersection++
			}
		}
		denominator := min(len(candidateTokens), len(existingTokens))
		if denominator == 0 {
			continue
		}
		score := float64(intersection) / float64(denominator)
		if score >= 0.6 && score > bestScore {
			bestID, bestScore = id, score
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return bestID, nil
}

func contradictionTokens(value string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, token := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		token = strings.TrimSuffix(token, "s")
		if len([]rune(token)) < 2 || contradictionStopWords[token] {
			continue
		}
		tokens[token] = struct{}{}
	}
	return tokens
}

func hasContradictionSlot(tokens map[string]struct{}) bool {
	for _, token := range []string{"live", "prefer", "timezone", "name", "work", "use", "project", "environment", "located", "based", "want", "need", "call"} {
		if _, ok := tokens[token]; ok {
			return true
		}
	}
	return false
}

var contradictionStopWords = map[string]bool{
	"the": true, "user": true, "directly": true, "stated": true, "explicitly": true,
	"asked": true, "remember": true, "that": true, "this": true, "and": true,
}
