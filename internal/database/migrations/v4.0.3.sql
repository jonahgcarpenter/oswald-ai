ALTER TABLE durable_jobs ADD COLUMN compaction_model TEXT;
ALTER TABLE durable_jobs ADD COLUMN compaction_generator_version TEXT;
ALTER TABLE durable_jobs
ADD COLUMN compaction_invalid_output_retry_count INTEGER NOT NULL DEFAULT 0
CHECK (
    compaction_invalid_output_retry_count = 0
    OR (
        job_kind = 'session_compaction'
        AND compaction_invalid_output_retry_count = 1
    )
);

UPDATE durable_jobs
SET compaction_model = 'legacy-unknown',
    compaction_generator_version = 'legacy-unknown'
WHERE job_kind = 'session_compaction';

UPDATE durable_jobs
SET state = 'skipped',
    completed_at = COALESCE(completed_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    lease_owner = '',
    lease_until = NULL,
    last_error_code = 'legacy_compaction_contract',
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE job_kind = 'session_compaction'
  AND state IN ('queued', 'running', 'retry', 'dead');

DROP INDEX idx_durable_jobs_compaction_range;
CREATE UNIQUE INDEX idx_durable_jobs_compaction_contract_range
ON durable_jobs(
    canonical_user_id,
    session_id,
    session_generation,
    covered_from_turn_id,
    covered_through_turn_id,
    compaction_model,
    compaction_generator_version
)
WHERE job_kind = 'session_compaction';

CREATE UNIQUE INDEX idx_durable_jobs_compaction_active_scope
ON durable_jobs(canonical_user_id, session_id, session_generation)
WHERE job_kind = 'session_compaction' AND state IN ('queued', 'running', 'retry');

CREATE TRIGGER durable_jobs_compaction_contract_insert
BEFORE INSERT ON durable_jobs
WHEN (NEW.job_kind = 'session_compaction' AND (
        NEW.compaction_model IS NULL OR length(trim(NEW.compaction_model)) = 0
        OR NEW.compaction_generator_version IS NULL OR length(trim(NEW.compaction_generator_version)) = 0
    )) OR (NEW.job_kind != 'session_compaction' AND (
        NEW.compaction_model IS NOT NULL OR NEW.compaction_generator_version IS NOT NULL
    ))
BEGIN
    SELECT RAISE(ABORT, 'invalid session compaction contract');
END;

CREATE TRIGGER durable_jobs_compaction_contract_update
BEFORE UPDATE OF job_kind, compaction_model, compaction_generator_version ON durable_jobs
WHEN (NEW.job_kind = 'session_compaction' AND (
        NEW.compaction_model IS NULL OR length(trim(NEW.compaction_model)) = 0
        OR NEW.compaction_generator_version IS NULL OR length(trim(NEW.compaction_generator_version)) = 0
    )) OR (NEW.job_kind != 'session_compaction' AND (
        NEW.compaction_model IS NOT NULL OR NEW.compaction_generator_version IS NOT NULL
    )) OR NEW.compaction_model IS NOT OLD.compaction_model
    OR NEW.compaction_generator_version IS NOT OLD.compaction_generator_version
BEGIN
    SELECT RAISE(ABORT, 'invalid or mutable session compaction contract');
END;
