package database

import (
	"context"
	"database/sql"
	"fmt"
)

const (
	baselineV1AccountColumnOperations = "ensure-column-if-missing:account_users.is_admin INTEGER NOT NULL DEFAULT 0\nensure-column-if-missing:account_users.is_banned INTEGER NOT NULL DEFAULT 0\nensure-column-if-missing:account_users.banned_at TEXT NOT NULL DEFAULT ''\nensure-column-if-missing:account_users.banned_by TEXT NOT NULL DEFAULT ''\nensure-column-if-missing:account_users.ban_reason TEXT NOT NULL DEFAULT ''\n"
	baselineV1MemoryColumnOperations  = "ensure-column-if-missing:memory_entries.profile_approved INTEGER NOT NULL DEFAULT 0 CHECK (profile_approved IN (0, 1))\nensure-column-if-missing:session_turns.session_generation INTEGER NOT NULL DEFAULT 1\n"
	baselineV1ProfileIndexSQL         = `CREATE INDEX IF NOT EXISTS idx_memory_entries_profile_candidates ON memory_entries (canonical_user_id, profile_approved, status, scope, category, expires_at);`
	baselineV1Definition              = accountUsersBaselineSQL + baselineV1AccountColumnOperations + linkedAccountsBaselineSQL + accountLinkChallengesBaselineSQL + userMemoryBaselineSQL + baselineV1MemoryColumnOperations + userMemoryCleanupIndexesSQL + baselineV1ProfileIndexSQL + mcpServersBaselineSQL

	baselineV2CategoryTransform = "data-transform:UPDATE memory_entries SET category = 'communication_preferences' WHERE category = 'system_rules';\n"
	baselineV2ApprovalTransform = "data-transform:UPDATE memory_entries SET profile_approved = 1;\n"
	baselineV2Definition        = baselineV2CategoryTransform + baselineV2ApprovalTransform + "legacy-ledger:stable_tenant_profiles_v1\n"

	baselineV3ColumnOperations = "ensure-column-if-missing:memory_entries.candidate_id INTEGER REFERENCES memory_candidates(id) ON DELETE SET NULL\nensure-column-if-missing:memory_entries.provenance_type TEXT NOT NULL DEFAULT 'legacy_import'\nensure-column-if-missing:memory_entries.source_authority TEXT NOT NULL DEFAULT 'unknown'\nensure-column-if-missing:memory_entries.source_request_id TEXT NOT NULL DEFAULT ''\nensure-column-if-missing:memory_entries.source_turn_id INTEGER REFERENCES session_turns(id) ON DELETE SET NULL\nensure-column-if-missing:memory_entries.formation_mode TEXT NOT NULL DEFAULT 'legacy_import'\nensure-column-if-missing:memory_entries.sensitivity TEXT NOT NULL DEFAULT 'unknown'\nensure-column-if-missing:memory_entries.approval_state TEXT NOT NULL DEFAULT 'approved' CHECK (approval_state IN ('proposed', 'pending_confirmation', 'approved', 'rejected'))\nensure-column-if-missing:memory_entries.approved_at TEXT NOT NULL DEFAULT ''\nensure-column-if-missing:memory_entries.approved_by TEXT NOT NULL DEFAULT ''\nensure-column-if-missing:memory_entries.valid_from TEXT NOT NULL DEFAULT ''\nensure-column-if-missing:memory_entries.valid_until TEXT\nensure-column-if-missing:memory_entries.invalidated_at TEXT\nensure-column-if-missing:memory_entries.invalidation_reason TEXT NOT NULL DEFAULT ''\nensure-column-if-missing:memory_entries.erased_at TEXT\nensure-column-if-missing:memory_entries.erasure_reason TEXT NOT NULL DEFAULT ''\nensure-column-if-missing:memory_entries.erasure_request_id TEXT NOT NULL DEFAULT ''\nensure-column-if-missing:session_turns.source_request_id TEXT NOT NULL DEFAULT ''\nensure-column-if-missing:session_turns.formation_eligible_at TEXT\n"
	baselineV3Definition       = baselineV3ColumnOperations + memoryFormationSchemaSQL + memoryFormationBackfillSQL + "legacy-ledger:" + memoryFormationMigration + "\n"
	baselineV4ColumnOperations = "ensure-column-if-missing:session_turns.delivered_at TEXT\nensure-column-if-missing:session_turns.delivery_failed_at TEXT\n"
	baselineV4Definition       = baselineV4ColumnOperations + sessionCompactionSchemaSQL + "legacy-ledger:" + sessionCompactionMigration + "\n"
	baselineV5Definition       = "optional-capability:fts5\n" + memoryFTSSchemaSQL + "legacy-ledger:" + memoryFTSMigration + "\n"
	baselineV6Definition       = "optional-capability:fts5\n" + sessionTurnsFTSSchemaSQL + "legacy-ledger:" + sessionTurnsFTSMigration + "\n"
)

