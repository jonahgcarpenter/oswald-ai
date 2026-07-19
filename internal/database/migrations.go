package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type schemaMigration struct {
	version    int
	name       string
	definition string
	optional   bool
	apply      func(context.Context, *sql.Conn) error
}

func orderedMigrations() []schemaMigration {
	return []schemaMigration{
		{version: 1, name: "legacy_core_schema", definition: baselineV1Definition, apply: applyBaselineV1},
		{version: 2, name: "stable_tenant_profiles", definition: baselineV2Definition, apply: applyBaselineV2},
		{version: 3, name: "canonical_memory_formation", definition: baselineV3Definition, apply: applyBaselineV3},
		{version: 4, name: "session_compaction", definition: baselineV4Definition, apply: applyBaselineV4},
		{version: 5, name: "memory_fts", definition: baselineV5Definition, optional: true, apply: applyBaselineV5},
		{version: 6, name: "session_transcript_fts", definition: baselineV6Definition, optional: true, apply: applyBaselineV6},
		{version: 7, name: "memory_operations_privacy", definition: phase7MigrationDefinition, apply: applyPhase7Migration},
		{version: 8, name: "privacy_operation_corrections", definition: phase8MigrationDefinition, apply: applyPhase8Migration},
		{version: 9, name: "privacy_operation_retention", definition: phase9MigrationDefinition, apply: applyPhase9Migration},
		{version: 10, name: "memory_event_redaction_retention", definition: phase10MigrationDefinition, apply: applyPhase10Migration},
		{version: 11, name: "privacy_invalidation_outbox", definition: phase11MigrationDefinition, apply: applyPhase11Migration},
	}
}

var legacySymbolicChecksums = map[int]string{
	1: migrationChecksum(schemaMigration{version: 1, name: "legacy_core_schema", definition: "baseline:v1:account and memory core"}),
	2: migrationChecksum(schemaMigration{version: 2, name: "stable_tenant_profiles", definition: "baseline:v2:stable_tenant_profiles_v1"}),
	3: migrationChecksum(schemaMigration{version: 3, name: "canonical_memory_formation", definition: "baseline:v3:canonical_memory_formation_v1"}),
	4: migrationChecksum(schemaMigration{version: 4, name: "session_compaction", definition: "baseline:v4:session_compaction_v2"}),
	5: migrationChecksum(schemaMigration{version: 5, name: "memory_fts", definition: "baseline:v5:memory_fts_v3_active_only:optional-derived"}),
	6: migrationChecksum(schemaMigration{version: 6, name: "session_transcript_fts", definition: "baseline:v6:session_turns_fts_v1:optional-derived"}),
}

func migrationChecksum(m schemaMigration) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\n%s\n%s", m.version, m.name, m.definition)))
	return hex.EncodeToString(sum[:])
}

