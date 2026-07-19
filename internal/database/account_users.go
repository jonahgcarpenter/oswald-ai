package database

import (
	"fmt"
	"time"
)

// AccountUser stores the linked accounts for a canonical Oswald user.
type AccountUser struct {
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	IsAdmin   bool            `json:"is_admin"`
	IsBanned  bool            `json:"is_banned"`
	BannedAt  time.Time       `json:"banned_at"`
	BannedBy  string          `json:"banned_by"`
	BanReason string          `json:"ban_reason"`
	Accounts  []LinkedAccount `json:"accounts"`
}

func (d *DB) initializeAccountUsers() error {
	_, err := d.db.Exec(accountUsersBaselineSQL)
	if err != nil {
		return fmt.Errorf("failed to initialize account_users table: %w", err)
	}
	for _, column := range accountUserLegacyColumns {
		if err := d.ensureAccountUserColumn(column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

const accountUsersBaselineSQL = `
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
`

var accountUserLegacyColumns = []struct {
	name       string
	definition string
}{
	{name: "is_admin", definition: "INTEGER NOT NULL DEFAULT 0"},
	{name: "is_banned", definition: "INTEGER NOT NULL DEFAULT 0"},
	{name: "banned_at", definition: "TEXT NOT NULL DEFAULT ''"},
	{name: "banned_by", definition: "TEXT NOT NULL DEFAULT ''"},
	{name: "ban_reason", definition: "TEXT NOT NULL DEFAULT ''"},
}

func (d *DB) ensureAccountUserColumn(name, definition string) error {
	rows, err := d.db.Query(`PRAGMA table_info(account_users)`)
	if err != nil {
		return fmt.Errorf("failed to inspect account_users table: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull int
		var defaultValue interface{}
		var pk int
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("failed to scan account_users schema: %w", err)
		}
		if columnName == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to inspect account_users schema: %w", err)
	}

	if _, err := d.db.Exec(fmt.Sprintf(`ALTER TABLE account_users ADD COLUMN %s %s`, name, definition)); err != nil {
		return fmt.Errorf("failed to add account_users.%s column: %w", name, err)
	}
	return nil
}
