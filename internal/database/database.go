package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"

	_ "github.com/mattn/go-sqlite3"
)

// DB owns the application's SQLite connection and schema initialization.
type DB struct {
	path string
	log  *config.Logger
	db   *sql.DB
}

var schemaInitializationMu sync.Mutex

// Open initializes the application database at path.
func Open(path string, log *config.Logger) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	sqlite_vec.Auto()

	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_secure_delete=on&_synchronous=NORMAL&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	var vecVersion string
	if err := db.QueryRow(`SELECT vec_version()`).Scan(&vecVersion); err != nil {
		db.Close() // nolint:errcheck
		return nil, fmt.Errorf("failed to initialize sqlite-vec: %w", err)
	}

	store := &DB{path: path, log: log, db: db}
	schemaInitializationMu.Lock()
	defer schemaInitializationMu.Unlock()
	var schemaObjectCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&schemaObjectCount); err != nil {
		db.Close() // nolint:errcheck
		return nil, fmt.Errorf("inspect database auto-vacuum eligibility: %w", err)
	}
	if schemaObjectCount == 0 {
		if _, err := db.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
			db.Close() // nolint:errcheck
			return nil, fmt.Errorf("enable incremental auto-vacuum for new database: %w", err)
		}
	}
	if err := store.initialize(); err != nil {
		db.Close() // nolint:errcheck
		return nil, err
	}
	return store, nil
}

// SQL returns the underlying database handle for package-specific stores.
func (d *DB) SQL() *sql.DB {
	if d == nil {
		return nil
	}
	return d.db
}

// WithTx executes fn in a transaction, committing on success and rolling back
// when fn or the commit fails.
func (d *DB) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin database transaction: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit database transaction: %w", err)
	}
	return nil
}

// Close closes the database connection.
func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

func (d *DB) initialize() error {
	if _, err := d.db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("failed to enable database foreign keys: %w", err)
	}
	if _, err := d.db.Exec(`PRAGMA secure_delete = ON`); err != nil {
		return fmt.Errorf("failed to enable secure deletion: %w", err)
	}
	// WAL keeps normal writers durable while maintenance performs passive
	// checkpoints after retention batches.
	if _, err := d.db.Exec(`PRAGMA journal_mode = WAL; PRAGMA synchronous = NORMAL; PRAGMA wal_autocheckpoint = 1000`); err != nil {
		return fmt.Errorf("failed to configure database WAL durability: %w", err)
	}
	if err := d.runSchemaMigrations(context.Background(), orderedMigrations()); err != nil {
		return err
	}
	return nil
}
