package database

import "fmt"

func (d *DB) initializeMCPServers() error {
	_, err := d.db.Exec(mcpServersBaselineSQL)
	if err != nil {
		return fmt.Errorf("failed to initialize mcp_servers table: %w", err)
	}
	return nil
}

const mcpServersBaselineSQL = `
CREATE TABLE IF NOT EXISTS mcp_servers (
	id TEXT PRIMARY KEY,
	scope TEXT NOT NULL CHECK (scope IN ('global', 'user')),
	owner_user_id TEXT,
	name TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT 'generic',
	transport TEXT NOT NULL CHECK (transport IN ('streamable_http', 'sse')),
	url_ciphertext TEXT NOT NULL,
	url_host_hash TEXT NOT NULL DEFAULT '',
	headers_ciphertext TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	CHECK (
		(scope = 'global' AND owner_user_id IS NULL) OR
		(scope = 'user' AND owner_user_id IS NOT NULL)
	)
);

CREATE UNIQUE INDEX IF NOT EXISTS mcp_servers_global_name_unique
ON mcp_servers(name)
WHERE scope = 'global';

CREATE UNIQUE INDEX IF NOT EXISTS mcp_servers_user_name_unique
ON mcp_servers(owner_user_id, name)
WHERE scope = 'user';
`
