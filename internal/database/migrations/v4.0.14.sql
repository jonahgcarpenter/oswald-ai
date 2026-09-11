CREATE TABLE user_documents (
    id TEXT PRIMARY KEY NOT NULL,
    canonical_user_id TEXT NOT NULL REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
    filename TEXT NOT NULL,
    media_type TEXT NOT NULL,
    admission_key TEXT,
    source_hash TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('queued','extracting','ready','partial','failed')),
    source_bytes INTEGER NOT NULL CHECK(source_bytes > 0 AND source_bytes <= 20971520),
    text_bytes INTEGER NOT NULL DEFAULT 0 CHECK(text_bytes BETWEEN 0 AND 1048576),
    reserved_bytes INTEGER NOT NULL DEFAULT 1048576 CHECK(reserved_bytes IN (0,1048576)),
    accepted_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK(expires_at > accepted_at),
    chunk_count INTEGER NOT NULL DEFAULT 0 CHECK(chunk_count BETWEEN 0 AND 1024),
    UNIQUE(canonical_user_id, admission_key)
);
CREATE INDEX user_documents_owner ON user_documents(canonical_user_id, id);
CREATE INDEX user_documents_expiry ON user_documents(expires_at, id);
CREATE TABLE user_document_sources (
    document_id TEXT PRIMARY KEY NOT NULL REFERENCES user_documents(id) ON DELETE CASCADE,
    data BLOB NOT NULL CHECK(length(data) BETWEEN 1 AND 20971520)
);
CREATE TABLE user_document_chunks (
    document_id TEXT NOT NULL REFERENCES user_documents(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK(ordinal BETWEEN 0 AND 1023),
    text TEXT NOT NULL CHECK(length(CAST(text AS BLOB)) BETWEEN 1 AND 16384),
    locator TEXT NOT NULL CHECK(length(CAST(locator AS BLOB)) <= 1024),
    method TEXT NOT NULL CHECK(length(CAST(method AS BLOB)) <= 64),
    PRIMARY KEY(document_id, ordinal)
);
CREATE TABLE user_document_extraction_jobs (
    document_id TEXT PRIMARY KEY NOT NULL REFERENCES user_documents(id) ON DELETE CASCADE,
    state TEXT NOT NULL CHECK(state IN ('queued','running','succeeded','failed')),
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_token TEXT NOT NULL DEFAULT '',
    lease_until INTEGER NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    failure_code TEXT NOT NULL DEFAULT ''
);
CREATE INDEX user_document_extraction_ready ON user_document_extraction_jobs(state, lease_until);
CREATE TABLE user_document_reservations (
    id TEXT PRIMARY KEY NOT NULL,
    canonical_user_id TEXT NOT NULL REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
    file_count INTEGER NOT NULL CHECK(file_count BETWEEN 1 AND 4),
    source_bytes INTEGER NOT NULL CHECK(source_bytes >= file_count AND source_bytes <= 41943040 AND source_bytes <= file_count * 20971520),
    reserved_text_bytes INTEGER NOT NULL CHECK(reserved_text_bytes = file_count * 1048576),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK(expires_at = created_at + 300000)
);
CREATE INDEX user_document_reservations_owner ON user_document_reservations(canonical_user_id, expires_at);
CREATE INDEX user_document_reservations_expiry ON user_document_reservations(expires_at, id);

-- Preserve AUTOINCREMENT high-water marks even when the highest rows were deleted.
CREATE TEMP TABLE document_index_sequences AS SELECT name,seq FROM sqlite_sequence WHERE name IN ('durable_jobs','derived_index_revisions');
CREATE TABLE derived_index_revisions_v414 (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 index_kind TEXT NOT NULL CHECK(index_kind IN ('memory_fts','transcript_fts','memory_vector','global_memory_fts','global_memory_vector','user_document_fts','user_document_vector')),
 model TEXT NOT NULL DEFAULT '', dimension INTEGER NOT NULL DEFAULT 0 CHECK(dimension>=0),
 schema_version INTEGER NOT NULL CHECK(schema_version>0), revision INTEGER NOT NULL CHECK(revision>0),
 table_name TEXT NOT NULL UNIQUE, state TEXT NOT NULL CHECK(state IN ('building','live','failed','retired')),
 expected_count INTEGER NOT NULL DEFAULT 0 CHECK(expected_count>=0), indexed_count INTEGER NOT NULL DEFAULT 0 CHECK(indexed_count>=0),
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_error_code TEXT NOT NULL DEFAULT '',
 UNIQUE(index_kind,revision), CHECK(indexed_count<=expected_count)
);
INSERT INTO derived_index_revisions_v414 SELECT * FROM derived_index_revisions;
DROP TABLE derived_index_revisions;
ALTER TABLE derived_index_revisions_v414 RENAME TO derived_index_revisions;
CREATE UNIQUE INDEX idx_derived_index_revisions_live_kind ON derived_index_revisions(index_kind) WHERE state='live';
CREATE INDEX idx_derived_index_revisions_state ON derived_index_revisions(state,updated_at,id);

CREATE TABLE durable_jobs_v414 (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 job_kind TEXT NOT NULL CHECK(job_kind IN ('memory_formation','session_compaction','derived_index')),
 idempotency_key TEXT NOT NULL,
 canonical_user_id TEXT REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
 state TEXT NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','running','retry','succeeded','skipped','dead')),
 source_request_id TEXT NOT NULL DEFAULT '', source_session_id TEXT NOT NULL DEFAULT '',
 source_session_generation INTEGER NOT NULL DEFAULT 0 CHECK(source_session_generation>=0),
 source_turn_id INTEGER REFERENCES session_turns(id) ON DELETE SET NULL,
 extraction_model TEXT NOT NULL DEFAULT '', extractor_version TEXT NOT NULL DEFAULT '', extraction_payload TEXT NOT NULL DEFAULT '',
 session_id TEXT, session_generation INTEGER, covered_from_turn_id INTEGER, covered_through_turn_id INTEGER,
 artifact_payload TEXT NOT NULL DEFAULT '', artifact_summary_id INTEGER REFERENCES session_summaries(id) ON DELETE SET NULL,
 entity_kind TEXT, entity_id INTEGER, operation TEXT,
 attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count>=0),
 available_at TEXT NOT NULL, lease_owner TEXT NOT NULL DEFAULT '', lease_until TEXT, completed_at TEXT,
 last_error_code TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL,
 invalid_output_retry_count INTEGER NOT NULL DEFAULT 0 CHECK(invalid_output_retry_count=0 OR (job_kind='memory_formation' AND invalid_output_retry_count=1)),
 compaction_model TEXT, compaction_generator_version TEXT,
 compaction_invalid_output_retry_count INTEGER NOT NULL DEFAULT 0 CHECK(compaction_invalid_output_retry_count=0 OR (job_kind='session_compaction' AND compaction_invalid_output_retry_count BETWEEN 1 AND 3)),
 compaction_target_turn_id INTEGER,
 formation_purpose TEXT CHECK(formation_purpose IS NULL OR formation_purpose IN ('background_pattern','agent_save')),
 model_submission_count INTEGER NOT NULL DEFAULT 0 CHECK(model_submission_count>=0
 AND ((job_kind='memory_formation' AND formation_purpose='background_pattern' AND model_submission_count<=3) OR (job_kind='session_compaction' AND model_submission_count<=4) OR model_submission_count=0)
 AND (model_submission_count<CASE WHEN job_kind='session_compaction' THEN 4 ELSE 3 END OR (job_kind='memory_formation' AND extraction_payload!='') OR (job_kind='session_compaction' AND artifact_payload!='') OR state NOT IN ('queued','retry'))),
 corrective_error_code TEXT NOT NULL DEFAULT '' CHECK(length(corrective_error_code)<=128 AND (corrective_error_code='' OR (job_kind='memory_formation' AND formation_purpose='background_pattern' AND invalid_output_retry_count=1) OR (job_kind='session_compaction' AND compaction_invalid_output_retry_count BETWEEN 1 AND 3))),
 UNIQUE(job_kind,idempotency_key),
 CHECK((job_kind='derived_index' AND entity_kind='global_memory' AND canonical_user_id IS NULL) OR (NOT(job_kind='derived_index' AND entity_kind='global_memory') AND canonical_user_id IS NOT NULL)),
 CHECK(job_kind!='memory_formation' OR (source_session_generation>0 AND extractor_version!='')),
 CHECK(job_kind NOT IN ('memory_formation','derived_index') OR ((state='running' AND lease_owner!='' AND lease_until IS NOT NULL) OR (state!='running' AND lease_owner='' AND lease_until IS NULL))),
 CHECK(job_kind!='session_compaction' OR (session_id IS NOT NULL AND session_generation>0 AND covered_from_turn_id>0 AND covered_through_turn_id>=covered_from_turn_id)),
 CHECK(job_kind!='session_compaction' OR attempt_count<=4),
 CHECK(job_kind!='derived_index' OR (entity_kind IN ('memory','session_turn','global_memory','user_document_chunk') AND entity_id>0 AND operation IN ('upsert','delete')))
);
INSERT INTO durable_jobs_v414 SELECT * FROM durable_jobs;
-- External triggers must be absent during the table-name gap. Recreate exactly.
DROP TRIGGER session_turns_durable_compaction_update;
DROP TRIGGER memory_assessment_input_source_insert;
DROP TABLE durable_jobs;
ALTER TABLE durable_jobs_v414 RENAME TO durable_jobs;
UPDATE sqlite_sequence SET seq=max(seq,COALESCE((SELECT seq FROM document_index_sequences saved WHERE saved.name=sqlite_sequence.name),0)) WHERE name IN ('durable_jobs','derived_index_revisions');
DROP TABLE document_index_sequences;
CREATE UNIQUE INDEX idx_durable_jobs_compaction_contract_range ON durable_jobs(canonical_user_id,session_id,session_generation,covered_from_turn_id,covered_through_turn_id,compaction_model,compaction_generator_version) WHERE job_kind='session_compaction';
CREATE UNIQUE INDEX idx_durable_jobs_compaction_active_scope ON durable_jobs(canonical_user_id,session_id,session_generation) WHERE job_kind='session_compaction' AND state IN ('queued','running','retry');
CREATE INDEX idx_durable_jobs_ready ON durable_jobs(job_kind,state,available_at,id);
CREATE INDEX idx_durable_jobs_tenant ON durable_jobs(canonical_user_id,job_kind,state,id) WHERE canonical_user_id IS NOT NULL;
CREATE INDEX idx_durable_jobs_session ON durable_jobs(canonical_user_id,session_id,session_generation,state,id) WHERE job_kind='session_compaction';
CREATE INDEX idx_durable_jobs_entity ON durable_jobs(canonical_user_id,entity_kind,entity_id,id) WHERE job_kind='derived_index';

