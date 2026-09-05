CREATE TABLE session_turns_v407 (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	canonical_user_id TEXT NOT NULL,
	user_text TEXT NOT NULL,
	assistant_text TEXT NOT NULL,
	tool_names TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	expires_at TEXT,
	session_generation INTEGER NOT NULL DEFAULT 1,
	delivered_at TEXT,
	source_request_id TEXT NOT NULL DEFAULT '',
	delivery_failed_at TEXT,
	compaction_pressure_tokens INTEGER CHECK (compaction_pressure_tokens IS NULL OR compaction_pressure_tokens >= 0),
	compaction_pressure_limit INTEGER CHECK (compaction_pressure_limit IS NULL OR compaction_pressure_limit > 0),
	compaction_pressure_version TEXT,
	tool_trace TEXT NOT NULL DEFAULT '{"version":1,"batches":[]}' CHECK (
		length(CAST(tool_trace AS BLOB)) <= 65536
		AND json_valid(tool_trace)
		AND json_type(tool_trace) = 'object'
		AND json_extract(tool_trace, '$.version') = 1
		AND json_type(tool_trace, '$.batches') = 'array'
	),
	tool_search_text TEXT NOT NULL DEFAULT '' CHECK (length(tool_search_text) <= 12000),
	foreground_memory TEXT NOT NULL DEFAULT '{"version":2,"candidates":[]}' CHECK (
		length(CAST(foreground_memory AS BLOB)) <= 32768
		AND json_valid(foreground_memory)
		AND json_type(foreground_memory) = 'object'
		AND json_extract(foreground_memory, '$.version') = 2
		AND json_type(foreground_memory, '$.candidates') = 'array'
		AND json_array_length(foreground_memory, '$.candidates') <= 5
	),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);

INSERT INTO session_turns_v407(
	id, session_id, canonical_user_id, user_text, assistant_text, tool_names, created_at, expires_at,
	session_generation, delivered_at, source_request_id, delivery_failed_at,
	compaction_pressure_tokens, compaction_pressure_limit, compaction_pressure_version,
	tool_trace, tool_search_text, foreground_memory
)
SELECT
	id, session_id, canonical_user_id, user_text, assistant_text, tool_names, created_at, expires_at,
	session_generation, delivered_at, source_request_id, delivery_failed_at,
	compaction_pressure_tokens, compaction_pressure_limit, compaction_pressure_version,
	tool_trace, tool_search_text, foreground_memory
FROM session_turns;

DROP TRIGGER session_summaries_range_insert;
DROP TRIGGER memory_candidates_tenant_insert;
DROP TRIGGER memory_candidates_tenant_update;
DROP TRIGGER durable_jobs_formation_source_insert;
DROP TRIGGER durable_jobs_formation_source_update;
DROP TRIGGER durable_jobs_compaction_range_insert;

DROP TABLE session_turns;
ALTER TABLE session_turns_v407 RENAME TO session_turns;

CREATE INDEX idx_session_turns_context ON session_turns(canonical_user_id, session_id, session_generation, created_at DESC, id DESC);
CREATE INDEX idx_session_turns_expiry ON session_turns(expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX idx_session_turns_pending_delivery ON session_turns(created_at, id) WHERE delivered_at IS NULL AND delivery_failed_at IS NULL;
CREATE INDEX idx_session_turns_compaction_pressure
ON session_turns(canonical_user_id, session_id, session_generation, id DESC)
WHERE delivered_at IS NOT NULL AND delivery_failed_at IS NULL AND compaction_pressure_tokens IS NOT NULL;

CREATE TRIGGER session_summaries_range_insert
BEFORE INSERT ON session_summaries
WHEN json_array_length(NEW.source_turn_ids) = 0
	OR EXISTS (
		SELECT 1 FROM json_each(NEW.source_turn_ids) source
		WHERE source.type != 'integer'
	)
	OR (SELECT value FROM json_each(NEW.source_turn_ids) ORDER BY key LIMIT 1) != NEW.covered_from_turn_id
	OR (SELECT value FROM json_each(NEW.source_turn_ids) ORDER BY key DESC LIMIT 1) != NEW.covered_through_turn_id
	OR EXISTS (
		SELECT 1
		FROM json_each(NEW.source_turn_ids) source
		JOIN json_each(NEW.source_turn_ids) previous ON previous.key = source.key - 1
		WHERE source.value <= previous.value
	)
	OR EXISTS (
		SELECT 1 FROM json_each(NEW.source_turn_ids) source
		WHERE NOT EXISTS (
			SELECT 1 FROM session_turns turn
			WHERE turn.id = source.value AND turn.canonical_user_id = NEW.canonical_user_id
				AND turn.session_id = NEW.session_id AND turn.session_generation = NEW.session_generation
				AND turn.delivered_at IS NOT NULL AND turn.delivery_failed_at IS NULL
		)
	)
BEGIN
	SELECT RAISE(ABORT, 'invalid session summary source turns');
END;

CREATE TRIGGER memory_candidates_tenant_insert
BEFORE INSERT ON memory_candidates
WHEN (NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.published_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.published_memory_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.supersedes_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.supersedes_memory_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory candidate reference');
END;

CREATE TRIGGER memory_candidates_tenant_update
BEFORE UPDATE OF canonical_user_id, source_turn_id, published_memory_id, supersedes_memory_id ON memory_candidates
WHEN (NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.published_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.published_memory_id AND canonical_user_id = NEW.canonical_user_id
)) OR (NEW.supersedes_memory_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM memory_entries WHERE id = NEW.supersedes_memory_id AND canonical_user_id = NEW.canonical_user_id
))
BEGIN
	SELECT RAISE(ABORT, 'cross-tenant memory candidate reference');
