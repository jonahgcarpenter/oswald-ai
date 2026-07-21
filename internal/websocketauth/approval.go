package websocketauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ApproveForUser approves a pending human code for an existing canonical user,
// creating or reusing that user's verified WebSocket linked identity.
func (s *Store) ApproveForUser(ctx context.Context, userID, displayName, userCode string) (string, error) {
	identifier, err := s.randomID("ws_", 18)
	if err != nil {
		return "", err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		identifier, err = s.ensureWebSocketIdentityTx(ctx, tx, userID, displayName, identifier)
		if err != nil {
			return err
		}
		return s.approveCodeTx(ctx, tx, userCode, userID, identifier)
	})
	return identifier, err
}

// ApproveNewUser atomically creates a canonical user and verified WebSocket
// identity and approves a pending human code for it.
func (s *Store) ApproveNewUser(ctx context.Context, userCode, displayName string, isAdmin bool) (string, error) {
	userID, err := s.randomID("usr_", 18)
	if err != nil {
		return "", err
	}
	identifier, err := s.randomID("ws_", 18)
	if err != nil {
		return "", err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		return s.createAndApproveUserTx(ctx, tx, userCode, userID, identifier, displayName, isAdmin)
	})
	return userID, err
}

func (s *Store) createAndApproveUserTx(ctx context.Context, tx *sql.Tx, userCode, userID, identifier, displayName string, isAdmin bool) error {
	now := timestamp(s.now().UTC())
	admin := 0
	if isAdmin {
		admin = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_users (canonical_user_id, created_at, updated_at, is_admin) VALUES (?, ?, ?, ?)`, userID, now, now, admin); err != nil {
		return fmt.Errorf("create websocket canonical user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO linked_accounts (gateway, identifier, canonical_user_id, display_name, linked_at, verified) VALUES ('websocket', ?, ?, ?, ?, 1)`, identifier, userID, cleanDisplayName(displayName), now); err != nil {
		return fmt.Errorf("create websocket linked identity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE account_users SET speaker_intro = ?, updated_at = ? WHERE canonical_user_id = ?`, "You are speaking with "+cleanDisplayName(displayName)+".", now, userID); err != nil {
		return fmt.Errorf("create websocket user profile: %w", err)
	}
	return s.approveCodeTx(ctx, tx, userCode, userID, identifier)
}

func (s *Store) ensureWebSocketIdentityTx(ctx context.Context, tx *sql.Tx, userID, displayName, generatedIdentifier string) (string, error) {
	var identifier string
	err := tx.QueryRowContext(ctx, `SELECT identifier FROM linked_accounts WHERE canonical_user_id = ? AND gateway = 'websocket'`, userID).Scan(&identifier)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE linked_accounts SET verified = 1, display_name = CASE WHEN ? = '' THEN display_name ELSE ? END WHERE gateway = 'websocket' AND identifier = ?`, cleanDisplayName(displayName), cleanDisplayName(displayName), identifier); err != nil {
			return "", fmt.Errorf("verify websocket linked identity: %w", err)
		}
		return identifier, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read websocket linked identity: %w", err)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_users WHERE canonical_user_id = ? AND lifecycle_state = 'active'`, userID).Scan(&exists); err != nil {
		return "", fmt.Errorf("read canonical user: %w", err)
	}
	if exists != 1 {
		return "", fmt.Errorf("canonical user does not exist or is inactive")
	}
	now := timestamp(s.now().UTC())
	if _, err := tx.ExecContext(ctx, `INSERT INTO linked_accounts (gateway, identifier, canonical_user_id, display_name, linked_at, verified) VALUES ('websocket', ?, ?, ?, ?, 1)`, generatedIdentifier, userID, cleanDisplayName(displayName), now); err != nil {
		return "", fmt.Errorf("create websocket linked identity: %w", err)
	}
	return generatedIdentifier, nil
}

func (s *Store) approveCodeTx(ctx context.Context, tx *sql.Tx, userCode, userID, identifier string) error {
	if normalizeUserCode(userCode) == "" {
		return ErrInvalidUserCode
	}
	var id int64
	var expiresRaw string
	err := tx.QueryRowContext(ctx, `SELECT id, expires_at FROM websocket_device_authorizations WHERE user_code_hash = ? AND state = 'pending'`, s.hashUserCode(userCode)).Scan(&id, &expiresRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidUserCode
	}
	if err != nil {
		return fmt.Errorf("read websocket user code: %w", err)
	}
	now := s.now().UTC()
	expires, err := parseTime(expiresRaw)
	if err != nil {
		return err
	}
	if !expires.After(now) {
		return ErrExpired
	}
	result, err := tx.ExecContext(ctx, `UPDATE websocket_device_authorizations SET state = 'approved', target_user_id = ?, websocket_identifier = ?, approved_at = ?, updated_at = ? WHERE id = ? AND state = 'pending'`, userID, identifier, timestamp(now), timestamp(now), id)
	if err != nil {
		return fmt.Errorf("approve websocket user code: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrInvalidUserCode
	}
	return nil
}

func cleanDisplayName(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 256 {
		value = value[:256]
	}
	return value
}
