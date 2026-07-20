package database

import (
	"context"
	"database/sql"
	"fmt"
)

const phase14DeploymentMemorySQL = `
CREATE TABLE deployment_memory_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	statement TEXT NOT NULL,
	statement_key TEXT NOT NULL,
	evidence TEXT NOT NULL,
	confidence REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
	importance INTEGER NOT NULL CHECK (importance BETWEEN 1 AND 5),
	status TEXT NOT NULL CHECK (status IN ('active', 'superseded', 'deleted')),
	provenance_type TEXT NOT NULL,
	source_authority TEXT NOT NULL,
	claim_key TEXT NOT NULL,
	claim_slot TEXT NOT NULL,
	claim_value TEXT NOT NULL,
	evidence_count INTEGER NOT NULL DEFAULT 1 CHECK (evidence_count > 0),
	supersedes_id INTEGER REFERENCES deployment_memory_entries(id) ON DELETE SET NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE INDEX idx_deployment_memory_entries_status
	ON deployment_memory_entries (status, importance DESC, confidence DESC, id);
CREATE INDEX idx_deployment_memory_entries_claim
	ON deployment_memory_entries (claim_slot, status, claim_key);
CREATE UNIQUE INDEX idx_deployment_memory_entries_active_statement
	ON deployment_memory_entries (statement_key) WHERE status = 'active';
CREATE UNIQUE INDEX idx_deployment_memory_entries_active_claim
	ON deployment_memory_entries (claim_key) WHERE status = 'active';
CREATE UNIQUE INDEX idx_deployment_memory_entries_active_slot
	ON deployment_memory_entries (claim_slot) WHERE status = 'active';

CREATE TABLE deployment_memory_candidates (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	idempotency_key TEXT NOT NULL UNIQUE,
	state TEXT NOT NULL CHECK (state IN ('staged', 'published', 'rejected')),
	statement TEXT NOT NULL,
	statement_key TEXT NOT NULL,
	evidence TEXT NOT NULL,
	confidence REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
	importance INTEGER NOT NULL CHECK (importance BETWEEN 1 AND 5),
	claim_key TEXT NOT NULL,
	claim_slot TEXT NOT NULL,
	claim_value TEXT NOT NULL,
	source_request_id TEXT NOT NULL,
	source_session_id TEXT NOT NULL,
	source_turn_id INTEGER,
	actor_user_id TEXT NOT NULL,
	source_kind TEXT NOT NULL CHECK (source_kind IN ('global_mcp_tool', 'administrator_statement')),
	source_tool_call_id TEXT NOT NULL,
	mcp_server_id TEXT NOT NULL,
	mcp_server_name TEXT NOT NULL,
	mcp_tool_name TEXT NOT NULL,
	mcp_remote_tool_name TEXT NOT NULL,
	mcp_arguments_digest TEXT NOT NULL,
	mcp_result_digest TEXT NOT NULL,
	published_memory_id INTEGER REFERENCES deployment_memory_entries(id) ON DELETE SET NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	published_at TEXT,
	CHECK (
		(source_kind = 'global_mcp_tool' AND source_tool_call_id != '' AND mcp_server_id != '' AND mcp_tool_name != '') OR
		(source_kind = 'administrator_statement' AND source_tool_call_id = '' AND mcp_server_id = '' AND mcp_server_name = '' AND mcp_tool_name = '' AND mcp_remote_tool_name = '' AND mcp_arguments_digest = '' AND mcp_result_digest = '')
	)
);

CREATE INDEX idx_deployment_memory_candidates_request
	ON deployment_memory_candidates (source_request_id, actor_user_id, state);

CREATE TABLE deployment_memory_evidence (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	candidate_id INTEGER NOT NULL REFERENCES deployment_memory_candidates(id) ON DELETE CASCADE,
	memory_id INTEGER REFERENCES deployment_memory_entries(id) ON DELETE CASCADE,
	evidence TEXT NOT NULL,
	confidence_contribution REAL NOT NULL CHECK (confidence_contribution >= 0 AND confidence_contribution <= 1),
	source_kind TEXT NOT NULL CHECK (source_kind IN ('global_mcp_tool', 'administrator_statement')),
	source_tool_call_id TEXT NOT NULL,
	mcp_server_id TEXT NOT NULL,
	mcp_tool_name TEXT NOT NULL,
	mcp_result_digest TEXT NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE(candidate_id, source_kind, source_tool_call_id),
	CHECK (
		(source_kind = 'global_mcp_tool' AND source_tool_call_id != '' AND mcp_server_id != '' AND mcp_tool_name != '') OR
		(source_kind = 'administrator_statement' AND source_tool_call_id = '' AND mcp_server_id = '' AND mcp_tool_name = '' AND mcp_result_digest = '')
	)
);
`

const phase14MigrationDefinition = phase14DeploymentMemorySQL

func applyPhase14Migration(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, phase14DeploymentMemorySQL); err != nil {
		return fmt.Errorf("add deployment memory schema: %w", err)
	}
	return nil
}
