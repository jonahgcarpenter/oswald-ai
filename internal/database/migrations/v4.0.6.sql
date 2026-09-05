ALTER TABLE session_turns ADD COLUMN foreground_memory TEXT NOT NULL DEFAULT '{"version":2,"candidates":[]}'
CHECK (
	length(CAST(foreground_memory AS BLOB)) <= 32768
	AND json_valid(foreground_memory)
	AND json_type(foreground_memory) = 'object'
	AND json_extract(foreground_memory, '$.version') = 2
	AND json_type(foreground_memory, '$.candidates') = 'array'
	AND json_array_length(foreground_memory, '$.candidates') <= 2
);

CREATE TRIGGER session_turns_foreground_memory_update
BEFORE UPDATE OF foreground_memory ON session_turns
WHEN NEW.foreground_memory IS NOT OLD.foreground_memory
BEGIN
	SELECT RAISE(ABORT, 'immutable session turn foreground memory');
END;

ALTER TABLE durable_jobs ADD COLUMN formation_purpose TEXT
CHECK (formation_purpose IS NULL OR formation_purpose IN ('background_pattern', 'agent_save'));

UPDATE durable_jobs
SET formation_purpose = 'background_pattern'
WHERE job_kind = 'memory_formation';

CREATE TRIGGER durable_jobs_formation_purpose_insert
BEFORE INSERT ON durable_jobs
WHEN (NEW.job_kind = 'memory_formation' AND NEW.formation_purpose IS NULL)
	OR (NEW.job_kind != 'memory_formation' AND NEW.formation_purpose IS NOT NULL)
BEGIN
	SELECT RAISE(ABORT, 'invalid durable job formation purpose');
END;

CREATE TRIGGER durable_jobs_formation_purpose_update
BEFORE UPDATE OF job_kind, formation_purpose ON durable_jobs
WHEN (NEW.job_kind = 'memory_formation' AND NEW.formation_purpose IS NULL)
	OR (NEW.job_kind != 'memory_formation' AND NEW.formation_purpose IS NOT NULL)
	OR NEW.formation_purpose IS NOT OLD.formation_purpose
BEGIN
	SELECT RAISE(ABORT, 'invalid durable job formation purpose');
END;

CREATE TRIGGER durable_jobs_pattern_context_insert
BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind = 'memory_formation' AND NEW.extractor_version = 'pattern-v1' AND (
	NEW.formation_purpose != 'background_pattern'
	OR NOT json_valid(NEW.artifact_payload)
	OR json_type(NEW.artifact_payload) != 'object'
	OR json_extract(NEW.artifact_payload, '$.version') != 1
	OR json_type(NEW.artifact_payload, '$.turn_ids') != 'array'
	OR json_array_length(NEW.artifact_payload, '$.turn_ids') NOT BETWEEN 2 AND 8
	OR (SELECT COUNT(*) FROM json_each(NEW.artifact_payload)) != 2
	OR (SELECT COUNT(*) FROM json_each(NEW.artifact_payload, '$.turn_ids') WHERE type != 'integer' OR value <= 0) != 0
	OR (SELECT COUNT(DISTINCT value) FROM json_each(NEW.artifact_payload, '$.turn_ids')) != json_array_length(NEW.artifact_payload, '$.turn_ids')
	OR json_extract(NEW.artifact_payload, '$.turn_ids[#-1]') != NEW.source_turn_id
)
BEGIN
	SELECT RAISE(ABORT, 'invalid durable pattern context');
END;

CREATE TRIGGER durable_jobs_pattern_context_update
BEFORE UPDATE OF artifact_payload ON durable_jobs
WHEN OLD.job_kind = 'memory_formation' AND NEW.artifact_payload IS NOT OLD.artifact_payload
BEGIN
	SELECT RAISE(ABORT, 'immutable durable pattern context');
END;
