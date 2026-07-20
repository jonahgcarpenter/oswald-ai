package websocketauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Store owns durable WebSocket authorization state and token signing.
type Store struct {
	db         *sql.DB
	signingKey []byte
	accessTTL  time.Duration
	now        func() time.Time
	random     io.Reader
}

// New creates a durable authorization store. Access JWT lifetime is capped at
// 15 minutes independently of refresh-token lifetime.
func New(db *sql.DB, signingKey string, accessTTL time.Duration, options ...Option) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("websocket authorization database is required")
	}
	if accessTTL <= 0 || accessTTL > maximumAccessTTL {
		return nil, fmt.Errorf("websocket access token TTL must be positive and no greater than %s", maximumAccessTTL)
	}
	key, err := decodeSigningKey(signingKey)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, signingKey: key, accessTTL: accessTTL, now: time.Now, random: rand.Reader}
	for _, option := range options {
		if err := option(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// RequestDevice creates a pending one-time device authorization.
func (s *Store) RequestDevice(ctx context.Context, clientName string) (DeviceRequest, error) {
	clientName = strings.TrimSpace(clientName)
	if len(clientName) < 1 || len(clientName) > 128 {
		return DeviceRequest{}, fmt.Errorf("client name must contain 1-128 characters")
	}
	deviceCode, err := s.randomToken(32)
	if err != nil {
		return DeviceRequest{}, err
	}
	userCode, err := s.humanCode()
	if err != nil {
		return DeviceRequest{}, err
	}
	now := s.now().UTC()
	expires := now.Add(defaultDeviceTTL)
	_, err = s.db.ExecContext(ctx, `INSERT INTO websocket_device_authorizations (device_code_hash, user_code_hash, requested_client_name, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`, hashOpaque(deviceCode), s.hashUserCode(userCode), clientName, timestamp(expires), timestamp(now), timestamp(now))
	if err != nil {
		return DeviceRequest{}, fmt.Errorf("create websocket device authorization: %w", err)
	}
	return DeviceRequest{DeviceCode: deviceCode, UserCode: userCode, ExpiresAt: expires, PollInterval: defaultPollInterval}, nil
}

// PollDevice polls a device authorization and consumes an approved code when
// credentials are issued successfully.
func (s *Store) PollDevice(ctx context.Context, deviceCode string) (TokenPair, error) {
	hash := hashOpaque(strings.TrimSpace(deviceCode))
	if strings.TrimSpace(deviceCode) == "" {
		return TokenPair{}, ErrInvalidGrant
	}
	var pair TokenPair
	var outcome error
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var id, pollCount int64
		var state, clientName, userID, identifier, expiresRaw string
		var interval int64
		var lastPoll sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, state, requested_client_name, COALESCE(target_user_id, ''), websocket_identifier, poll_interval_seconds, poll_count, last_polled_at, expires_at FROM websocket_device_authorizations WHERE device_code_hash = ?`, hash).Scan(&id, &state, &clientName, &userID, &identifier, &interval, &pollCount, &lastPoll, &expiresRaw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidGrant
		}
		if err != nil {
			return fmt.Errorf("read device authorization: %w", err)
		}
		now := s.now().UTC()
		expires, err := parseTime(expiresRaw)
		if err != nil {
			return err
		}
		if state == "expired" || !expires.After(now) {
			if state == "pending" || state == "approved" {
				_, err = tx.ExecContext(ctx, `UPDATE websocket_device_authorizations SET state = 'expired', expired_at = ?, updated_at = ? WHERE id = ?`, timestamp(now), timestamp(now), id)
				if err != nil {
					return fmt.Errorf("expire device authorization: %w", err)
				}
			}
			outcome = ErrExpired
			return nil
		}
		if state == "consumed" {
			return ErrInvalidGrant
		}
		if lastPoll.Valid {
			last, err := parseTime(lastPoll.String)
			if err != nil {
				return err
			}
			if now.Before(last.Add(time.Duration(interval) * time.Second)) {
				_, err = tx.ExecContext(ctx, `UPDATE websocket_device_authorizations SET poll_interval_seconds = poll_interval_seconds + 5, poll_count = poll_count + 1, last_polled_at = ?, updated_at = ? WHERE id = ?`, timestamp(now), timestamp(now), id)
				if err != nil {
					return fmt.Errorf("record early device poll: %w", err)
				}
				outcome = ErrSlowDown
				return nil
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE websocket_device_authorizations SET poll_count = poll_count + 1, last_polled_at = ?, updated_at = ? WHERE id = ?`, timestamp(now), timestamp(now), id); err != nil {
			return fmt.Errorf("record device poll: %w", err)
		}
		if state == "pending" {
			outcome = ErrAuthorizationPending
			return nil
		}
		clientID, err := s.randomID("wsc_", 18)
		if err != nil {
			return err
		}
		refresh, err := s.randomToken(32)
		if err != nil {
			return err
		}
		refreshExpires := now.Add(refreshTTL)
		if _, err := tx.ExecContext(ctx, `INSERT INTO websocket_clients (client_id, canonical_user_id, websocket_identifier, client_name, refresh_token_hash, refresh_expires_at, created_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, clientID, userID, identifier, clientName, hashOpaque(refresh), timestamp(refreshExpires), timestamp(now), timestamp(now)); err != nil {
			return fmt.Errorf("create websocket client: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE websocket_device_authorizations SET state = 'consumed', consumed_at = ?, updated_at = ? WHERE id = ? AND state = 'approved'`, timestamp(now), timestamp(now), id)
		if err != nil {
			return fmt.Errorf("consume device authorization: %w", err)
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrInvalidGrant
		}
		access, accessExpires, err := s.issueAccess(identifier, clientID, 1)
		if err != nil {
			return fmt.Errorf("issue websocket access token: %w", err)
		}
		pair = TokenPair{AccessToken: access, RefreshToken: refresh, ExpiresAt: accessExpires, ClientID: clientID}
		return nil
	})
	if err == nil && outcome != nil {
		return TokenPair{}, outcome
	}
	return pair, err
}