func (d *DB) runSchemaMigrations(ctx context.Context, registry []schemaMigration) (err error) {
	if err := validateMigrationRegistry(registry); err != nil {
		return err
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire schema migration connection: %w", err)
	}
	defer conn.Close()

	// Rebuild migrations need to replace parent tables without firing legacy
	// cascading actions. Integrity is checked before the transaction commits.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for schema migrations: %w", err)
	}
	defer func() {
		_, enableErr := conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`)
		if err == nil && enableErr != nil {
			err = fmt.Errorf("restore foreign keys after schema migrations: %w", enableErr)
		}
	}()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin immediate schema migration transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	if _, err := conn.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migration_versions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	applied_at TEXT NOT NULL
)`); err != nil {
		return fmt.Errorf("initialize ordered schema migration ledger: %w", err)
	}
	registered := make(map[int]schemaMigration, len(registry))
	for _, migration := range registry {
		registered[migration.version] = migration
	}
	rows, err := conn.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migration_versions ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read applied schema migrations: %w", err)
	}
	expectedAppliedVersion := 1
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("scan applied schema migration: %w", err)
		}
		if version != expectedAppliedVersion {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("schema migration ledger is not contiguous: expected version %d, found %d", expectedAppliedVersion, version)
		}
		expectedAppliedVersion++
		migration, ok := registered[version]
		if !ok {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("database has unknown schema migration version %d", version)
		}
		expected := migrationChecksum(migration)
		legacyChecksum := legacySymbolicChecksums[version]
		if name != migration.name || (checksum != expected && checksum != legacyChecksum) {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("schema migration checksum drift at version %d: database has %q/%q, registry has %q/%q", version, name, checksum, migration.name, expected)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close() // nolint:errcheck
		return fmt.Errorf("read applied schema migrations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close applied schema migrations: %w", err)
	}
	// The first ordered framework used symbolic validators for versions 1-6.
	// Rebase only those exact known checksums after every ledger row has passed
	// drift validation, then apply the frozen operations idempotently.
	for _, migration := range registry {
		legacyChecksum, ok := legacySymbolicChecksums[migration.version]
		if !ok {
			continue
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM schema_migration_versions WHERE version = ? AND name = ? AND checksum = ?`, migration.version, migration.name, legacyChecksum); err != nil {
			return fmt.Errorf("rebase symbolic schema migration %d: %w", migration.version, err)
		}
	}

	for _, migration := range registry {
		checksum := migrationChecksum(migration)
		var appliedName, appliedChecksum string
		err := conn.QueryRowContext(ctx, `SELECT name, checksum FROM schema_migration_versions WHERE version = ?`, migration.version).Scan(&appliedName, &appliedChecksum)
		switch {
		case err == nil:
			if appliedName != migration.name || appliedChecksum != checksum {
				return fmt.Errorf("schema migration checksum drift at version %d: database has %q/%q, registry has %q/%q", migration.version, appliedName, appliedChecksum, migration.name, checksum)
			}
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("inspect schema migration version %d: %w", migration.version, err)
		}
		if err := migration.apply(ctx, conn); err != nil {
			if !migration.optional || !errors.Is(err, ErrFTS5Unavailable) {
				return fmt.Errorf("apply schema migration %d %q: %w", migration.version, migration.name, err)
			}
			if d.log != nil {
				d.log.Server("database.migrations").Warn("database.schema.optional_unavailable", "optional database schema capability unavailable", config.F("status", "degraded"), config.F("migration_version", migration.version), config.F("migration_name", migration.name), config.ErrorField(err))
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migration_versions (version, name, checksum, applied_at) VALUES (?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`, migration.version, migration.name, checksum); err != nil {
			return fmt.Errorf("record schema migration %d %q: %w", migration.version, migration.name, err)
		}
	}
	if err := foreignKeyCheck(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit schema migrations: %w", err)
	}
	committed = true
	return nil
}

func validateMigrationRegistry(registry []schemaMigration) error {
	previous := 0
	names := make(map[string]struct{}, len(registry))
	for _, migration := range registry {
		if migration.version != previous+1 || migration.name == "" || migration.definition == "" || migration.apply == nil {
			return fmt.Errorf("invalid schema migration registry entry at version %d", migration.version)
		}
		if _, exists := names[migration.name]; exists {
			return fmt.Errorf("duplicate schema migration name %q", migration.name)
		}
		names[migration.name] = struct{}{}
		previous = migration.version
	}
	return nil
}

func requireTables(ctx context.Context, conn *sql.Conn, names ...string) error {
	for _, name := range names {
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("required legacy table %q is missing", name)
		}
	}
	return nil
}

func foreignKeyCheck(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check schema migration foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID sql.NullInt64
		var parent string
		var foreignKey int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKey); err != nil {
			return fmt.Errorf("read schema migration foreign key violation: %w", err)
		}
		return fmt.Errorf("schema migration foreign key violation in %s row %v referencing %s", table, rowID, parent)
	}
	return rows.Err()
}
