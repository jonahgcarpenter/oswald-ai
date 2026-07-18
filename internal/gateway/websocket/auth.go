package websocket

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	tokenAudience        = "oswald-websocket"
	maxTokenChars        = 8192
	maxTokenSubjectChars = 256
	maxDisplayNameChars  = 256
)

var errUnauthorized = errors.New("unauthorized websocket request")

// AuthenticatedIdentity is the identity established by a signed WebSocket token.
type AuthenticatedIdentity struct {
	Subject     string
	DisplayName string
	ExpiresAt   time.Time
}

type tokenClaims struct {
	DisplayName string `json:"name,omitempty"`
	jwt.RegisteredClaims
}

// Authenticator verifies and issues short-lived subject-bound WebSocket tokens.
type Authenticator struct {
	key    []byte
	maxTTL time.Duration
	now    func() time.Time
}

// NewAuthenticator creates an authenticator from raw or base64-encoded key material.
func NewAuthenticator(signingKey string, maxTTL time.Duration) (*Authenticator, error) {
	key, err := decodeSigningKey(signingKey)
	if err != nil {
		return nil, err
	}
	if maxTTL <= 0 {
		return nil, fmt.Errorf("websocket token max TTL must be positive")
	}
	return &Authenticator{key: key, maxTTL: maxTTL, now: time.Now}, nil
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

// Authenticate verifies the request bearer token and returns its bound identity.
func (a *Authenticator) Authenticate(r *http.Request) (AuthenticatedIdentity, error) {
	if a == nil || r == nil {
		return AuthenticatedIdentity{}, errUnauthorized
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return AuthenticatedIdentity{}, errUnauthorized
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return AuthenticatedIdentity{}, errUnauthorized
	}
	return a.Verify(parts[1])
}

// Verify validates one signed token and returns its subject-bound identity.
func (a *Authenticator) Verify(value string) (AuthenticatedIdentity, error) {
	if a == nil || strings.TrimSpace(value) == "" || len(value) > maxTokenChars {
		return AuthenticatedIdentity{}, errUnauthorized
	}
	claims := &tokenClaims{}
	token, err := jwt.ParseWithClaims(value, claims, func(token *jwt.Token) (interface{}, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errUnauthorized
		}
		return a.key, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithAudience(tokenAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(a.now),
	)
	if err != nil || !token.Valid || claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return AuthenticatedIdentity{}, errUnauthorized
	}
	subject := strings.TrimSpace(claims.Subject)
	displayName := strings.TrimSpace(claims.DisplayName)
	now := a.now()
	if subject == "" || len(subject) > maxTokenSubjectChars || len(displayName) > maxDisplayNameChars || claims.IssuedAt.Time.After(now) || !claims.ExpiresAt.Time.After(now) || !claims.ExpiresAt.Time.After(claims.IssuedAt.Time) || claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time) > a.maxTTL {
		return AuthenticatedIdentity{}, errUnauthorized
	}
	return AuthenticatedIdentity{Subject: subject, DisplayName: displayName, ExpiresAt: claims.ExpiresAt.Time}, nil
}

// Issue signs a subject-bound token using this authenticator's configured key.
func (a *Authenticator) Issue(subject, displayName string, ttl time.Duration) (string, error) {
	if a == nil {
		return "", fmt.Errorf("websocket authenticator is not configured")
	}
	subject = strings.TrimSpace(subject)
	if subject == "" || len(subject) > maxTokenSubjectChars {
		return "", fmt.Errorf("websocket token subject must contain 1-%d characters", maxTokenSubjectChars)
	}
	displayName = strings.TrimSpace(displayName)
	if len(displayName) > maxDisplayNameChars {
		return "", fmt.Errorf("websocket token display name is too long")
	}
	if ttl <= 0 || ttl > a.maxTTL {
		return "", fmt.Errorf("websocket token TTL must be positive and no greater than %s", a.maxTTL)
	}
	now := a.now().UTC().Truncate(time.Second)
	claims := tokenClaims{
		DisplayName: displayName,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			Audience:  jwt.ClaimStrings{tokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.key)
}
