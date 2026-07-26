package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// These definitions are copied from the published v3.2.0 database bootstrap.
// SQLite preserves the v3.1.2 memory category constraint when v3.2.0 opens an
// existing database, producing the only other accepted source schema.
const publishedV32SchemaDefinition = `
CREATE TABLE IF NOT EXISTS account_users (
	canonical_user_id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	is_admin INTEGER NOT NULL DEFAULT 0,
	is_banned INTEGER NOT NULL DEFAULT 0,
	banned_at TEXT NOT NULL DEFAULT '',
	banned_by TEXT NOT NULL DEFAULT '',
	ban_reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS linked_accounts (
	gateway TEXT NOT NULL,
	identifier TEXT NOT NULL,
	canonical_user_id TEXT NOT NULL,
	display_name TEXT NOT NULL DEFAULT '',
	linked_at TEXT NOT NULL,
	verified INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (gateway, identifier),
	UNIQUE (canonical_user_id, gateway),
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS user_memory_profiles (
	canonical_user_id TEXT PRIMARY KEY,
	intro TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS memory_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_user_id TEXT NOT NULL,
	scope TEXT NOT NULL CHECK (scope IN ('short_term', 'long_term')),
	category TEXT NOT NULL CHECK (category IN ('identity', 'system_rules', 'communication_preferences', 'durable_preferences', 'projects', 'relationships', 'environment', 'notes')),
	statement TEXT NOT NULL,
	statement_key TEXT NOT NULL,
	evidence TEXT NOT NULL,
	confidence REAL NOT NULL DEFAULT 0.8,
	importance INTEGER NOT NULL DEFAULT 3,
	status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'expired', 'superseded', 'deleted')),
	source_session_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	last_used_at TEXT,
	expires_at TEXT,
	supersedes_id INTEGER,
	embedding_model TEXT NOT NULL DEFAULT '',
	embedding_dim INTEGER NOT NULL DEFAULT 0,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE,
	FOREIGN KEY (supersedes_id) REFERENCES memory_entries(id) ON DELETE SET NULL,
	UNIQUE (canonical_user_id, scope, statement_key)
);
CREATE INDEX IF NOT EXISTS idx_memory_entries_user_scope_category ON memory_entries (canonical_user_id, scope, category, status);
CREATE INDEX IF NOT EXISTS idx_memory_entries_user_updated ON memory_entries (canonical_user_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_memory_entries_expiry ON memory_entries (expires_at, status);
CREATE TABLE IF NOT EXISTS session_turns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	canonical_user_id TEXT NOT NULL,
	user_text TEXT NOT NULL,
	assistant_text TEXT NOT NULL,
	tool_names TEXT NOT NULL DEFAULT '',
	importance INTEGER NOT NULL DEFAULT 2,
	topic_tags TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	expires_at TEXT,
	FOREIGN KEY (canonical_user_id) REFERENCES account_users(canonical_user_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_session_turns_session_created ON session_turns (session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_session_turns_user_created ON session_turns (canonical_user_id, created_at);
CREATE TABLE IF NOT EXISTS memory_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	memory_id INTEGER,
	event_type TEXT NOT NULL,
	request_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	metadata TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (memory_id) REFERENCES memory_entries(id) ON DELETE SET NULL
);
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
	CHECK ((scope = 'global' AND owner_user_id IS NULL) OR (scope = 'user' AND owner_user_id IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS mcp_servers_global_name_unique ON mcp_servers(name) WHERE scope = 'global';
CREATE UNIQUE INDEX IF NOT EXISTS mcp_servers_user_name_unique ON mcp_servers(owner_user_id, name) WHERE scope = 'user';
`

const publishedV312UpgradedSchemaDefinition = `
The v3.1.2-upgraded schema is identical to publishedV32SchemaDefinition except
memory_entries.category also permits 'tasks'. Runtime construction performs
that one frozen substitution before computing the complete schema fingerprint.
`

const (
	publishedV3StructuralFingerprintContractVersion = "published-v3-structural-fingerprint-v1"
	publishedV3SpeakerIntroRenderingContractVersion = "published-v3-speaker-intro-v1"
	publishedV3SpeakerIntroFallback                 = "You are speaking with a returning user."
	publishedV3SpeakerIntroSingleFormat             = "You are speaking with %s."
	publishedV3SpeakerIntroAliasFormat              = "You are speaking with %s aka %s."
	publishedV3VectorSQLPattern                     = `^CREATEVIRTUALTABLEmemory_entry_vectorsUSINGvec0\(embeddingfloat\[([1-9][0-9]*)\]\)$`
)

