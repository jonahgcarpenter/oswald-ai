package websocket

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSigningKey = "0123456789abcdef0123456789abcdef"

func TestAuthenticatorIssuesAndVerifiesToken(t *testing.T) {
	now := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	auth, err := NewAuthenticator(testSigningKey, 15*time.Minute)
	if err != nil {
		t.Fatalf("new authenticator: %v", err)
	}
	auth.now = func() time.Time { return now }
	token, err := auth.Issue(" alice ", " Alice ", 10*time.Minute)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://example/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	identity, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if identity.Subject != "alice" || identity.DisplayName != "Alice" || !identity.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestAuthenticatorRejectsInvalidRequestsAndClaims(t *testing.T) {
	now := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	auth, err := NewAuthenticator(testSigningKey, 15*time.Minute)
	if err != nil {
		t.Fatalf("new authenticator: %v", err)
	}
	auth.now = func() time.Time { return now }

	validClaims := tokenClaims{RegisteredClaims: jwt.RegisteredClaims{
		Subject:   "alice",
		Audience:  jwt.ClaimStrings{tokenAudience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
	}}
	tests := []struct {
		name   string
		claims tokenClaims
		method jwt.SigningMethod
		key    interface{}
	}{
		{name: "wrong audience", claims: withAudience(validClaims, "other"), method: jwt.SigningMethodHS256, key: []byte(testSigningKey)},
		{name: "missing subject", claims: withSubject(validClaims, ""), method: jwt.SigningMethodHS256, key: []byte(testSigningKey)},
		{name: "missing issued at", claims: withTimes(validClaims, time.Time{}, now.Add(10*time.Minute)), method: jwt.SigningMethodHS256, key: []byte(testSigningKey)},
		{name: "future issued at", claims: withTimes(validClaims, now.Add(time.Minute), now.Add(10*time.Minute)), method: jwt.SigningMethodHS256, key: []byte(testSigningKey)},
		{name: "expired", claims: withTimes(validClaims, now.Add(-20*time.Minute), now.Add(-10*time.Minute)), method: jwt.SigningMethodHS256, key: []byte(testSigningKey)},
		{name: "excessive lifetime", claims: withTimes(validClaims, now, now.Add(20*time.Minute)), method: jwt.SigningMethodHS256, key: []byte(testSigningKey)},
		{name: "bad signature", claims: validClaims, method: jwt.SigningMethodHS256, key: []byte("abcdef0123456789abcdef0123456789")},
		{name: "unsupported algorithm", claims: validClaims, method: jwt.SigningMethodNone, key: jwt.UnsafeAllowNoneSignatureType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := jwt.NewWithClaims(tt.method, tt.claims).SignedString(tt.key)
			if err != nil {
				t.Fatalf("sign token: %v", err)
			}
			if _, err := auth.Verify(value); err == nil {
				t.Fatal("expected verification failure")
			}
		})
	}

	for _, header := range []string{"", "Basic abc", "Bearer", "Bearer a b"} {
		req, _ := http.NewRequest(http.MethodGet, "http://example/ws", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		if _, err := auth.Authenticate(req); err == nil {
			t.Fatalf("header %q unexpectedly authenticated", header)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example/ws", nil)
	req.Header.Add("Authorization", "Bearer one")
	req.Header.Add("Authorization", "Bearer two")
	if _, err := auth.Authenticate(req); err == nil {
		t.Fatal("multiple authorization headers unexpectedly authenticated")
	}
}

func TestNewAuthenticatorValidatesConfiguration(t *testing.T) {
	if _, err := NewAuthenticator("short", time.Minute); err == nil {
		t.Fatal("expected short key error")
	}
	if _, err := NewAuthenticator(testSigningKey, 0); err == nil {
		t.Fatal("expected max TTL error")
	}
}

func TestAuthenticatorRejectsOversizedValues(t *testing.T) {
	auth, err := NewAuthenticator(testSigningKey, 15*time.Minute)
	if err != nil {
		t.Fatalf("new authenticator: %v", err)
	}
	if _, err := auth.Verify(strings.Repeat("x", maxTokenChars+1)); err == nil {
		t.Fatal("expected oversized token error")
	}
	if _, err := auth.Issue(strings.Repeat("x", maxTokenSubjectChars+1), "", time.Minute); err == nil {
		t.Fatal("expected oversized subject error")
	}
}

func withAudience(claims tokenClaims, audience string) tokenClaims {
	claims.Audience = jwt.ClaimStrings{audience}
	return claims
}

func withSubject(claims tokenClaims, subject string) tokenClaims {
	claims.Subject = subject
	return claims
}

func withTimes(claims tokenClaims, issuedAt, expiresAt time.Time) tokenClaims {
	if issuedAt.IsZero() {
		claims.IssuedAt = nil
	} else {
		claims.IssuedAt = jwt.NewNumericDate(issuedAt)
	}
	claims.ExpiresAt = jwt.NewNumericDate(expiresAt)
	return claims
}
