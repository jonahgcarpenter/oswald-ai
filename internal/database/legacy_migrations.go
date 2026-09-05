package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

//go:embed legacy_migrations/*.sql
var legacyMigrationFiles embed.FS

const legacyV320TargetMarker = "-- PERMANENT-MIGRATION-v4.0.0"

var legacyV320VectorSQL = regexp.MustCompile(`^CREATEVIRTUALTABLEmemory_entry_vectorsUSINGvec0\(embeddingfloat\[([1-9][0-9]*)\]\)$`)

func migrateLegacyV320(ctx context.Context, conn *sql.Conn, target schemaMigration) (bool, error) {
	sourceSQL, err := legacyMigrationFiles.ReadFile("legacy_migrations/v3.2.0_schema.sql")
	if err != nil {
		return false, fmt.Errorf("read v3.2.0 source schema: %w", err)
	}
	actual, err := schemaFingerprint(ctx, conn)
	if err != nil {
		return false, fmt.Errorf("fingerprint existing database schema: %w", err)
	}
	expected, err := definitionFingerprint(string(sourceSQL))
	if err != nil {
		return false, err
	}
	if actual != expected {
		vectorDefinition, ok, err := legacyV320VectorDefinition(ctx, conn)
		if err != nil || !ok {
			return false, err
		}
		expected, err = definitionFingerprint(string(sourceSQL) + "\n" + vectorDefinition)
		if err != nil {
			return false, err
		}
		if actual != expected {
			return false, nil
		}
	}
	if err := validateLegacyV320Ownership(ctx, conn); err != nil {
		return false, err
	}
	if err := validateLegacyV320WebsocketOwners(ctx, conn); err != nil {
		return false, err
	}

	conversion, err := legacyMigrationFiles.ReadFile("legacy_migrations/v3.2.0_to_v4.0.0.sql")
	if err != nil {
		return false, fmt.Errorf("read v3.2.0 conversion: %w", err)
	}
	if strings.Count(string(conversion), legacyV320TargetMarker) != 1 {
		return false, fmt.Errorf("v3.2.0 conversion must contain exactly one %q marker", legacyV320TargetMarker)
	}
	conversionSQL := strings.Replace(string(conversion), legacyV320TargetMarker, target.sql, 1)
	if _, err := conn.ExecContext(ctx, conversionSQL); err != nil {
		return false, fmt.Errorf("convert v3.2.0 database to v4.0.0: %w", err)
	}
	if err := rebuildLegacyV320SpeakerIntros(ctx, conn); err != nil {
		return false, err
	}
	return true, nil
}