const publishedV3StructuralFingerprintContract = `
version: ` + publishedV3StructuralFingerprintContractVersion + `
inventory: sqlite_master non-sqlite objects ordered by type and name
normalization: remove ASCII SQL whitespace outside SQLite quotes
vector definition pattern: ` + publishedV3VectorSQLPattern + `
`

const publishedV3SpeakerIntroRenderingContract = `
version: ` + publishedV3SpeakerIntroRenderingContractVersion + `
gateway priority: imessage, discord, websocket
equal imessage/discord names: single name, case-insensitive
fallback: ` + publishedV3SpeakerIntroFallback + `
single-name format: ` + publishedV3SpeakerIntroSingleFormat + `
alias format: ` + publishedV3SpeakerIntroAliasFormat + `
`

const publishedV3StageAndDropSQL = `
CREATE TEMP TABLE published_v3_account_users AS SELECT * FROM account_users;
CREATE TEMP TABLE published_v3_linked_accounts AS SELECT * FROM linked_accounts;
CREATE TEMP TABLE published_v3_mcp_servers AS SELECT * FROM mcp_servers;
DROP TABLE memory_events;
DROP TABLE session_turns;
DROP TABLE memory_entries;
DROP TABLE user_memory_profiles;
DROP TABLE mcp_servers;
DROP TABLE linked_accounts;
DROP TABLE account_users;`

const publishedV3VectorDropSQL = `
DROP TABLE memory_entry_vectors;
DROP TABLE IF EXISTS memory_entry_vectors_vector_chunks00;
DROP TABLE IF EXISTS memory_entry_vectors_rowids;
DROP TABLE IF EXISTS memory_entry_vectors_info;
DROP TABLE IF EXISTS memory_entry_vectors_chunks;`

const publishedV3RestoreSQL = `
INSERT INTO account_users (
	canonical_user_id, created_at, updated_at, is_admin, is_banned, banned_at, banned_by, ban_reason, lifecycle_state
)
SELECT canonical_user_id, created_at, updated_at, is_admin, is_banned, banned_at, banned_by, ban_reason, 'active'
FROM published_v3_account_users;
INSERT INTO linked_accounts (gateway, identifier, canonical_user_id, display_name, linked_at, verified)
SELECT gateway, identifier, canonical_user_id, display_name, linked_at, verified FROM published_v3_linked_accounts;
INSERT INTO mcp_servers (id, scope, owner_user_id, name, type, transport, url_ciphertext, url_host_hash, headers_ciphertext, enabled, created_at, updated_at)
SELECT id, scope, owner_user_id, name, type, transport, url_ciphertext, url_host_hash, headers_ciphertext, enabled, created_at, updated_at
FROM published_v3_mcp_servers;
CREATE TABLE schema_migration_versions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	applied_at TEXT NOT NULL
);`

const publishedV3DropStagingSQL = `
DROP TABLE published_v3_mcp_servers;
DROP TABLE published_v3_linked_accounts;
DROP TABLE published_v3_account_users;`

// Keep the reset source contract in the baseline checksum. This deliberately
// makes pre-release experimental v4 ledgers fail closed after the reset ships.
const compactV4MigrationDefinition = compactV4BaselineDefinition + `

-- selective published-v3 reset source
` + publishedV32SchemaDefinition + publishedV312UpgradedSchemaDefinition + publishedV3ImportDefinition + `
structural fingerprint contract:
` + publishedV3StructuralFingerprintContract + `
speaker intro rendering contract:
` + publishedV3SpeakerIntroRenderingContract + `

-- executable selective-reset SQL
` + publishedV3StageAndDropSQL + publishedV3VectorDropSQL + publishedV3RestoreSQL + publishedV3DropStagingSQL

const publishedV3ImportDefinition = `
Preserve every account_users column from v3, every linked_accounts column, and
every mcp_servers column. Require linked and user-scoped MCP owners to exist.
Replace every main-schema object with compactV4BaselineDefinition, set all
preserved users active, and derive speaker_intro from linked display names in
imessage, discord, websocket priority. Record only v4_compact_baseline.
`