var baselineV3MemoryColumns = []struct{ name, definition string }{
	{"candidate_id", "INTEGER REFERENCES memory_candidates(id) ON DELETE SET NULL"},
	{"provenance_type", "TEXT NOT NULL DEFAULT 'legacy_import'"},
	{"source_authority", "TEXT NOT NULL DEFAULT 'unknown'"},
	{"source_request_id", "TEXT NOT NULL DEFAULT ''"},
	{"source_turn_id", "INTEGER REFERENCES session_turns(id) ON DELETE SET NULL"},
	{"formation_mode", "TEXT NOT NULL DEFAULT 'legacy_import'"},
	{"sensitivity", "TEXT NOT NULL DEFAULT 'unknown'"},
	{"approval_state", "TEXT NOT NULL DEFAULT 'approved' CHECK (approval_state IN ('proposed', 'pending_confirmation', 'approved', 'rejected'))"},
	{"approved_at", "TEXT NOT NULL DEFAULT ''"},
	{"approved_by", "TEXT NOT NULL DEFAULT ''"},
	{"valid_from", "TEXT NOT NULL DEFAULT ''"},
	{"valid_until", "TEXT"},
	{"invalidated_at", "TEXT"},
	{"invalidation_reason", "TEXT NOT NULL DEFAULT ''"},
	{"erased_at", "TEXT"},
	{"erasure_reason", "TEXT NOT NULL DEFAULT ''"},
	{"erasure_request_id", "TEXT NOT NULL DEFAULT ''"},
}

func applyBaselineV1(ctx context.Context, conn *sql.Conn) error {
	for _, statement := range []string{accountUsersBaselineSQL, linkedAccountsBaselineSQL, accountLinkChallengesBaselineSQL, userMemoryBaselineSQL} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply frozen core baseline: %w", err)
		}
	}
	for _, column := range accountUserLegacyColumns {
		if err := ensureColumnConn(ctx, conn, "account_users", column.name, column.definition); err != nil {
			return err
		}
	}
	if err := ensureColumnConn(ctx, conn, "memory_entries", "profile_approved", "INTEGER NOT NULL DEFAULT 0 CHECK (profile_approved IN (0, 1))"); err != nil {
		return err
	}
	if err := ensureColumnConn(ctx, conn, "session_turns", "session_generation", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	for _, statement := range []string{userMemoryCleanupIndexesSQL, baselineV1ProfileIndexSQL, mcpServersBaselineSQL} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("complete frozen core baseline: %w", err)
		}
	}
	return nil
}

func applyBaselineV2(ctx context.Context, conn *sql.Conn) error {
	hasFormationColumns, err := columnExistsConn(ctx, conn, "memory_entries", "provenance_type")
	if err != nil {
		return err
	}
	approvalSQL := `UPDATE memory_entries SET profile_approved = 1;`
	if hasFormationColumns {
		approvalSQL = `UPDATE memory_entries SET profile_approved = 1 WHERE provenance_type = 'legacy_import' AND source_request_id = '' AND source_turn_id IS NULL;`
	}
	if _, err := conn.ExecContext(ctx, `UPDATE memory_entries SET category = 'communication_preferences' WHERE category = 'system_rules'; `+approvalSQL); err != nil {
		return fmt.Errorf("apply stable tenant profile data migration: %w", err)
	}
	return recordLegacyMigration(ctx, conn, "stable_tenant_profiles_v1")
}

func applyBaselineV3(ctx context.Context, conn *sql.Conn) error {
	hadFormationColumns, err := columnExistsConn(ctx, conn, "memory_entries", "provenance_type")
	if err != nil {
		return err
	}
	for _, column := range baselineV3MemoryColumns {
		if err := ensureColumnConn(ctx, conn, "memory_entries", column.name, column.definition); err != nil {
			return err
		}
	}
	if err := ensureColumnConn(ctx, conn, "session_turns", "source_request_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumnConn(ctx, conn, "session_turns", "formation_eligible_at", "TEXT"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, memoryFormationSchemaSQL); err != nil {
		return fmt.Errorf("create memory formation schema: %w", err)
	}
	if !hadFormationColumns {
		if _, err := conn.ExecContext(ctx, memoryFormationBackfillSQL); err != nil {
			return fmt.Errorf("backfill memory formation schema: %w", err)
		}
	}
	return recordLegacyMigration(ctx, conn, memoryFormationMigration)
}

func columnExistsConn(ctx context.Context, conn *sql.Conn, table, name string) (bool, error) {
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if columnName == name {
			return true, nil
		}
	}
	return false, rows.Err()
}

func applyBaselineV4(ctx context.Context, conn *sql.Conn) error {
	if err := ensureColumnConn(ctx, conn, "session_turns", "delivered_at", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumnConn(ctx, conn, "session_turns", "delivery_failed_at", "TEXT"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, sessionCompactionSchemaSQL); err != nil {
		return fmt.Errorf("create session compaction schema: %w", err)
	}
	return recordLegacyMigration(ctx, conn, sessionCompactionMigration)
}

func applyBaselineV5(ctx context.Context, conn *sql.Conn) error {
	if err := applyMemoryFTSMigration(ctx, conn); err != nil {
		return err
	}
	return recordLegacyMigration(ctx, conn, memoryFTSMigration)
}

func applyBaselineV6(ctx context.Context, conn *sql.Conn) error {
	if err := applySessionTurnsFTSMigration(ctx, conn); err != nil {
		return err
	}
	return recordLegacyMigration(ctx, conn, sessionTurnsFTSMigration)
}

func recordLegacyMigration(ctx context.Context, conn *sql.Conn, name string) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(name, applied_at) VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')) ON CONFLICT(name) DO NOTHING`, name)
	return err
}