END;

CREATE TRIGGER durable_jobs_formation_source_insert
BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind = 'memory_formation' AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.source_session_id
		AND session_generation = NEW.source_session_generation
		AND source_request_id = NEW.source_request_id
		AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL
)
BEGIN
	SELECT RAISE(ABORT, 'invalid memory formation source turn');
END;

CREATE TRIGGER durable_jobs_formation_source_update
BEFORE UPDATE OF canonical_user_id, source_request_id, source_session_id, source_session_generation, source_turn_id ON durable_jobs
WHEN NEW.job_kind = 'memory_formation' AND NEW.source_turn_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.source_turn_id AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.source_session_id
		AND session_generation = NEW.source_session_generation
		AND source_request_id = NEW.source_request_id
		AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL
)
BEGIN
	SELECT RAISE(ABORT, 'invalid memory formation source turn');
END;

CREATE TRIGGER durable_jobs_compaction_range_insert
BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind = 'session_compaction' AND (
	NOT EXISTS (SELECT 1 FROM session_turns WHERE id = NEW.covered_from_turn_id AND canonical_user_id = NEW.canonical_user_id AND session_id = NEW.session_id AND session_generation = NEW.session_generation AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL)
	OR NOT EXISTS (SELECT 1 FROM session_turns WHERE id = NEW.covered_through_turn_id AND canonical_user_id = NEW.canonical_user_id AND session_id = NEW.session_id AND session_generation = NEW.session_generation AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL)
)
BEGIN
	SELECT RAISE(ABORT, 'invalid session compaction job turn range');
END;

CREATE TRIGGER session_turns_summary_content_update
BEFORE UPDATE OF user_text, assistant_text ON session_turns
WHEN EXISTS (
	SELECT 1 FROM session_summaries summary, json_each(summary.source_turn_ids) source
	WHERE CAST(source.value AS INTEGER) = OLD.id
)
BEGIN
	SELECT RAISE(ABORT, 'session summary source content is immutable');
END;

CREATE TRIGGER session_turns_summary_delete
BEFORE DELETE ON session_turns
WHEN EXISTS (
	SELECT 1 FROM session_summaries summary, json_each(summary.source_turn_ids) source
	WHERE CAST(source.value AS INTEGER) = OLD.id
)
BEGIN
	SELECT RAISE(ABORT, 'delete session summaries before source turns');
END;

CREATE TRIGGER session_turns_durable_compaction_update
BEFORE UPDATE OF canonical_user_id, session_id, session_generation ON session_turns
WHEN EXISTS (
	SELECT 1 FROM durable_jobs job
	WHERE job.job_kind = 'session_compaction' AND OLD.id IN (job.covered_from_turn_id, job.covered_through_turn_id)
		AND (job.canonical_user_id != NEW.canonical_user_id OR job.session_id != NEW.session_id OR job.session_generation != NEW.session_generation)
)
BEGIN
	SELECT RAISE(ABORT, 'session turn has compaction references');
END;

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

CREATE TRIGGER session_turns_tool_history_update
BEFORE UPDATE OF tool_trace, tool_search_text ON session_turns
WHEN NEW.tool_trace IS NOT OLD.tool_trace OR NEW.tool_search_text IS NOT OLD.tool_search_text
BEGIN
	SELECT RAISE(ABORT, 'immutable session turn tool history');
END;

CREATE TRIGGER session_turns_foreground_memory_update
BEFORE UPDATE OF foreground_memory ON session_turns
WHEN NEW.foreground_memory IS NOT OLD.foreground_memory
BEGIN
	SELECT RAISE(ABORT, 'immutable session turn foreground memory');
END;

ALTER TABLE durable_jobs
ADD COLUMN model_submission_count INTEGER NOT NULL DEFAULT 0
CHECK (
	model_submission_count BETWEEN 0 AND 3
	AND (
		(job_kind = 'memory_formation' AND formation_purpose = 'background_pattern')
		OR job_kind = 'session_compaction'
		OR model_submission_count = 0
	)
	AND (
		model_submission_count < 3
		OR (job_kind = 'memory_formation' AND extraction_payload != '')
		OR (job_kind = 'session_compaction' AND artifact_payload != '')
		OR state NOT IN ('queued', 'retry')
	)
);