func resetPublishedV3(ctx context.Context, conn *sql.Conn, registry []schemaMigration) (bool, error) {
	actual, err := schemaFingerprint(ctx, conn)
	if err != nil {
		return false, fmt.Errorf("fingerprint existing database schema: %w", err)
	}
	freshFingerprint, err := definitionFingerprint(publishedV32SchemaDefinition)
	if err != nil {
		return false, err
	}
	upgradedDefinition := strings.Replace(publishedV32SchemaDefinition,
		"'environment', 'notes'", "'environment', 'tasks', 'notes'", 1)
	upgradedFingerprint, err := definitionFingerprint(upgradedDefinition)
	if err != nil {
		return false, err
	}
	hasVector := false
	if actual != freshFingerprint && actual != upgradedFingerprint {
		vectorDefinition, ok, err := publishedV3VectorDefinition(ctx, conn)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		freshVectorFingerprint, err := definitionFingerprint(publishedV32SchemaDefinition + vectorDefinition)
		if err != nil {
			return false, err
		}
		upgradedVectorFingerprint, err := definitionFingerprint(upgradedDefinition + vectorDefinition)
		if err != nil {
			return false, err
		}
		if actual != freshVectorFingerprint && actual != upgradedVectorFingerprint {
			return false, nil
		}
		hasVector = true
	}
	if len(registry) != 1 || registry[0].version != 1 || registry[0].name != "v4_compact_baseline" {
		return false, fmt.Errorf("published v3 reset requires the compact v4 baseline registry")
	}
	if err := validatePublishedV3Ownership(ctx, conn); err != nil {
		return false, err
	}

	counts := make(map[string]int, 3)
	for _, table := range []string{"account_users", "linked_accounts", "mcp_servers"} {
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			return false, fmt.Errorf("count published v3 %s: %w", table, err)
		}
		counts[table] = count
	}
	if hasVector {
		if _, err := conn.ExecContext(ctx, publishedV3VectorDropSQL); err != nil {
			return false, fmt.Errorf("drop published v3 vector objects: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, publishedV3StageAndDropSQL); err != nil {
		return false, fmt.Errorf("stage published v3 preserved data: %w", err)
	}
	if err := applyCompactV4Baseline(ctx, conn); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, publishedV3RestoreSQL); err != nil {
		return false, fmt.Errorf("restore published v3 preserved data: %w", err)
	}
	if err := rebuildPublishedV3SpeakerIntros(ctx, conn); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migration_versions(version, name, checksum, applied_at) VALUES (1, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`, registry[0].name, migrationChecksum(registry[0])); err != nil {
		return false, fmt.Errorf("record compact v4 baseline after published v3 reset: %w", err)
	}
	if _, err := conn.ExecContext(ctx, publishedV3DropStagingSQL); err != nil {
		return false, fmt.Errorf("clear published v3 staging data: %w", err)
	}
	if err := validatePublishedV3Reset(ctx, conn, counts); err != nil {
		return false, err
	}
	return true, nil
}

var publishedV3VectorSQL = regexp.MustCompile(publishedV3VectorSQLPattern)

func publishedV3VectorDefinition(ctx context.Context, conn *sql.Conn) (string, bool, error) {
	var definition string
	err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'memory_entry_vectors'`).Scan(&definition)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read published v3 vector definition: %w", err)
	}
	matches := publishedV3VectorSQL.FindStringSubmatch(canonicalSchemaSQL(definition))
	if len(matches) != 2 {
		return "", false, nil
	}
	dimension, err := strconv.Atoi(matches[1])
	if err != nil || dimension <= 0 {
		return "", false, nil
	}
	return fmt.Sprintf(`CREATE VIRTUAL TABLE memory_entry_vectors USING vec0(embedding float[%d]);`, dimension), true, nil
}

