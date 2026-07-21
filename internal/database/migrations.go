package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

type schemaMigration struct {
	version    int
	name       string
	definition string
	apply      func(context.Context, *sql.Conn) error
}

func orderedMigrations() []schemaMigration {
	return []schemaMigration{
		{version: 1, name: "v4_compact_baseline", definition: compactV4BaselineDefinition, apply: applyCompactV4Baseline},
	}
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

	// Build the frozen baseline without applying foreign-key actions until every
	// referenced table exists, then verify integrity before commit.
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
	var ledgerExists, schemaObjectCount int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migration_versions'`).Scan(&ledgerExists); err != nil {
		return fmt.Errorf("inspect schema migration ledger: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&schemaObjectCount); err != nil {
		return fmt.Errorf("inspect existing database schema: %w", err)
	}
	if ledgerExists == 0 && schemaObjectCount != 0 {
		return fmt.Errorf("database predates the disposable v4 baseline; reset the development database")
	}
	if ledgerExists != 0 {
		if err := validateFrozenV4Ledger(ctx, conn, registry); err != nil {
			return err
		}
	}

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
		if name != migration.name || checksum != expected {
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
			return fmt.Errorf("apply schema migration %d %q: %w", migration.version, migration.name, err)
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

func validateFrozenV4Ledger(ctx context.Context, conn *sql.Conn, registry []schemaMigration) error {
	var rowCount int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migration_versions`).Scan(&rowCount); err != nil {
		return fmt.Errorf("read frozen v4 schema migration ledger: %w", err)
	}
	if rowCount != len(registry) {
		return fmt.Errorf("invalid frozen v4 schema migration ledger: found %d rows, expected %d", rowCount, len(registry))
	}
	for _, migration := range registry {
		var name, checksum string
		if err := conn.QueryRowContext(ctx, `SELECT name, checksum FROM schema_migration_versions WHERE version = ?`, migration.version).Scan(&name, &checksum); err != nil {
			return fmt.Errorf("read frozen v4 schema migration version %d: %w", migration.version, err)
		}
		expected := migrationChecksum(migration)
		if name != migration.name || checksum != expected {
			return fmt.Errorf("schema migration checksum drift at version %d: database has %q/%q, registry has %q/%q", migration.version, name, checksum, migration.name, expected)
		}
	}
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
