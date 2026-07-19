package database

import "fmt"

func (d *DB) initializeAccountLinkChallenges() error {
	_, err := d.db.Exec(accountLinkChallengesBaselineSQL)
	if err != nil {
		return fmt.Errorf("failed to initialize account_link_challenges table: %w", err)
	}
	return nil
}

const accountLinkChallengesBaselineSQL = `
CREATE TABLE IF NOT EXISTS account_link_challenges (
	id TEXT PRIMARY KEY,
	code_hash TEXT NOT NULL UNIQUE,
	initiator_user_id TEXT NOT NULL,
	initiator_gateway TEXT NOT NULL,
	initiator_identifier TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	consumed_at TEXT,
	consumed_by_user_id TEXT,
	consumed_gateway TEXT,
	consumed_identifier TEXT,
	result_user_id TEXT,
	invalidated_at TEXT,
	invalidated_by_user_id TEXT,
	invalidated_reason TEXT
);

CREATE INDEX IF NOT EXISTS idx_account_link_challenges_expiry
ON account_link_challenges (expires_at);

CREATE INDEX IF NOT EXISTS idx_account_link_challenges_initiator_state
ON account_link_challenges (initiator_user_id, consumed_at, invalidated_at, expires_at);
`
