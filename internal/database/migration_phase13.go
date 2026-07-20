package database

import (
	"context"
	"database/sql"
	"fmt"
)

const phase13ConfidenceEvidenceMemorySQL = `
ALTER TABLE memory_candidates ADD COLUMN claim_key TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_candidates ADD COLUMN claim_slot TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_candidates ADD COLUMN claim_value TEXT NOT NULL DEFAULT '';

ALTER TABLE memory_entries ADD COLUMN claim_key TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_entries ADD COLUMN claim_slot TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_entries ADD COLUMN claim_value TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_entries ADD COLUMN evidence_count INTEGER NOT NULL DEFAULT 1 CHECK (evidence_count > 0);

ALTER TABLE memory_evidence ADD COLUMN provenance_type TEXT NOT NULL DEFAULT 'legacy_import';
ALTER TABLE memory_evidence ADD COLUMN relation_type TEXT NOT NULL DEFAULT 'supports';
ALTER TABLE memory_evidence ADD COLUMN confidence_contribution REAL NOT NULL DEFAULT 0 CHECK (confidence_contribution >= 0 AND confidence_contribution <= 1);
ALTER TABLE memory_evidence ADD COLUMN extraction_model TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_evidence ADD COLUMN extractor_version TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_evidence ADD COLUMN source_session_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE memory_evidence ADD COLUMN correlation_key TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_memory_candidates_claim_key
	ON memory_candidates (canonical_user_id, scope, claim_key);
CREATE INDEX idx_memory_entries_claim_key
	ON memory_entries (canonical_user_id, scope, claim_key);
CREATE INDEX idx_memory_entries_claim_slot_status
	ON memory_entries (canonical_user_id, scope, claim_slot, status);

UPDATE memory_candidates
SET claim_key = 'legacy:' || id,
	claim_slot = category || '.legacy',
	claim_value = statement_key
WHERE statement != '';

UPDATE memory_entries
SET claim_key = 'legacy:' || id,
	claim_slot = category || '.legacy',
	claim_value = statement_key,
	evidence_count = MAX(1, (SELECT COUNT(*) FROM memory_evidence evidence WHERE evidence.memory_id = memory_entries.id AND evidence.canonical_user_id = memory_entries.canonical_user_id))
WHERE statement != '';

UPDATE memory_evidence
SET provenance_type = COALESCE(
		(SELECT provenance_type FROM memory_candidates candidate WHERE candidate.id = memory_evidence.candidate_id),
		(SELECT provenance_type FROM memory_entries memory WHERE memory.id = memory_evidence.memory_id),
		'legacy_import'
	),
	confidence_contribution = COALESCE(
		(SELECT confidence FROM memory_candidates candidate WHERE candidate.id = memory_evidence.candidate_id),
		(SELECT confidence FROM memory_entries memory WHERE memory.id = memory_evidence.memory_id),
		0
	),
	source_session_generation = COALESCE(
		(SELECT source_session_generation FROM memory_candidates candidate WHERE candidate.id = memory_evidence.candidate_id),
		0
	);

UPDATE memory_candidates
SET state = CASE
		WHEN formation_mode = 'explicit_remember' OR confidence >= 0.35 THEN 'approved'
		ELSE 'proposed'
	END,
	policy_decision = CASE
		WHEN formation_mode = 'explicit_remember' OR confidence >= 0.35 THEN 'automatic'
		ELSE 'proposed'
	END,
	decided_at = CASE
		WHEN formation_mode = 'explicit_remember' OR confidence >= 0.35 THEN COALESCE(decided_at, updated_at)
		ELSE NULL
	END,
	decided_by = CASE
		WHEN formation_mode = 'explicit_remember' THEN 'explicit_user_request'
		WHEN confidence >= 0.35 THEN 'formation_policy'
		ELSE ''
	END,
	decision_reason = CASE
		WHEN formation_mode = 'explicit_remember' THEN 'user explicitly requested this memory'
		WHEN confidence >= 0.35 THEN 'confidence threshold met'
		ELSE 'candidate requires review'
	END
WHERE state = 'pending_confirmation';

UPDATE memory_candidates
SET confirmation_session_id = '',
	confirmation_request_id = '',
	confirmation_presented_at = NULL
WHERE confirmation_session_id != ''
	OR confirmation_request_id != ''
	OR confirmation_presented_at IS NOT NULL;

DELETE FROM memory_confirmation_presentations;
`

const phase13MigrationDefinition = phase13ConfidenceEvidenceMemorySQL

func applyPhase13Migration(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, phase13ConfidenceEvidenceMemorySQL); err != nil {
		return fmt.Errorf("add confidence evidence memory schema: %w", err)
	}
	return nil
}
