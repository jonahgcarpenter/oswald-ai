package accounts

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

// EnsureLocalOpenAIAdmin creates the loopback listener's persistent owner as
// an administrator. Existing admin and ban state is never reset on restart.
func (s *Service) EnsureLocalOpenAIAdmin(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Initialize(); err != nil {
		return err
	}
	var createdID string
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, `SELECT u.lifecycle_state FROM linked_accounts a
			JOIN account_users u ON u.canonical_user_id = a.canonical_user_id
			WHERE a.gateway = 'openai' AND a.identifier = ?`, identity.LocalOpenAIIdentifier).Scan(&state)
		if err == nil {
			if state != "active" {
				return fmt.Errorf("local openai account is not active")
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("resolve local openai account: %w", err)
		}
		createdID, err = newCanonicalUserID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_users(canonical_user_id, is_admin, speaker_intro) VALUES (?, 1, ?)`, createdID, FormatSpeakerLine(nil)); err != nil {
			return fmt.Errorf("create local openai administrator: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, verified) VALUES ('openai', ?, ?, 1)`, identity.LocalOpenAIIdentifier, createdID); err != nil {
			return fmt.Errorf("link local openai administrator: %w", err)
		}
		return nil
	})
	if err == nil && createdID != "" && s.log != nil {
		s.log.Server("accounts").Info("account.openai_local.created", "created local openai administrator", config.F("user_id", createdID), config.F("status", "ok"))
	}
	return err
}

// LocalOpenAIPrincipal resolves the active owner of the loopback listener's
// fixed identity without accepting a client-supplied account identifier.
func (s *Service) LocalOpenAIPrincipal(ctx context.Context) (identity.Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Initialize(); err != nil {
		return identity.Principal{}, err
	}
	var owner string
	err := s.db.SQL().QueryRowContext(ctx, `SELECT a.canonical_user_id FROM linked_accounts a
		JOIN account_users u ON u.canonical_user_id = a.canonical_user_id AND u.lifecycle_state = 'active'
		WHERE a.gateway = 'openai' AND a.identifier = ?`, identity.LocalOpenAIIdentifier).Scan(&owner)
	if err == sql.ErrNoRows {
		return identity.Principal{}, ErrPrincipalMismatch
	}
	if err != nil {
		return identity.Principal{}, fmt.Errorf("resolve local openai principal: %w", err)
	}
	return identity.Principal{CanonicalUserID: owner, Gateway: "openai", ExternalID: identity.LocalOpenAIIdentifier, Assurance: identity.AssuranceLocalLoopback}, nil
}
