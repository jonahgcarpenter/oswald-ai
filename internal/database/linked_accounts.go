package database

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// LinkedAccount records a single external gateway identity linked to a canonical user.
type LinkedAccount struct {
	Gateway     string    `json:"gateway"`
	Identifier  string    `json:"identifier"`
	DisplayName string    `json:"display_name"`
	LinkedAt    time.Time `json:"linked_at"`
	Verified    bool      `json:"verified"`
}

// AccountLinkData is the complete account-link dataset persisted in SQLite.
type AccountLinkData struct {
	Version      int                    `json:"version"`
	Users        map[string]AccountUser `json:"users"`
	AccountIndex map[string]string      `json:"account_index"`
}

func (d *DB) initializeLinkedAccounts() error {
	_, err := d.db.Exec(linkedAccountsBaselineSQL)
	if err != nil {
		return fmt.Errorf("failed to initialize linked_accounts table: %w", err)
	}
	return nil
}

const linkedAccountsBaselineSQL = `
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
`

// LoadAccountLinks reads all canonical users and linked accounts.
func (d *DB) LoadAccountLinks() (AccountLinkData, error) {
	data := AccountLinkData{
		Version:      1,
		Users:        make(map[string]AccountUser),
		AccountIndex: make(map[string]string),
	}

	userRows, err := d.db.Query(`SELECT canonical_user_id, created_at, updated_at, is_admin, is_banned, banned_at, banned_by, ban_reason FROM account_users`)
	if err != nil {
		return AccountLinkData{}, fmt.Errorf("failed to read account users: %w", err)
	}
	defer userRows.Close()

	for userRows.Next() {
		var canonicalID, createdRaw, updatedRaw, bannedRaw string
		var user AccountUser
		var isAdmin, isBanned int
		if err := userRows.Scan(&canonicalID, &createdRaw, &updatedRaw, &isAdmin, &isBanned, &bannedRaw, &user.BannedBy, &user.BanReason); err != nil {
			return AccountLinkData{}, fmt.Errorf("failed to scan account user: %w", err)
		}
		createdAt, err := parseDBTime(createdRaw)
		if err != nil {
			return AccountLinkData{}, err
		}
		updatedAt, err := parseDBTime(updatedRaw)
		if err != nil {
			return AccountLinkData{}, err
		}
		user.CreatedAt = createdAt
		user.UpdatedAt = updatedAt
		user.IsAdmin = isAdmin != 0
		user.IsBanned = isBanned != 0
		if bannedRaw != "" {
			bannedAt, err := parseDBTime(bannedRaw)
			if err != nil {
				return AccountLinkData{}, err
			}
			user.BannedAt = bannedAt
		}
		data.Users[canonicalID] = user
	}
	if err := userRows.Err(); err != nil {
		return AccountLinkData{}, fmt.Errorf("failed to read account users: %w", err)
	}

	accountRows, err := d.db.Query(`SELECT gateway, identifier, canonical_user_id, display_name, linked_at, verified FROM linked_accounts ORDER BY gateway, identifier`)
	if err != nil {
		return AccountLinkData{}, fmt.Errorf("failed to read linked accounts: %w", err)
	}
	defer accountRows.Close()

	for accountRows.Next() {
		var account LinkedAccount
		var canonicalID, linkedRaw string
		var verified int
		if err := accountRows.Scan(&account.Gateway, &account.Identifier, &canonicalID, &account.DisplayName, &linkedRaw, &verified); err != nil {
			return AccountLinkData{}, fmt.Errorf("failed to scan linked account: %w", err)
		}
		linkedAt, err := parseDBTime(linkedRaw)
		if err != nil {
			return AccountLinkData{}, err
		}
		account.LinkedAt = linkedAt
		account.Verified = verified != 0

		user := data.Users[canonicalID]
		user.Accounts = append(user.Accounts, account)
		data.Users[canonicalID] = user
		data.AccountIndex[accountKey(account.Gateway, account.Identifier)] = canonicalID
	}
	if err := accountRows.Err(); err != nil {
		return AccountLinkData{}, fmt.Errorf("failed to read linked accounts: %w", err)
	}

	return data, nil
}