// Refresh rotates a current token, or a previous token still inside its short
// grace window, and extends the sliding refresh expiry.
func (s *Store) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return TokenPair{}, ErrInvalidGrant
	}
	presented := hashOpaque(refreshToken)
	var pair TokenPair
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var clientID, identifier, expiresRaw string
		var version int64
		var current, previous []byte
		var grace, revoked sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT client_id, websocket_identifier, token_version, refresh_token_hash, previous_refresh_token_hash, previous_token_grace_expires_at, refresh_expires_at, revoked_at FROM websocket_clients WHERE refresh_token_hash = ? OR previous_refresh_token_hash = ?`, presented, presented).Scan(&clientID, &identifier, &version, &current, &previous, &grace, &expiresRaw, &revoked)
		if errors.Is(err, sql.ErrNoRows) || revoked.Valid {
			return ErrInvalidGrant
		}
		if err != nil {
			return fmt.Errorf("read websocket refresh token: %w", err)
		}
		now := s.now().UTC()
		expires, err := parseTime(expiresRaw)
		if err != nil {
			return err
		}
		if !expires.After(now) {
			return ErrExpired
		}
		isCurrent := subtle.ConstantTimeCompare(presented, current) == 1
		if !isCurrent {
			if subtle.ConstantTimeCompare(presented, previous) != 1 || !grace.Valid {
				return ErrInvalidGrant
			}
			graceExpiry, err := parseTime(grace.String)
			if err != nil || !graceExpiry.After(now) {
				return ErrInvalidGrant
			}
		}
		newRefresh, err := s.randomToken(32)
		if err != nil {
			return err
		}
		newVersion := version + 1
		newRefreshExpires := now.Add(refreshTTL)
		result, err := tx.ExecContext(ctx, `UPDATE websocket_clients SET refresh_token_hash = ?, previous_refresh_token_hash = ?, previous_token_grace_expires_at = ?, refresh_expires_at = ?, token_version = ?, last_used_at = ? WHERE client_id = ? AND token_version = ? AND revoked_at IS NULL`, hashOpaque(newRefresh), current, timestamp(now.Add(previousTokenGrace)), timestamp(newRefreshExpires), newVersion, timestamp(now), clientID, version)
		if err != nil {
			return fmt.Errorf("rotate websocket refresh token: %w", err)
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrInvalidGrant
		}
		access, accessExpires, err := s.issueAccess(identifier, clientID, newVersion)
		if err != nil {
			return err
		}
		pair = TokenPair{AccessToken: access, RefreshToken: newRefresh, ExpiresAt: accessExpires, ClientID: clientID}
		return nil
	})
	return pair, err
}

// RevokeRefresh revokes the client represented by a current or grace refresh token.
func (s *Store) RevokeRefresh(ctx context.Context, refreshToken string) error {
	_, err := s.RevokeRefreshClient(ctx, refreshToken)
	return err
}

// RevokeRefreshClient revokes a refresh token and returns its client identifier.
func (s *Store) RevokeRefreshClient(ctx context.Context, refreshToken string) (string, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "", ErrInvalidGrant
	}
	presented := hashOpaque(refreshToken)
	var clientID string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT client_id FROM websocket_clients WHERE revoked_at IS NULL AND (refresh_token_hash = ? OR previous_refresh_token_hash = ?)`, presented, presented).Scan(&clientID); errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidGrant
		} else if err != nil {
			return fmt.Errorf("read websocket refresh token for revocation: %w", err)
		}
		now := timestamp(s.now().UTC())
		result, err := tx.ExecContext(ctx, `UPDATE websocket_clients SET revoked_at = ?, token_version = token_version + 1, refresh_token_hash = NULL, previous_refresh_token_hash = NULL, previous_token_grace_expires_at = NULL, refresh_expires_at = NULL WHERE client_id = ? AND revoked_at IS NULL`, now, clientID)
		if err != nil {
			return fmt.Errorf("revoke websocket refresh token: %w", err)
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrInvalidGrant
		}
		return nil
	})
	return clientID, err
}

