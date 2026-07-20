// Package websocketauth implements durable WebSocket device authorization,
// refresh-token rotation, and fresh-install bootstrap credentials.
package websocketauth

import (
	"errors"
	"io"
	"time"
)

const (
	defaultDeviceTTL    = 10 * time.Minute
	defaultPollInterval = 5 * time.Second
	refreshTTL          = 90 * 24 * time.Hour
	previousTokenGrace  = 30 * time.Second
	maximumAccessTTL    = 15 * time.Minute
)

var (
	// ErrAuthorizationPending indicates that a device request awaits approval.
	ErrAuthorizationPending = errors.New("authorization_pending")
	// ErrSlowDown indicates that a device polled sooner than its allowed interval.
	ErrSlowDown = errors.New("slow_down")
	// ErrExpired indicates that a device code or refresh token has expired.
	ErrExpired = errors.New("expired")
	// ErrInvalidGrant indicates an invalid, consumed, or revoked credential.
	ErrInvalidGrant = errors.New("invalid_grant")
	// ErrInvalidUserCode indicates an unknown or no-longer-pending human code.
	ErrInvalidUserCode = errors.New("invalid_user_code")
	// ErrUnauthorized indicates invalid HTTP or JWT authentication.
	ErrUnauthorized = errors.New("unauthorized websocket request")
	// ErrBootstrapUnavailable indicates that bootstrap is absent, complete, or mismatched.
	ErrBootstrapUnavailable = errors.New("websocket bootstrap unavailable")
)

// DeviceRequest contains the one-time secrets and polling parameters returned
// to a newly requesting client.
type DeviceRequest struct {
	DeviceCode   string
	UserCode     string
	ExpiresAt    time.Time
	PollInterval time.Duration
}

// TokenPair contains a short-lived access JWT and rotating opaque refresh token.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	ClientID     string
}

// AuthenticatedClient is the active durable client represented by an access JWT.
type AuthenticatedClient struct {
	UserID       string
	Subject      string
	DisplayName  string
	ClientID     string
	ClientName   string
	TokenVersion int64
	ExpiresAt    time.Time
	IsBootstrap  bool
}

// Client describes a durable client without exposing token hashes.
type Client struct {
	ClientID            string
	WebSocketIdentifier string
	ClientName          string
	TokenVersion        int64
	IsBootstrap         bool
	CreatedAt           time.Time
	LastUsedAt          *time.Time
	RefreshExpiresAt    *time.Time
	RevokedAt           *time.Time
}

// BootstrapCredentials identifies the temporary fresh-install administrator.
type BootstrapCredentials struct {
	AccessToken         string
	ExpiresAt           time.Time
	DefaultUserID       string
	ClientID            string
	WebSocketIdentifier string
}

// BootstrapCompletion identifies the temporary identity and client that callers
// must disconnect after bootstrap completion.
type BootstrapCompletion struct {
	ClientID      string
	DefaultUserID string
}

// Option customizes deterministic service dependencies.
type Option func(*Store) error

// WithClock overrides the service clock.
func WithClock(now func() time.Time) Option {
	return func(s *Store) error {
		if now == nil {
			return errors.New("websocket authorization clock is nil")
		}
		s.now = now
		return nil
	}
}

// WithRandom overrides the cryptographically secure random source.
func WithRandom(random io.Reader) Option {
	return func(s *Store) error {
		if random == nil {
			return errors.New("websocket authorization random source is nil")
		}
		s.random = random
		return nil
	}
}