func validatePublishedV3Ownership(ctx context.Context, conn *sql.Conn) error {
	var invalid int
	if err := conn.QueryRowContext(ctx, `
SELECT
	(SELECT COUNT(*) FROM linked_accounts link LEFT JOIN account_users user ON user.canonical_user_id = link.canonical_user_id WHERE user.canonical_user_id IS NULL)
	+ (SELECT COUNT(*) FROM mcp_servers server LEFT JOIN account_users user ON user.canonical_user_id = server.owner_user_id
		WHERE (server.scope = 'user' AND user.canonical_user_id IS NULL) OR (server.scope = 'global' AND server.owner_user_id IS NOT NULL))`).Scan(&invalid); err != nil {
		return fmt.Errorf("validate published v3 ownership: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("published v3 ownership validation failed: found %d invalid linked-account or MCP owners", invalid)
	}
	return nil
}

func rebuildPublishedV3SpeakerIntros(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `SELECT canonical_user_id FROM account_users ORDER BY canonical_user_id`)
	if err != nil {
		return fmt.Errorf("read preserved users for speaker intros: %w", err)
	}
	var userIDs []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("scan preserved user for speaker intro: %w", err)
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close preserved users for speaker intros: %w", err)
	}
	for _, userID := range userIDs {
		accountRows, err := conn.QueryContext(ctx, `SELECT gateway, display_name FROM linked_accounts WHERE canonical_user_id = ? ORDER BY gateway, identifier`, userID)
		if err != nil {
			return fmt.Errorf("read links for speaker intro: %w", err)
		}
		names := make(map[string]string, 3)
		for accountRows.Next() {
			var gateway, name string
			if err := accountRows.Scan(&gateway, &name); err != nil {
				accountRows.Close() // nolint:errcheck
				return fmt.Errorf("scan link for speaker intro: %w", err)
			}
			name = strings.TrimSpace(name)
			if name != "" && names[gateway] == "" {
				names[gateway] = name
			}
		}
		if err := accountRows.Close(); err != nil {
			return fmt.Errorf("close links for speaker intro: %w", err)
		}
		intro := publishedV3SpeakerIntroFallback
		switch {
		case names["imessage"] != "" && names["discord"] != "" && strings.EqualFold(names["imessage"], names["discord"]):
			intro = fmt.Sprintf(publishedV3SpeakerIntroSingleFormat, names["imessage"])
		case names["imessage"] != "" && names["discord"] != "":
			intro = fmt.Sprintf(publishedV3SpeakerIntroAliasFormat, names["imessage"], names["discord"])
		case names["imessage"] != "":
			intro = fmt.Sprintf(publishedV3SpeakerIntroSingleFormat, names["imessage"])
		case names["discord"] != "":
			intro = fmt.Sprintf(publishedV3SpeakerIntroSingleFormat, names["discord"])
		case names["websocket"] != "":
			intro = fmt.Sprintf(publishedV3SpeakerIntroSingleFormat, names["websocket"])
		}
		if _, err := conn.ExecContext(ctx, `UPDATE account_users SET speaker_intro = ? WHERE canonical_user_id = ?`, intro, userID); err != nil {
			return fmt.Errorf("rebuild speaker intro: %w", err)
		}
	}
	return nil
}

func validatePublishedV3Reset(ctx context.Context, conn *sql.Conn, expected map[string]int) error {
	for _, table := range []string{"account_users", "linked_accounts", "mcp_servers"} {
		var actual int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&actual); err != nil {
			return fmt.Errorf("validate reset %s count: %w", table, err)
		}
		if actual != expected[table] {
			return fmt.Errorf("published v3 reset changed %s row count: got %d, want %d", table, actual, expected[table])
		}
	}
	if err := validatePublishedV3Ownership(ctx, conn); err != nil {
		return err
	}
	actual, err := schemaFingerprint(ctx, conn)
	if err != nil {
		return fmt.Errorf("fingerprint reset schema: %w", err)
	}
	expectedFingerprint, err := definitionFingerprint(compactV4BaselineDefinition + `
CREATE TABLE schema_migration_versions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	applied_at TEXT NOT NULL
);`)
	if err != nil {
		return err
	}
	if actual != expectedFingerprint {
		return fmt.Errorf("published v3 reset produced an unexpected compact v4 schema inventory")
	}
	return foreignKeyCheck(ctx, conn)
}

func definitionFingerprint(definition string) (string, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return "", fmt.Errorf("open schema fingerprint database: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(definition); err != nil {
		return "", fmt.Errorf("execute frozen schema fingerprint definition: %w", err)
	}
	return schemaFingerprint(context.Background(), db)
}

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func schemaFingerprint(ctx context.Context, queryer schemaQueryer) (string, error) {
	rows, err := queryer.QueryContext(ctx, `
SELECT type, name, tbl_name, sql
FROM sqlite_master
WHERE name NOT LIKE 'sqlite_%'
ORDER BY type, name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hash := sha256.New()
	for rows.Next() {
		var objectType, name, tableName string
		var definition sql.NullString
		if err := rows.Scan(&objectType, &name, &tableName, &definition); err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%s\n", objectType, name, tableName, canonicalSchemaSQL(definition.String))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func canonicalSchemaSQL(definition string) string {
	var result strings.Builder
	result.Grow(len(definition))
	var quote rune
	for _, r := range definition {
		if quote == 0 {
			if r == '\'' || r == '"' || r == '`' || r == '[' {
				quote = r
				result.WriteRune(r)
			} else if !strings.ContainsRune(" \t\r\n", r) {
				result.WriteRune(r)
			}
			continue
		}
		result.WriteRune(r)
		if (quote == '[' && r == ']') || (quote != '[' && r == quote) {
			quote = 0
		}
	}
	return result.String()
}
