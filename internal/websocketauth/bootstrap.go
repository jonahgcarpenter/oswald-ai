package websocketauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EnsureBootstrap initializes a temporary administrator only for a completely
// empty account database. A pending state is recovered by replacing its JWT and
// incrementing the client token version.
func (s *Store) EnsureBootstrap(ctx context.Context) (*BootstrapCredentials, error) {
	newUserID, err := s.randomID("usr_", 18)
	if err != nil {
		return nil, err
	}
	newIdentifier, err := s.randomID("ws_", 18)
	if err != nil {
		return nil, err
	}
	newClientID, err := s.randomID("wsc_", 18)
	if err != nil {
		return nil, err
	}
	var credentials BootstrapCredentials
	initialized := false
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var state string
		var version int64
		err := tx.QueryRowContext(ctx, `SELECT b.state, b.default_user_id, b.websocket_identifier, b.bootstrap_client_id, c.token_version FROM websocket_bootstrap_state b JOIN websocket_clients c ON c.client_id = b.bootstrap_client_id WHERE b.singleton_id = 1`).Scan(&state, &credentials.DefaultUserID, &credentials.WebSocketIdentifier, &credentials.ClientID, &version)
		if err == nil {
			if state != "pending" {
				return nil
			}
			version++
			now := timestamp(s.now().UTC())
			if _, err := tx.ExecContext(ctx, `UPDATE websocket_clients SET token_version = ?, revoked_at = NULL, last_used_at = ? WHERE client_id = ? AND is_bootstrap = 1`, version, now, credentials.ClientID); err != nil {
				return fmt.Errorf("replace websocket bootstrap credential: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE websocket_bootstrap_state SET updated_at = ? WHERE singleton_id = 1`, now); err != nil {
				return err
			}
			initialized = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read websocket bootstrap state: %w", err)
		}
		var users int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_users`).Scan(&users); err != nil {
			return fmt.Errorf("count canonical users for websocket bootstrap: %w", err)
		}
		if users != 0 {
			return nil
		}
		now := timestamp(s.now().UTC())
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_users (canonical_user_id, created_at, updated_at, is_admin) VALUES (?, ?, ?, 1)`, newUserID, now, now); err != nil {
			return fmt.Errorf("create websocket bootstrap user: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO linked_accounts (gateway, identifier, canonical_user_id, display_name, linked_at, verified) VALUES ('websocket', ?, ?, 'Bootstrap Administrator', ?, 1)`, newIdentifier, newUserID, now); err != nil {
			return fmt.Errorf("create websocket bootstrap identity: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_users SET speaker_intro = 'You are speaking with Bootstrap Administrator.', updated_at = ? WHERE canonical_user_id = ?`, now, newUserID); err != nil {
			return fmt.Errorf("create websocket bootstrap profile: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO websocket_clients (client_id, canonical_user_id, websocket_identifier, client_name, token_version, is_bootstrap, created_at, last_used_at) VALUES (?, ?, ?, 'Bootstrap Administrator', 1, 1, ?, ?)`, newClientID, newUserID, newIdentifier, now, now); err != nil {
			return fmt.Errorf("create websocket bootstrap client: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO websocket_bootstrap_state (singleton_id, default_user_id, websocket_identifier, bootstrap_client_id, created_at, updated_at) VALUES (1, ?, ?, ?, ?, ?)`, newUserID, newIdentifier, newClientID, now, now); err != nil {
			return fmt.Errorf("create websocket bootstrap state: %w", err)
		}
		credentials.DefaultUserID = newUserID
		credentials.WebSocketIdentifier = newIdentifier
		credentials.ClientID = newClientID
		initialized = true
		return nil
	})
	if err != nil || !initialized {
		return nil, err
	}
	var version int64
	if err := s.db.QueryRowContext(ctx, `SELECT token_version FROM websocket_clients WHERE client_id = ?`, credentials.ClientID).Scan(&version); err != nil {
		return nil, fmt.Errorf("read websocket bootstrap client version: %w", err)
	}
	credentials.AccessToken, credentials.ExpiresAt, err = s.issueAccess(credentials.WebSocketIdentifier, credentials.ClientID, version)
	if err != nil {
		return nil, err
	}
	return &credentials, nil
}

// BootstrapAdmin verifies the pending temporary bootstrap caller, creates a
// distinct permanent administrator, and approves the supplied device code.
func (s *Store) BootstrapAdmin(ctx context.Context, userCode, displayName, bootstrapUserID, bootstrapClientID string) (string, error) {
	userID, err := s.randomID("usr_", 18)
	if err != nil {
		return "", err
	}
	identifier, err := s.randomID("ws_", 18)
	if err != nil {
		return "", err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var state, defaultUserID, clientID string
		var permanent sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT state, default_user_id, bootstrap_client_id, permanent_admin_user_id FROM websocket_bootstrap_state WHERE singleton_id = 1`).Scan(&state, &defaultUserID, &clientID, &permanent)
		if err != nil || state != "pending" || defaultUserID != bootstrapUserID || clientID != bootstrapClientID || permanent.Valid {
			return ErrBootstrapUnavailable
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM websocket_clients WHERE client_id = ? AND canonical_user_id = ? AND is_bootstrap = 1 AND revoked_at IS NULL`, bootstrapClientID, bootstrapUserID).Scan(&active); err != nil || active != 1 {
			return ErrBootstrapUnavailable
		}
		if err := s.createAndApproveUserTx(ctx, tx, userCode, userID, identifier, displayName, true); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE websocket_bootstrap_state SET permanent_admin_user_id = ?, updated_at = ? WHERE singleton_id = 1 AND state = 'pending' AND permanent_admin_user_id IS NULL`, userID, timestamp(s.now().UTC()))
		if err != nil {
			return fmt.Errorf("record permanent websocket administrator: %w", err)
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrBootstrapUnavailable
		}
		return nil
	})
	return userID, err
}

// CompleteBootstrapOnAdminConnection completes bootstrap after the permanent
// administrator first authenticates and revokes the temporary client.
func (s *Store) CompleteBootstrapOnAdminConnection(ctx context.Context, userID string) (*BootstrapCompletion, error) {
	var completion BootstrapCompletion
	completed := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var state string
		var permanent sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT state, default_user_id, bootstrap_client_id, permanent_admin_user_id FROM websocket_bootstrap_state WHERE singleton_id = 1`).Scan(&state, &completion.DefaultUserID, &completion.ClientID, &permanent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read websocket bootstrap completion state: %w", err)
		}
		if state != "pending" || !permanent.Valid || permanent.String != userID {
			return nil
		}
		now := timestamp(s.now().UTC())
		if _, err := tx.ExecContext(ctx, `UPDATE websocket_clients SET revoked_at = ?, token_version = token_version + 1 WHERE client_id = ? AND revoked_at IS NULL`, now, completion.ClientID); err != nil {
			return fmt.Errorf("revoke websocket bootstrap client: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE websocket_bootstrap_state SET state = 'completed', completed_at = ?, updated_at = ? WHERE singleton_id = 1 AND state = 'pending'`, now, now)
		if err != nil {
			return fmt.Errorf("complete websocket bootstrap: %w", err)
		}
		changed, _ := result.RowsAffected()
		completed = changed == 1
		return nil
	})
	if err != nil || !completed {
		return nil, err
	}
	return &completion, nil
}