UPDATE durable_jobs
SET model_submission_count = MIN(3,
	CASE
		WHEN job_kind = 'memory_formation' AND formation_purpose = 'background_pattern'
			THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + invalid_output_retry_count
		WHEN job_kind = 'session_compaction'
			THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + compaction_invalid_output_retry_count
		ELSE 0
	END
),
	state = CASE
		WHEN job_kind IN ('memory_formation', 'session_compaction')
			AND CASE WHEN job_kind = 'memory_formation' THEN extraction_payload ELSE artifact_payload END = ''
			AND state IN ('queued', 'running', 'retry')
			AND CASE
				WHEN job_kind = 'memory_formation' AND formation_purpose = 'background_pattern'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + invalid_output_retry_count
				WHEN job_kind = 'session_compaction'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + compaction_invalid_output_retry_count
				ELSE 0
			END >= 3
		THEN 'dead'
		ELSE state
	END,
	completed_at = CASE
		WHEN job_kind IN ('memory_formation', 'session_compaction')
			AND CASE WHEN job_kind = 'memory_formation' THEN extraction_payload ELSE artifact_payload END = ''
			AND state IN ('queued', 'running', 'retry')
			AND CASE
				WHEN job_kind = 'memory_formation' AND formation_purpose = 'background_pattern'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + invalid_output_retry_count
				WHEN job_kind = 'session_compaction'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + compaction_invalid_output_retry_count
				ELSE 0
			END >= 3
		THEN COALESCE(completed_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		ELSE completed_at
	END,
	lease_owner = CASE
		WHEN job_kind IN ('memory_formation', 'session_compaction')
			AND CASE WHEN job_kind = 'memory_formation' THEN extraction_payload ELSE artifact_payload END = ''
			AND state IN ('queued', 'running', 'retry')
			AND CASE
				WHEN job_kind = 'memory_formation' AND formation_purpose = 'background_pattern'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + invalid_output_retry_count
				WHEN job_kind = 'session_compaction'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + compaction_invalid_output_retry_count
				ELSE 0
			END >= 3
		THEN ''
		ELSE lease_owner
	END,
	lease_until = CASE
		WHEN job_kind IN ('memory_formation', 'session_compaction')
			AND CASE WHEN job_kind = 'memory_formation' THEN extraction_payload ELSE artifact_payload END = ''
			AND state IN ('queued', 'running', 'retry')
			AND CASE
				WHEN job_kind = 'memory_formation' AND formation_purpose = 'background_pattern'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + invalid_output_retry_count
				WHEN job_kind = 'session_compaction'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + compaction_invalid_output_retry_count
				ELSE 0
			END >= 3
		THEN NULL
		ELSE lease_until
	END,
	last_error_code = CASE
		WHEN job_kind IN ('memory_formation', 'session_compaction')
			AND CASE WHEN job_kind = 'memory_formation' THEN extraction_payload ELSE artifact_payload END = ''
			AND state IN ('queued', 'running', 'retry')
			AND CASE
				WHEN job_kind = 'memory_formation' AND formation_purpose = 'background_pattern'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + invalid_output_retry_count
				WHEN job_kind = 'session_compaction'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + compaction_invalid_output_retry_count
				ELSE 0
			END >= 3
		THEN 'model_submission_budget_exhausted'
		ELSE last_error_code
	END,
	updated_at = CASE
		WHEN job_kind IN ('memory_formation', 'session_compaction')
			AND CASE WHEN job_kind = 'memory_formation' THEN extraction_payload ELSE artifact_payload END = ''
			AND state IN ('queued', 'running', 'retry')
			AND CASE
				WHEN job_kind = 'memory_formation' AND formation_purpose = 'background_pattern'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + invalid_output_retry_count
				WHEN job_kind = 'session_compaction'
					THEN attempt_count + CASE WHEN redrive_count > 0 THEN 3 ELSE 0 END + compaction_invalid_output_retry_count
				ELSE 0
			END >= 3
		THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		ELSE updated_at
	END;

ALTER TABLE durable_jobs
ADD COLUMN corrective_error_code TEXT NOT NULL DEFAULT ''
CHECK (
	length(corrective_error_code) <= 128
	AND (
		corrective_error_code = ''
		OR (job_kind = 'memory_formation' AND formation_purpose = 'background_pattern' AND invalid_output_retry_count = 1)
		OR (job_kind = 'session_compaction' AND compaction_invalid_output_retry_count = 1)
	)
);

UPDATE durable_jobs
SET corrective_error_code = last_error_code
WHERE last_error_code != ''
	AND length(last_error_code) <= 128
	AND (
		(job_kind = 'memory_formation' AND formation_purpose = 'background_pattern' AND invalid_output_retry_count = 1)
		OR (job_kind = 'session_compaction' AND compaction_invalid_output_retry_count = 1)
	);
