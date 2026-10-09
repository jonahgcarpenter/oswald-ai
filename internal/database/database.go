package database

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	_ "github.com/mattn/go-sqlite3"
	"sync"
)

// DB owns one approved profile-local SQLite connection.
type DB struct {
	path string
	log  *config.Logger
	db   *sql.DB
}

var schemaInitializationMu sync.Mutex

// SQL returns the profile database handle for domain stores.
func (d *DB) SQL() *sql.DB {
	if d == nil {
		return nil
	}
	return d.db
}

// WithTx commits only if fn and commit both succeed.
func (d *DB) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin profile transaction: %w", err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Close releases the database after all profile workers are joined.
func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}