CREATE TRIGGER session_turns_durable_compaction_update BEFORE UPDATE OF canonical_user_id,session_id,session_generation ON session_turns
WHEN EXISTS(SELECT 1 FROM durable_jobs job WHERE job.job_kind='session_compaction' AND OLD.id IN (job.covered_from_turn_id,job.covered_through_turn_id) AND (job.canonical_user_id!=NEW.canonical_user_id OR job.session_id!=NEW.session_id OR job.session_generation!=NEW.session_generation))
BEGIN SELECT RAISE(ABORT,'session turn has compaction references'); END;
CREATE TRIGGER memory_assessment_input_source_insert BEFORE INSERT ON memory_assessment_inputs
WHEN NOT EXISTS(SELECT 1 FROM durable_jobs j WHERE j.id=NEW.job_id AND j.job_kind='memory_formation' AND j.canonical_user_id=NEW.canonical_user_id AND json_extract(NEW.payload,'$.Anchor.ID') IS j.source_turn_id AND json_extract(NEW.payload,'$.Anchor.UserID') IS j.canonical_user_id AND json_extract(NEW.payload,'$.Anchor.SessionID') IS j.source_session_id AND json_extract(NEW.payload,'$.Anchor.Generation') IS j.source_session_generation)
BEGIN SELECT RAISE(ABORT,'invalid frozen assessment source identity'); END;
CREATE TRIGGER durable_jobs_formation_source_insert BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind='memory_formation' AND NOT EXISTS(SELECT 1 FROM session_turns WHERE id=NEW.source_turn_id AND canonical_user_id=NEW.canonical_user_id AND session_id=NEW.source_session_id AND session_generation=NEW.source_session_generation AND source_request_id=NEW.source_request_id AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL)
BEGIN SELECT RAISE(ABORT,'invalid memory formation source turn'); END;
CREATE TRIGGER durable_jobs_formation_source_update BEFORE UPDATE OF canonical_user_id,source_request_id,source_session_id,source_session_generation,source_turn_id ON durable_jobs
WHEN NEW.job_kind='memory_formation' AND NEW.source_turn_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM session_turns WHERE id=NEW.source_turn_id AND canonical_user_id=NEW.canonical_user_id AND session_id=NEW.source_session_id AND session_generation=NEW.source_session_generation AND source_request_id=NEW.source_request_id AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL)
BEGIN SELECT RAISE(ABORT,'invalid memory formation source turn'); END;
CREATE TRIGGER durable_jobs_compaction_range_insert BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind='session_compaction' AND (NOT EXISTS(SELECT 1 FROM session_turns WHERE id=NEW.covered_from_turn_id AND canonical_user_id=NEW.canonical_user_id AND session_id=NEW.session_id AND session_generation=NEW.session_generation AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL) OR NOT EXISTS(SELECT 1 FROM session_turns WHERE id=NEW.covered_through_turn_id AND canonical_user_id=NEW.canonical_user_id AND session_id=NEW.session_id AND session_generation=NEW.session_generation AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL))
BEGIN SELECT RAISE(ABORT,'invalid session compaction job turn range'); END;
CREATE TRIGGER durable_jobs_compaction_range_update BEFORE UPDATE OF canonical_user_id,session_id,session_generation,covered_from_turn_id,covered_through_turn_id ON durable_jobs
WHEN OLD.job_kind='session_compaction' AND (NEW.canonical_user_id!=OLD.canonical_user_id OR NEW.session_id!=OLD.session_id OR NEW.session_generation!=OLD.session_generation OR NEW.covered_from_turn_id!=OLD.covered_from_turn_id OR NEW.covered_through_turn_id!=OLD.covered_through_turn_id)
BEGIN SELECT RAISE(ABORT,'session compaction job range is immutable'); END;
CREATE TRIGGER durable_jobs_compaction_artifact_update BEFORE UPDATE OF artifact_summary_id ON durable_jobs
WHEN NEW.job_kind='session_compaction' AND NEW.artifact_summary_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM session_summaries WHERE id=NEW.artifact_summary_id AND canonical_user_id=NEW.canonical_user_id AND session_id=NEW.session_id AND session_generation=NEW.session_generation AND covered_from_turn_id=NEW.covered_from_turn_id AND covered_through_turn_id=NEW.covered_through_turn_id)
BEGIN SELECT RAISE(ABORT,'invalid session compaction artifact'); END;
CREATE TRIGGER durable_jobs_compaction_artifact_insert BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind='session_compaction' AND NEW.artifact_summary_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM session_summaries WHERE id=NEW.artifact_summary_id AND canonical_user_id=NEW.canonical_user_id AND session_id=NEW.session_id AND session_generation=NEW.session_generation AND covered_from_turn_id=NEW.covered_from_turn_id AND covered_through_turn_id=NEW.covered_through_turn_id)
BEGIN SELECT RAISE(ABORT,'invalid session compaction artifact'); END;
CREATE TRIGGER durable_jobs_compaction_contract_insert BEFORE INSERT ON durable_jobs
WHEN (NEW.job_kind='session_compaction' AND (NEW.compaction_model IS NULL OR length(trim(NEW.compaction_model))=0 OR NEW.compaction_generator_version IS NULL OR length(trim(NEW.compaction_generator_version))=0)) OR (NEW.job_kind!='session_compaction' AND (NEW.compaction_model IS NOT NULL OR NEW.compaction_generator_version IS NOT NULL))
BEGIN SELECT RAISE(ABORT,'invalid session compaction contract'); END;
CREATE TRIGGER durable_jobs_compaction_contract_update BEFORE UPDATE OF job_kind,compaction_model,compaction_generator_version ON durable_jobs
WHEN (NEW.job_kind='session_compaction' AND (NEW.compaction_model IS NULL OR length(trim(NEW.compaction_model))=0 OR NEW.compaction_generator_version IS NULL OR length(trim(NEW.compaction_generator_version))=0)) OR (NEW.job_kind!='session_compaction' AND (NEW.compaction_model IS NOT NULL OR NEW.compaction_generator_version IS NOT NULL)) OR NEW.compaction_model IS NOT OLD.compaction_model OR NEW.compaction_generator_version IS NOT OLD.compaction_generator_version
BEGIN SELECT RAISE(ABORT,'invalid or mutable session compaction contract'); END;
CREATE TRIGGER durable_jobs_compaction_target_insert BEFORE INSERT ON durable_jobs
WHEN (NEW.job_kind='session_compaction' AND (NEW.compaction_target_turn_id IS NULL OR NEW.compaction_target_turn_id<NEW.covered_through_turn_id)) OR (NEW.job_kind!='session_compaction' AND NEW.compaction_target_turn_id IS NOT NULL)
BEGIN SELECT RAISE(ABORT,'invalid session compaction target'); END;
CREATE TRIGGER durable_jobs_compaction_target_update BEFORE UPDATE OF job_kind,covered_through_turn_id,compaction_target_turn_id ON durable_jobs
WHEN (NEW.job_kind='session_compaction' AND (NEW.compaction_target_turn_id IS NULL OR NEW.compaction_target_turn_id<NEW.covered_through_turn_id)) OR (NEW.job_kind!='session_compaction' AND NEW.compaction_target_turn_id IS NOT NULL) OR NEW.compaction_target_turn_id IS NOT OLD.compaction_target_turn_id
BEGIN SELECT RAISE(ABORT,'invalid or mutable session compaction target'); END;
CREATE TRIGGER durable_jobs_formation_purpose_insert BEFORE INSERT ON durable_jobs
WHEN (NEW.job_kind='memory_formation' AND NEW.formation_purpose IS NULL) OR (NEW.job_kind!='memory_formation' AND NEW.formation_purpose IS NOT NULL)
BEGIN SELECT RAISE(ABORT,'invalid durable job formation purpose'); END;
CREATE TRIGGER durable_jobs_formation_purpose_update BEFORE UPDATE OF job_kind,formation_purpose ON durable_jobs
WHEN (NEW.job_kind='memory_formation' AND NEW.formation_purpose IS NULL) OR (NEW.job_kind!='memory_formation' AND NEW.formation_purpose IS NOT NULL) OR NEW.formation_purpose IS NOT OLD.formation_purpose
BEGIN SELECT RAISE(ABORT,'invalid durable job formation purpose'); END;
CREATE TRIGGER durable_jobs_pattern_context_insert BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind='memory_formation' AND NEW.extractor_version='pattern-v1' AND (NEW.formation_purpose!='background_pattern' OR NOT json_valid(NEW.artifact_payload) OR json_type(NEW.artifact_payload)!='object' OR json_extract(NEW.artifact_payload,'$.version')!=1 OR json_type(NEW.artifact_payload,'$.turn_ids')!='array' OR json_array_length(NEW.artifact_payload,'$.turn_ids') NOT BETWEEN 2 AND 8 OR (SELECT COUNT(*) FROM json_each(NEW.artifact_payload))!=2 OR (SELECT COUNT(*) FROM json_each(NEW.artifact_payload,'$.turn_ids') WHERE type!='integer' OR value<=0)!=0 OR (SELECT COUNT(DISTINCT value) FROM json_each(NEW.artifact_payload,'$.turn_ids'))!=json_array_length(NEW.artifact_payload,'$.turn_ids') OR json_extract(NEW.artifact_payload,'$.turn_ids[#-1]')!=NEW.source_turn_id)
BEGIN SELECT RAISE(ABORT,'invalid durable pattern context'); END;
CREATE TRIGGER durable_jobs_pattern_context_update BEFORE UPDATE OF artifact_payload ON durable_jobs
WHEN OLD.job_kind='memory_formation' AND NEW.artifact_payload IS NOT OLD.artifact_payload
BEGIN SELECT RAISE(ABORT,'immutable durable pattern context'); END;
CREATE TRIGGER durable_jobs_assessment_context_insert BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind='memory_formation' AND NEW.extractor_version='assessment-v1' AND (NEW.formation_purpose!='background_pattern' OR NOT json_valid(NEW.artifact_payload) OR json_type(NEW.artifact_payload) IS NOT 'object' OR json_extract(NEW.artifact_payload,'$.version') IS NOT 1 OR json_type(NEW.artifact_payload,'$.turn_ids') IS NOT 'array' OR json_array_length(NEW.artifact_payload,'$.turn_ids') NOT BETWEEN 1 AND 8 OR (SELECT COUNT(*) FROM json_each(NEW.artifact_payload))!=2 OR EXISTS(SELECT 1 FROM json_each(NEW.artifact_payload,'$.turn_ids') WHERE type!='integer' OR value<=0) OR (SELECT COUNT(DISTINCT value) FROM json_each(NEW.artifact_payload,'$.turn_ids'))!=json_array_length(NEW.artifact_payload,'$.turn_ids') OR json_extract(NEW.artifact_payload,'$.turn_ids[#-1]') IS NOT NEW.source_turn_id OR length(CAST(NEW.artifact_payload AS BLOB))>1024 OR EXISTS(SELECT 1 FROM json_each(NEW.artifact_payload,'$.turn_ids') member WHERE NOT EXISTS(SELECT 1 FROM session_turns t WHERE t.id=member.value AND t.canonical_user_id=NEW.canonical_user_id AND t.session_id=NEW.source_session_id AND t.session_generation=NEW.source_session_generation AND t.delivered_at IS NOT NULL AND t.delivery_failed_at IS NULL)))
BEGIN SELECT RAISE(ABORT,'invalid durable assessment context'); END;

