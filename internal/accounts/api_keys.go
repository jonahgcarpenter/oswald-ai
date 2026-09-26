package accounts

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

// ErrInvalidAPIKey indicates a malformed, unknown, or revoked API key.
var ErrInvalidAPIKey = errors.New("invalid API key")

// CreateAPIKey issues one key for an existing canonical user. The token is returned
// only once; persistence stores only its secret hash.
func (s *Service) CreateAPIKey(ctx context.Context, canonicalUserID string) (keyID, token string, err error) {
	if canonicalUserID == "" || strings.TrimSpace(canonicalUserID) != canonicalUserID {
		return "", "", fmt.Errorf("canonical user ID is required")
	}
	var random [48]byte
	if _, err := io.ReadFull(s.random, random[:]); err != nil {
		return "", "", fmt.Errorf("generate API key: %w", err)
	}
	keyID = hex.EncodeToString(random[:16])
	secret := hex.EncodeToString(random[16:])
	hash := sha256.Sum256([]byte(secret))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Initialize(); err != nil {
		return "", "", err
	}
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM account_users WHERE canonical_user_id = ? AND lifecycle_state = 'active'`, canonicalUserID).Scan(&exists); err == sql.ErrNoRows {
			return fmt.Errorf("canonical user not found")
		} else if err != nil {
			return fmt.Errorf("check canonical user: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, verified) VALUES ('openai', ?, ?, 1)`, keyID, canonicalUserID); err != nil {
			return fmt.Errorf("link API key: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_keys(key_id, canonical_user_id, secret_hash) VALUES (?, ?, ?)`, keyID, canonicalUserID, hash[:]); err != nil {
			return fmt.Errorf("store API key: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if s.log != nil {
		s.log.Server("accounts").Info("account.api_key.created", "API key issued", config.F("user_id", canonicalUserID), config.F("status", "ok"))
	}
	return keyID, "osk_" + keyID + "_" + secret, nil
}

// AuthenticateAPIKey resolves a live key to its current canonical owner.
func (s *Service) AuthenticateAPIKey(ctx context.Context, token string) (identity.Principal, error) {
	parts := strings.Split(token, "_")
	if len(parts) != 3 || parts[0] != "osk" || len(parts[1]) != 32 || len(parts[2]) != 64 {
		return identity.Principal{}, ErrInvalidAPIKey
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return identity.Principal{}, ErrInvalidAPIKey
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return identity.Principal{}, ErrInvalidAPIKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Initialize(); err != nil {
		return identity.Principal{}, err
	}
	var owner string
	var stored []byte
	err := s.db.SQL().QueryRowContext(ctx, `SELECT k.canonical_user_id, k.secret_hash FROM api_keys k
		JOIN linked_accounts a ON a.gateway = 'openai' AND a.identifier = k.key_id AND a.canonical_user_id = k.canonical_user_id
		JOIN account_users u ON u.canonical_user_id = k.canonical_user_id AND u.lifecycle_state = 'active'
		WHERE k.key_id = ?`, parts[1]).Scan(&owner, &stored)
	if err == sql.ErrNoRows {
		return identity.Principal{}, ErrInvalidAPIKey
	}
	if err != nil {
		return identity.Principal{}, fmt.Errorf("authenticate API key: %w", err)
	}
	hash := sha256.Sum256([]byte(parts[2]))
	if subtle.ConstantTimeCompare(hash[:], stored) != 1 {
		return identity.Principal{}, ErrInvalidAPIKey
	}
	return identity.Principal{CanonicalUserID: owner, Gateway: "openai", ExternalID: parts[1], Assurance: identity.AssuranceAPIKey}, nil
}

// RevokeAPIKey permanently removes an issued key and its linked identity.
func (s *Service) RevokeAPIKey(ctx context.Context, keyID string) error {
	if _, err := NormalizeIdentifier("openai", keyID); err != nil {
		return ErrInvalidAPIKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Initialize(); err != nil {
		return err
	}
	var owner string
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT canonical_user_id FROM api_keys WHERE key_id = ?`, keyID).Scan(&owner); err == sql.ErrNoRows {
			return ErrInvalidAPIKey
		} else if err != nil {
			return fmt.Errorf("resolve API key: %w", err)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE key_id = ?`, keyID)
		if err != nil {
			return fmt.Errorf("revoke API key: %w", err)
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrInvalidAPIKey
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM linked_accounts WHERE gateway = 'openai' AND identifier = ?`, keyID); err != nil {
			return fmt.Errorf("unlink API key: %w", err)
		}
		return nil
	})
	if err == nil && s.log != nil {
		s.log.Server("accounts").Info("account.api_key.revoked", "API key revoked", config.F("user_id", owner), config.F("status", "ok"))
	}
	return err
}