// ReplaceAccountLinks atomically replaces all account-link rows without
// deleting unchanged account_users. User memory rows reference account_users, so
// wholesale deletes would cascade and erase persistent memories.
func (d *DB) ReplaceAccountLinks(data AccountLinkData) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin account link transaction: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	if _, err := tx.Exec(`DELETE FROM linked_accounts`); err != nil {
		return fmt.Errorf("failed to clear linked accounts: %w", err)
	}

	userStmt, err := tx.Prepare(`
INSERT INTO account_users (canonical_user_id, created_at, updated_at, is_admin, is_banned, banned_at, banned_by, ban_reason)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_user_id) DO UPDATE SET
	created_at = excluded.created_at,
	updated_at = excluded.updated_at,
	is_admin = excluded.is_admin,
	is_banned = excluded.is_banned,
	banned_at = excluded.banned_at,
	banned_by = excluded.banned_by,
	ban_reason = excluded.ban_reason
`)
	if err != nil {
		return fmt.Errorf("failed to prepare account user insert: %w", err)
	}
	defer userStmt.Close()

	accountStmt, err := tx.Prepare(`INSERT INTO linked_accounts (gateway, identifier, canonical_user_id, display_name, linked_at, verified) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("failed to prepare linked account insert: %w", err)
	}
	defer accountStmt.Close()

	userIDs := make([]string, 0, len(data.Users))
	for canonicalID := range data.Users {
		userIDs = append(userIDs, canonicalID)
	}
	sort.Strings(userIDs)

	for _, canonicalID := range userIDs {
		user := data.Users[canonicalID]
		if _, err := userStmt.Exec(canonicalID, formatDBTime(user.CreatedAt), formatDBTime(user.UpdatedAt), boolToInt(user.IsAdmin), boolToInt(user.IsBanned), formatOptionalDBTime(user.BannedAt), user.BannedBy, user.BanReason); err != nil {
			return fmt.Errorf("failed to save account user: %w", err)
		}
		for _, account := range user.Accounts {
			if _, err := accountStmt.Exec(account.Gateway, account.Identifier, canonicalID, account.DisplayName, formatDBTime(account.LinkedAt), boolToInt(account.Verified)); err != nil {
				return fmt.Errorf("failed to save linked account: %w", err)
			}
		}
	}
	if err := deleteStaleAccountUsers(tx, userIDs); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account link transaction: %w", err)
	}
	return nil
}

func deleteStaleAccountUsers(tx *sql.Tx, userIDs []string) error {
	if len(userIDs) == 0 {
		if _, err := tx.Exec(`DELETE FROM account_users`); err != nil {
			return fmt.Errorf("failed to remove stale account users: %w", err)
		}
		return nil
	}
	placeholders := make([]string, len(userIDs))
	args := make([]interface{}, len(userIDs))
	for i, userID := range userIDs {
		placeholders[i] = "?"
		args[i] = userID
	}
	query := `DELETE FROM account_users WHERE canonical_user_id NOT IN (` + strings.Join(placeholders, ",") + `)`
	if _, err := tx.Exec(query, args...); err != nil {
		return fmt.Errorf("failed to remove stale account users: %w", err)
	}
	return nil
}

// AccountLinksEmpty reports whether any canonical account users are stored.
func (d *DB) AccountLinksEmpty() (bool, error) {
	var count int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM account_users`).Scan(&count); err != nil {
		return false, fmt.Errorf("failed to inspect account link database: %w", err)
	}
	return count == 0, nil
}

func accountKey(gateway, identifier string) string {
	return strings.ToLower(strings.TrimSpace(gateway)) + ":" + strings.TrimSpace(identifier)
}

func formatDBTime(t time.Time) string {
	if t.IsZero() {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func formatOptionalDBTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseDBTime(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse account link timestamp: %w", err)
	}
	return t, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