CREATE TABLE user_document_chunk_ids (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 document_id TEXT NOT NULL, ordinal INTEGER NOT NULL,
 UNIQUE(document_id,ordinal),
 FOREIGN KEY(document_id,ordinal) REFERENCES user_document_chunks(document_id,ordinal) ON DELETE CASCADE
);
CREATE TRIGGER user_document_chunk_index_insert AFTER INSERT ON user_document_chunks
BEGIN
 INSERT INTO user_document_chunk_ids(document_id,ordinal) VALUES(NEW.document_id,NEW.ordinal);
END;
-- Extracted chunks are immutable; replacement deletes/reinserts with a fresh ID.
CREATE TRIGGER user_document_chunk_index_immutable BEFORE UPDATE ON user_document_chunks
BEGIN SELECT RAISE(ABORT,'immutable document chunk'); END;
CREATE TRIGGER user_document_index_publish AFTER UPDATE OF status,canonical_user_id ON user_documents
WHEN NEW.status IN ('ready','partial')
BEGIN
 INSERT INTO durable_jobs(job_kind,idempotency_key,canonical_user_id,entity_kind,entity_id,operation,available_at,updated_at)
 SELECT 'derived_index','document:publish:'||key.id||':'||NEW.canonical_user_id,NEW.canonical_user_id,'user_document_chunk',key.id,'upsert',strftime('%Y-%m-%dT%H:%M:%f','now')||'000000Z',strftime('%Y-%m-%dT%H:%M:%f','now')||'000000Z' FROM user_document_chunk_ids key WHERE key.document_id=NEW.id
 ON CONFLICT(job_kind,idempotency_key) DO NOTHING;
END;
CREATE TRIGGER user_document_index_delete BEFORE DELETE ON user_documents
WHEN EXISTS(SELECT 1 FROM account_users WHERE canonical_user_id=OLD.canonical_user_id)
BEGIN
 INSERT INTO durable_jobs(job_kind,idempotency_key,canonical_user_id,entity_kind,entity_id,operation,available_at,updated_at)
 SELECT 'derived_index','document:delete:'||key.id||':'||OLD.canonical_user_id,OLD.canonical_user_id,'user_document_chunk',key.id,'delete',strftime('%Y-%m-%dT%H:%M:%f','now')||'000000Z',strftime('%Y-%m-%dT%H:%M:%f','now')||'000000Z' FROM user_document_chunk_ids key WHERE key.document_id=OLD.id
 ON CONFLICT(job_kind,idempotency_key) DO NOTHING;
END;
