package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

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

// Open initializes the application database at path.
func Open(path string, log *config.Logger) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	sqlite_vec.Auto()

	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	var vecVersion string
	if err := db.QueryRow(`SELECT vec_version()`).Scan(&vecVersion); err != nil {
		db.Close() // nolint:errcheck
		return nil, fmt.Errorf("failed to initialize sqlite-vec: %w", err)
	}

	store := &DB{path: path, log: log, db: db}
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
	if err := d.initializeAccountUsers(); err != nil {
		return err
	}
	if err := d.initializeLinkedAccounts(); err != nil {
		return err
	}
	if err := d.initializeUserMemory(); err != nil {
		return err
	}
	if err := d.initializeMCPServers(); err != nil {
		return err
	}
	return nil
}