func validateLegacyV320WebsocketOwners(ctx context.Context, conn *sql.Conn) error {
	var inaccessible int
	if err := conn.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM account_users user
WHERE EXISTS (
	SELECT 1 FROM linked_accounts link
	WHERE link.canonical_user_id = user.canonical_user_id AND link.gateway = 'websocket'
)
AND NOT EXISTS (
	SELECT 1 FROM linked_accounts link
	WHERE link.canonical_user_id = user.canonical_user_id AND link.gateway != 'websocket'
)
AND (
	user.is_admin != 0
	OR EXISTS (
		SELECT 1 FROM mcp_servers server
		WHERE server.scope = 'user' AND server.owner_user_id = user.canonical_user_id
	)
)`).Scan(&inaccessible); err != nil {
		return fmt.Errorf("validate v3.2.0 websocket ownership: %w", err)
	}
	if inaccessible != 0 {
		return fmt.Errorf("v3.2.0 websocket-only privileged ownership validation failed: found %d inaccessible administrator or MCP owner(s)", inaccessible)
	}
	return nil
}

func rebuildLegacyV320SpeakerIntros(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `
SELECT user.canonical_user_id, account.gateway, account.display_name
FROM account_users user
LEFT JOIN linked_accounts account ON account.canonical_user_id = user.canonical_user_id
ORDER BY user.canonical_user_id, account.gateway, account.identifier`)
	if err != nil {
		return fmt.Errorf("read migrated speaker identities: %w", err)
	}
	namesByUser := make(map[string]map[string]string)
	for rows.Next() {
		var userID string
		var gateway, displayName sql.NullString
		if err := rows.Scan(&userID, &gateway, &displayName); err != nil {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("scan migrated speaker identity: %w", err)
		}
		if _, ok := namesByUser[userID]; !ok {
			namesByUser[userID] = make(map[string]string)
		}
		name := strings.TrimSpace(displayName.String)
		if gateway.Valid && name != "" && namesByUser[userID][gateway.String] == "" {
			namesByUser[userID][gateway.String] = name
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close() // nolint:errcheck
		return fmt.Errorf("read migrated speaker identities: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close migrated speaker identities: %w", err)
	}
	for userID, names := range namesByUser {
		intro := legacyV320SpeakerIntro(names)
		if _, err := conn.ExecContext(ctx, `UPDATE account_users SET speaker_intro = ? WHERE canonical_user_id = ?`, intro, userID); err != nil {
			return fmt.Errorf("rebuild migrated speaker intro: %w", err)
		}
	}
	return nil
}

func legacyV320SpeakerIntro(names map[string]string) string {
	imessageName := names["imessage"]
	discordName := names["discord"]
	homeAssistantName := names["homeassistant"]
	switch {
	case imessageName != "" && discordName != "":
		if strings.EqualFold(imessageName, discordName) {
			return fmt.Sprintf("You are speaking with %s.", imessageName)
		}
		return fmt.Sprintf("You are speaking with %s aka %s.", imessageName, discordName)
	case imessageName != "":
		return fmt.Sprintf("You are speaking with %s.", imessageName)
	case discordName != "":
		return fmt.Sprintf("You are speaking with %s.", discordName)
	case homeAssistantName != "":
		return fmt.Sprintf("You are speaking with %s.", homeAssistantName)
	default:
		return "You are speaking with a returning user."
	}
}

func legacyV320VectorDefinition(ctx context.Context, conn *sql.Conn) (string, bool, error) {
	var definition string
	err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'memory_entry_vectors'`).Scan(&definition)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read v3.2.0 vector definition: %w", err)
	}
	matches := legacyV320VectorSQL.FindStringSubmatch(canonicalSchemaSQL(definition))
	if len(matches) != 2 {
		return "", false, nil
	}
	dimension, err := strconv.Atoi(matches[1])
	if err != nil || dimension <= 0 {
		return "", false, nil
	}
	return fmt.Sprintf(`CREATE VIRTUAL TABLE memory_entry_vectors USING vec0(embedding float[%d]);`, dimension), true, nil
}

func validateLegacyV320Ownership(ctx context.Context, conn *sql.Conn) error {
	var invalid int
	if err := conn.QueryRowContext(ctx, `
SELECT
	(SELECT COUNT(*) FROM linked_accounts link LEFT JOIN account_users user ON user.canonical_user_id = link.canonical_user_id WHERE user.canonical_user_id IS NULL)
	+ (SELECT COUNT(*) FROM mcp_servers server LEFT JOIN account_users user ON user.canonical_user_id = server.owner_user_id
		WHERE (server.scope = 'user' AND user.canonical_user_id IS NULL) OR (server.scope = 'global' AND server.owner_user_id IS NOT NULL))`).Scan(&invalid); err != nil {
		return fmt.Errorf("validate v3.2.0 ownership: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("v3.2.0 ownership validation failed: found %d invalid linked-account or MCP owners", invalid)
	}
	return nil
}

func definitionFingerprint(definition string) (string, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return "", fmt.Errorf("open schema fingerprint database: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(definition); err != nil {
		return "", fmt.Errorf("execute schema fingerprint definition: %w", err)
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
