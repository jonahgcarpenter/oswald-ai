package websocketauth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	accessAudience = "oswald-websocket"
	maxTokenChars  = 8192
)

type accessClaims struct {
	ClientID     string `json:"cid"`
	TokenVersion int64  `json:"ver"`
	jwt.RegisteredClaims
}

func decodeSigningKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("websocket auth signing key is required")
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil && len(decoded) >= 32 {
		return decoded, nil
	}
	if len(value) < 32 {
		return nil, fmt.Errorf("websocket auth signing key must contain at least 32 bytes")
	}
	return []byte(value), nil
}

func (s *Store) issueAccess(subject, clientID string, version int64) (string, time.Time, error) {
	now := s.now().UTC().Truncate(time.Second)
	expires := now.Add(s.accessTTL)
	claims := accessClaims{
		ClientID:     clientID,
		TokenVersion: version,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			Audience:  jwt.ClaimStrings{accessAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	}
	value, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.signingKey)
	return value, expires, err
}

// Authenticate verifies exactly one HTTP bearer token and its active durable client.
func (s *Store) Authenticate(r *http.Request) (AuthenticatedClient, error) {
	if r == nil {
		return AuthenticatedClient{}, ErrUnauthorized
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return AuthenticatedClient{}, ErrUnauthorized
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return AuthenticatedClient{}, ErrUnauthorized
	}
	return s.VerifyAccess(r.Context(), parts[1])
}

// VerifyAccess verifies an access JWT and confirms its subject and token version
// against the active durable client record.
func (s *Store) VerifyAccess(ctx context.Context, value string) (AuthenticatedClient, error) {
	if s == nil || strings.TrimSpace(value) == "" || len(value) > maxTokenChars {
		return AuthenticatedClient{}, ErrUnauthorized
	}
	claims := &accessClaims{}
	token, err := jwt.ParseWithClaims(value, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, ErrUnauthorized
		}
		return s.signingKey, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithAudience(accessAudience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(s.now))
	if err != nil || !token.Valid || claims.IssuedAt == nil || claims.ExpiresAt == nil || strings.TrimSpace(claims.Subject) == "" || strings.TrimSpace(claims.ClientID) == "" || claims.TokenVersion <= 0 {
		return AuthenticatedClient{}, ErrUnauthorized
	}
	now := s.now().UTC()
	if claims.IssuedAt.Time.After(now) || !claims.ExpiresAt.Time.After(now) || !claims.ExpiresAt.Time.After(claims.IssuedAt.Time) || claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time) > s.accessTTL {
		return AuthenticatedClient{}, ErrUnauthorized
	}
	var client AuthenticatedClient
	var revoked sql.NullString
	var bootstrap int
	err = s.db.QueryRowContext(ctx, `SELECT clients.canonical_user_id, clients.websocket_identifier, accounts.display_name, clients.client_name, clients.token_version, clients.is_bootstrap, clients.revoked_at FROM websocket_clients clients JOIN linked_accounts accounts ON accounts.gateway = 'websocket' AND accounts.identifier = clients.websocket_identifier AND accounts.canonical_user_id = clients.canonical_user_id WHERE clients.client_id = ?`, claims.ClientID).Scan(&client.UserID, &client.Subject, &client.DisplayName, &client.ClientName, &client.TokenVersion, &bootstrap, &revoked)
	if err != nil || revoked.Valid || client.Subject != claims.Subject || client.TokenVersion != claims.TokenVersion {
		return AuthenticatedClient{}, ErrUnauthorized
	}
	client.ClientID = claims.ClientID
	client.ExpiresAt = claims.ExpiresAt.Time
	client.IsBootstrap = bootstrap == 1
	return client, nil
}
