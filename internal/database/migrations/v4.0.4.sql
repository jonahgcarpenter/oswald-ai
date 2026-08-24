ALTER TABLE session_turns ADD COLUMN compaction_pressure_tokens INTEGER
CHECK (compaction_pressure_tokens IS NULL OR compaction_pressure_tokens >= 0);
ALTER TABLE session_turns ADD COLUMN compaction_pressure_limit INTEGER
CHECK (compaction_pressure_limit IS NULL OR compaction_pressure_limit > 0);
ALTER TABLE session_turns ADD COLUMN compaction_pressure_version TEXT;

CREATE TRIGGER session_turns_compaction_pressure_insert
BEFORE INSERT ON session_turns
WHEN (NEW.compaction_pressure_tokens IS NULL) != (NEW.compaction_pressure_limit IS NULL)
    OR (NEW.compaction_pressure_tokens IS NULL) != (NEW.compaction_pressure_version IS NULL)
    OR (NEW.compaction_pressure_version IS NOT NULL AND length(trim(NEW.compaction_pressure_version)) = 0)
BEGIN
    SELECT RAISE(ABORT, 'invalid session turn compaction pressure');
END;

CREATE TRIGGER session_turns_compaction_pressure_update
BEFORE UPDATE OF compaction_pressure_tokens, compaction_pressure_limit, compaction_pressure_version ON session_turns
WHEN NEW.compaction_pressure_tokens IS NOT OLD.compaction_pressure_tokens
    OR NEW.compaction_pressure_limit IS NOT OLD.compaction_pressure_limit
    OR NEW.compaction_pressure_version IS NOT OLD.compaction_pressure_version
BEGIN
    SELECT RAISE(ABORT, 'immutable session turn compaction pressure');
END;

CREATE INDEX idx_session_turns_compaction_pressure
ON session_turns(canonical_user_id, session_id, session_generation, id DESC)
WHERE delivered_at IS NOT NULL AND delivery_failed_at IS NULL AND compaction_pressure_tokens IS NOT NULL;

ALTER TABLE durable_jobs ADD COLUMN compaction_target_turn_id INTEGER;

UPDATE durable_jobs
SET compaction_target_turn_id = covered_through_turn_id
WHERE job_kind = 'session_compaction';

CREATE TRIGGER durable_jobs_compaction_target_insert
BEFORE INSERT ON durable_jobs
WHEN (NEW.job_kind = 'session_compaction' AND (
        NEW.compaction_target_turn_id IS NULL
        OR NEW.compaction_target_turn_id < NEW.covered_through_turn_id
    )) OR (NEW.job_kind != 'session_compaction' AND NEW.compaction_target_turn_id IS NOT NULL)
BEGIN
    SELECT RAISE(ABORT, 'invalid session compaction target');
END;

CREATE TRIGGER durable_jobs_compaction_target_update
BEFORE UPDATE OF job_kind, covered_through_turn_id, compaction_target_turn_id ON durable_jobs
WHEN (NEW.job_kind = 'session_compaction' AND (
        NEW.compaction_target_turn_id IS NULL
        OR NEW.compaction_target_turn_id < NEW.covered_through_turn_id
    )) OR (NEW.job_kind != 'session_compaction' AND NEW.compaction_target_turn_id IS NOT NULL)
    OR NEW.compaction_target_turn_id IS NOT OLD.compaction_target_turn_id
BEGIN
    SELECT RAISE(ABORT, 'invalid or mutable session compaction target');
END;
