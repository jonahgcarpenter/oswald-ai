package database

import (
	"fmt"
	"time"
)

// AccountUser stores the linked accounts for a canonical Oswald user.
type AccountUser struct {
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Accounts  []LinkedAccount `json:"accounts"`
}

func (d *DB) initializeAccountUsers() error {
	_, err := d.db.Exec(`
CREATE TABLE IF NOT EXISTS account_users (
	canonical_user_id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`)
	if err != nil {
		return fmt.Errorf("failed to initialize account_users table: %w", err)
	}
	return nil
}
