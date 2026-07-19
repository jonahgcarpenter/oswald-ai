package database

import "fmt"

const sessionCompactionMigration = "session_compaction_v2"

func (d *DB) migrateSessionCompactionSchema() error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin session compaction migration: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	if err := ensureColumnTx(tx, "session_turns", "delivered_at", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumnTx(tx, "session_turns", "delivery_failed_at", "TEXT"); err != nil {
		return err
	}
	var applied int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, sessionCompactionMigration).Scan(&applied); err != nil {
		return fmt.Errorf("inspect session compaction migration: %w", err)
	}
	if applied != 0 {
		return tx.Commit()
	}
	if _, err := tx.Exec(`
UPDATE session_turns
SET delivered_at = formation_eligible_at
WHERE delivered_at IS NULL AND formation_eligible_at IS NOT NULL;
UPDATE session_turns
SET delivery_failed_at = created_at
WHERE delivered_at IS NULL AND delivery_failed_at IS NULL;

CREATE TABLE IF NOT EXISTS session_summaries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	session_generation INTEGER NOT NULL CHECK (session_generation > 0),
	covered_from_turn_id INTEGER NOT NULL,
	covered_through_turn_id INTEGER NOT NULL,
	narrative TEXT NOT NULL,
	open_tasks TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(open_tasks) AND json_type(open_tasks) = 'array'),
	commitments TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(commitments) AND json_type(commitments) = 'array'),
	entities TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(entities) AND json_type(entities) = 'array'),
	decisions TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(decisions) AND json_type(decisions) = 'array'),
	topic_tags TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(topic_tags) AND json_type(topic_tags) = 'array'),
	generation_model TEXT NOT NULL,
	generator_version TEXT NOT NULL,
	source_digest TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT,
	CHECK (covered_from_turn_id <= covered_through_turn_id),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	UNIQUE (canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id),
	UNIQUE (canonical_user_id, id)
);

CREATE INDEX IF NOT EXISTS idx_session_summaries_context
ON session_summaries (canonical_user_id, session_id, session_generation, covered_through_turn_id DESC);

CREATE INDEX IF NOT EXISTS idx_session_summaries_expiry
ON session_summaries (expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS session_summary_sources (
	summary_id INTEGER NOT NULL,
	canonical_user_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	session_generation INTEGER NOT NULL,
	turn_id INTEGER NOT NULL,
	ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
	PRIMARY KEY (summary_id, turn_id),
	UNIQUE (summary_id, ordinal),
	FOREIGN KEY (canonical_user_id, summary_id) REFERENCES session_summaries(canonical_user_id, id) ON DELETE CASCADE,
	FOREIGN KEY (turn_id) REFERENCES session_turns(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_session_summary_sources_turn
ON session_summary_sources (canonical_user_id, session_id, session_generation, turn_id);

CREATE TABLE IF NOT EXISTS session_compaction_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	session_generation INTEGER NOT NULL CHECK (session_generation > 0),
	covered_from_turn_id INTEGER NOT NULL,
	covered_through_turn_id INTEGER NOT NULL,
	state TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'retry', 'succeeded', 'skipped', 'dead')),
	artifact_payload TEXT NOT NULL DEFAULT '',
	artifact_summary_id INTEGER,
	generation_model TEXT NOT NULL DEFAULT '',
	generator_version TEXT NOT NULL DEFAULT '',
	attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 3),
	redrive_count INTEGER NOT NULL DEFAULT 0 CHECK (redrive_count BETWEEN 0 AND 3),
	available_at TEXT NOT NULL,
	lease_owner TEXT NOT NULL DEFAULT '',
	lease_until TEXT,
	started_at TEXT,
	completed_at TEXT,
	last_error_code TEXT NOT NULL DEFAULT '',
	last_error_message TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	CHECK (covered_from_turn_id <= covered_through_turn_id),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (canonical_user_id, artifact_summary_id) REFERENCES session_summaries(canonical_user_id, id),
	UNIQUE (canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id)
);

CREATE INDEX IF NOT EXISTS idx_session_compaction_jobs_ready
ON session_compaction_jobs (state, available_at, id);

CREATE INDEX IF NOT EXISTS idx_session_compaction_jobs_tenant
ON session_compaction_jobs (canonical_user_id, session_id, session_generation, state, created_at);

CREATE TRIGGER IF NOT EXISTS session_summaries_range_insert
BEFORE INSERT ON session_summaries
WHEN NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.covered_from_turn_id
		AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.session_id
		AND session_generation = NEW.session_generation
) OR NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.covered_through_turn_id
		AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.session_id
		AND session_generation = NEW.session_generation
)
BEGIN
	SELECT RAISE(ABORT, 'invalid session summary turn range');
END;

CREATE TRIGGER IF NOT EXISTS session_summaries_no_update
BEFORE UPDATE ON session_summaries
BEGIN
	SELECT RAISE(ABORT, 'session summary checkpoints are immutable');
END;

CREATE TRIGGER IF NOT EXISTS session_summary_sources_insert
BEFORE INSERT ON session_summary_sources
WHEN NOT EXISTS (
	SELECT 1
	FROM session_summaries AS summary
	JOIN session_turns AS turn ON turn.id = NEW.turn_id
	WHERE summary.id = NEW.summary_id
		AND summary.canonical_user_id = NEW.canonical_user_id
		AND summary.session_id = NEW.session_id
		AND summary.session_generation = NEW.session_generation
		AND turn.canonical_user_id = summary.canonical_user_id
		AND turn.session_id = summary.session_id
		AND turn.session_generation = summary.session_generation
		AND turn.id BETWEEN summary.covered_from_turn_id AND summary.covered_through_turn_id
)
BEGIN
	SELECT RAISE(ABORT, 'invalid session summary source');
END;

CREATE TRIGGER IF NOT EXISTS session_summary_sources_no_update
BEFORE UPDATE ON session_summary_sources
BEGIN
	SELECT RAISE(ABORT, 'session summary sources are immutable');
END;

CREATE TRIGGER IF NOT EXISTS session_turns_compaction_tenant_update
BEFORE UPDATE OF canonical_user_id, session_id, session_generation ON session_turns
WHEN EXISTS (
	SELECT 1
	FROM session_summary_sources AS source
	WHERE source.turn_id = OLD.id
		AND (source.canonical_user_id != NEW.canonical_user_id
			OR source.session_id != NEW.session_id
			OR source.session_generation != NEW.session_generation)
) OR EXISTS (
	SELECT 1
	FROM session_summaries AS summary
	WHERE OLD.id IN (summary.covered_from_turn_id, summary.covered_through_turn_id)
		AND (summary.canonical_user_id != NEW.canonical_user_id
			OR summary.session_id != NEW.session_id
			OR summary.session_generation != NEW.session_generation)
) OR EXISTS (
	SELECT 1
	FROM session_compaction_jobs AS job
	WHERE OLD.id IN (job.covered_from_turn_id, job.covered_through_turn_id)
		AND (job.canonical_user_id != NEW.canonical_user_id
			OR job.session_id != NEW.session_id
			OR job.session_generation != NEW.session_generation)
)
BEGIN
	SELECT RAISE(ABORT, 'session turn has compaction references');
END;

CREATE TRIGGER IF NOT EXISTS session_turns_summary_content_update
BEFORE UPDATE OF user_text, assistant_text ON session_turns
WHEN EXISTS (SELECT 1 FROM session_summary_sources WHERE turn_id = OLD.id)
BEGIN
	SELECT RAISE(ABORT, 'session summary source content is immutable');
END;

CREATE TRIGGER IF NOT EXISTS session_turns_summary_delete
BEFORE DELETE ON session_turns
WHEN EXISTS (SELECT 1 FROM session_summary_sources WHERE turn_id = OLD.id)
BEGIN
	SELECT RAISE(ABORT, 'delete session summaries before source turns');
END;

CREATE TRIGGER IF NOT EXISTS session_compaction_jobs_range_insert
BEFORE INSERT ON session_compaction_jobs
WHEN NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.covered_from_turn_id
		AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.session_id
		AND session_generation = NEW.session_generation
) OR NOT EXISTS (
	SELECT 1 FROM session_turns
	WHERE id = NEW.covered_through_turn_id
		AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.session_id
		AND session_generation = NEW.session_generation
)
BEGIN
	SELECT RAISE(ABORT, 'invalid session compaction job turn range');
END;

CREATE TRIGGER IF NOT EXISTS session_compaction_jobs_range_update
BEFORE UPDATE OF canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id ON session_compaction_jobs
WHEN NEW.canonical_user_id != OLD.canonical_user_id
	OR NEW.session_id != OLD.session_id
	OR NEW.session_generation != OLD.session_generation
	OR NEW.covered_from_turn_id != OLD.covered_from_turn_id
	OR NEW.covered_through_turn_id != OLD.covered_through_turn_id
BEGIN
	SELECT RAISE(ABORT, 'session compaction job range is immutable');
END;

CREATE TRIGGER IF NOT EXISTS session_compaction_jobs_artifact_insert
BEFORE INSERT ON session_compaction_jobs
WHEN NEW.artifact_summary_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_summaries
	WHERE id = NEW.artifact_summary_id
		AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.session_id
		AND session_generation = NEW.session_generation
		AND covered_from_turn_id = NEW.covered_from_turn_id
		AND covered_through_turn_id = NEW.covered_through_turn_id
)
BEGIN
	SELECT RAISE(ABORT, 'invalid session compaction artifact');
END;

CREATE TRIGGER IF NOT EXISTS session_compaction_jobs_artifact_update
BEFORE UPDATE OF artifact_summary_id ON session_compaction_jobs
WHEN NEW.artifact_summary_id IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM session_summaries
	WHERE id = NEW.artifact_summary_id
		AND canonical_user_id = NEW.canonical_user_id
		AND session_id = NEW.session_id
		AND session_generation = NEW.session_generation
		AND covered_from_turn_id = NEW.covered_from_turn_id
		AND covered_through_turn_id = NEW.covered_through_turn_id
)
BEGIN
	SELECT RAISE(ABORT, 'invalid session compaction artifact');
END;
`); err != nil {
		return fmt.Errorf("create session compaction schema: %w", err)
	}
	if _, err := tx.Exec(`
INSERT INTO schema_migrations (name, applied_at)
VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
ON CONFLICT(name) DO NOTHING`, sessionCompactionMigration); err != nil {
		return fmt.Errorf("record session compaction migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session compaction migration: %w", err)
	}
	return nil
}