// ListClients lists all clients belonging to a canonical user, including revoked clients.
func (s *Store) ListClients(ctx context.Context, userID string) ([]Client, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT client_id, websocket_identifier, client_name, token_version, is_bootstrap, created_at, last_used_at, refresh_expires_at, revoked_at FROM websocket_clients WHERE canonical_user_id = ? ORDER BY created_at, client_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list websocket clients: %w", err)
	}
	defer rows.Close()
	var clients []Client
	for rows.Next() {
		var client Client
		var bootstrap int
		var created string
		var lastUsed, refreshExpires, revoked sql.NullString
		if err := rows.Scan(&client.ClientID, &client.WebSocketIdentifier, &client.ClientName, &client.TokenVersion, &bootstrap, &created, &lastUsed, &refreshExpires, &revoked); err != nil {
			return nil, fmt.Errorf("scan websocket client: %w", err)
		}
		client.IsBootstrap = bootstrap == 1
		client.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		client.LastUsedAt, err = optionalTime(lastUsed)
		if err != nil {
			return nil, err
		}
		client.RefreshExpiresAt, err = optionalTime(refreshExpires)
		if err != nil {
			return nil, err
		}
		client.RevokedAt, err = optionalTime(revoked)
		if err != nil {
			return nil, err
		}
		clients = append(clients, client)
	}
	return clients, rows.Err()
}

// RevokeClient revokes one client only when it belongs to userID.
func (s *Store) RevokeClient(ctx context.Context, userID, clientID string) error {
	now := timestamp(s.now().UTC())
	result, err := s.db.ExecContext(ctx, `UPDATE websocket_clients SET revoked_at = ?, token_version = token_version + 1, refresh_token_hash = NULL, previous_refresh_token_hash = NULL, previous_token_grace_expires_at = NULL, refresh_expires_at = NULL WHERE canonical_user_id = ? AND client_id = ? AND revoked_at IS NULL`, now, userID, clientID)
	if err != nil {
		return fmt.Errorf("revoke websocket client: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrInvalidGrant
	}
	return nil
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := io.ReadFull(s.random, value); err != nil {
		return "", fmt.Errorf("generate websocket authorization secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *Store) randomID(prefix string, bytes int) (string, error) {
	value, err := s.randomToken(bytes)
	if err != nil {
		return "", err
	}
	return prefix + value, nil
}

func (s *Store) humanCode() (string, error) {
	const alphabet = "23456789ABCDEFGH"
	raw := make([]byte, 8)
	if _, err := io.ReadFull(s.random, raw); err != nil {
		return "", fmt.Errorf("generate websocket user code: %w", err)
	}
	for i := range raw {
		raw[i] = alphabet[int(raw[i])&15]
	}
	return string(raw[:4]) + "-" + string(raw[4:]), nil
}

func hashOpaque(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func (s *Store) hashUserCode(value string) []byte {
	mac := hmac.New(sha256.New, s.signingKey)
	_, _ = mac.Write([]byte(normalizeUserCode(value)))
	return mac.Sum(nil)
}

func normalizeUserCode(value string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), "-", ""))
}

func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse websocket authorization timestamp: %w", err)
	}
	return parsed, nil
}

func optionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
