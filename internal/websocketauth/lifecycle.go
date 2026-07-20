package websocketauth

import (
	"context"
	"database/sql"
	"fmt"
)

// MergeUsersTx moves durable WebSocket ownership references during a canonical
// account merge. The caller owns transaction commit and loser deletion.
func MergeUsersTx(ctx context.Context, tx *sql.Tx, winner, loser string) error {
	if tx == nil || winner == "" || loser == "" || winner == loser {
		return fmt.Errorf("invalid websocket user merge")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM websocket_bootstrap_state WHERE (default_user_id = ? AND permanent_admin_user_id = ?) OR (default_user_id = ? AND permanent_admin_user_id = ?)`, loser, winner, winner, loser); err != nil {
		return fmt.Errorf("collapse websocket bootstrap ownership: %w", err)
	}
	statements := []string{
		`UPDATE websocket_clients SET canonical_user_id = ? WHERE canonical_user_id = ?`,
		`UPDATE websocket_device_authorizations SET target_user_id = ? WHERE target_user_id = ? AND state IN ('approved', 'consumed')`,
		`UPDATE websocket_bootstrap_state SET permanent_admin_user_id = ? WHERE permanent_admin_user_id = ?`,
		`UPDATE websocket_bootstrap_state SET default_user_id = ? WHERE default_user_id = ?`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, winner, loser); err != nil {
			return fmt.Errorf("merge websocket user authorization state: %w", err)
		}
	}
	return nil
}

// ExternalIdentitiesTx returns privacy invalidation scopes for a canonical
// user's WebSocket linked identities and durable clients.
func ExternalIdentitiesTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	if tx == nil || userID == "" {
		return nil, fmt.Errorf("invalid websocket external identity query")
	}
	rows, err := tx.QueryContext(ctx, `SELECT identifier FROM linked_accounts WHERE canonical_user_id = ? AND gateway = 'websocket' UNION SELECT websocket_identifier FROM websocket_clients WHERE canonical_user_id = ? ORDER BY 1`, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("read websocket external identities: %w", err)
	}
	defer rows.Close()
	var identities []string
	for rows.Next() {
		var identifier string
		if err := rows.Scan(&identifier); err != nil {
			return nil, err
		}
		identities = append(identities, "websocket:"+identifier)
	}
	return identities, rows.Err()
}
