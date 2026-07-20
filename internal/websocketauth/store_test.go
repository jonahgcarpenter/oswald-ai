package websocketauth

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
)

const testSigningKey = "0123456789abcdef0123456789abcdef"

type incrementReader struct{ next byte }

func (r *incrementReader) Read(p []byte) (int, error) {
	for i := range p {
		r.next++
		p[i] = r.next
	}
	return len(p), nil
}

type testStore struct {
	store *Store
	db    *database.DB
	now   *time.Time
}

func newTestStore(t *testing.T) testStore {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	store, err := New(db.SQL(), testSigningKey, 15*time.Minute, WithClock(func() time.Time { return now }), WithRandom(&incrementReader{}))
	if err != nil {
		t.Fatal(err)
	}
	return testStore{store: store, db: db, now: &now}
}

func addUser(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, '2026-07-19T12:00:00Z', '2026-07-19T12:00:00Z')`, userID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeviceApprovalJWTAndSecretStorage(t *testing.T) {
	test := newTestStore(t)
	ctx := context.Background()
	addUser(t, test.db.SQL(), "user-a")

	request, err := test.store.RequestDevice(ctx, "Desktop")
	if err != nil {
		t.Fatal(err)
	}
	if request.DeviceCode == "" || len(request.UserCode) != 9 || request.PollInterval != 5*time.Second || !request.ExpiresAt.Equal(test.now.Add(10*time.Minute)) {
		t.Fatalf("unexpected device request: %+v", request)
	}
	var storedDevice, storedUser []byte
	if err := test.db.SQL().QueryRow(`SELECT device_code_hash, user_code_hash FROM websocket_device_authorizations`).Scan(&storedDevice, &storedUser); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(storedDevice, []byte(request.DeviceCode)) || bytes.Contains(storedUser, []byte(normalizeUserCode(request.UserCode))) || len(storedDevice) != 32 || len(storedUser) != 32 {
		t.Fatal("raw authorization secrets were stored")
	}
	if _, err := test.store.PollDevice(ctx, request.DeviceCode); !errors.Is(err, ErrAuthorizationPending) {
		t.Fatalf("first poll error = %v", err)
	}
	if _, err := test.store.PollDevice(ctx, request.DeviceCode); !errors.Is(err, ErrSlowDown) {
		t.Fatalf("early poll error = %v", err)
	}

	if _, err := test.store.ApproveForUser(ctx, "user-a", "Alice", "WRONG-CODE"); !errors.Is(err, ErrInvalidUserCode) {
		t.Fatalf("wrong approval error = %v", err)
	}
	var identities int
	if err := test.db.SQL().QueryRow(`SELECT COUNT(*) FROM linked_accounts WHERE canonical_user_id = 'user-a' AND gateway = 'websocket'`).Scan(&identities); err != nil || identities != 0 {
		t.Fatalf("failed approval leaked identity: count=%d err=%v", identities, err)
	}
	identifier, err := test.store.ApproveForUser(ctx, "user-a", "Alice", request.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	*test.now = test.now.Add(10 * time.Second)
	pair, err := test.store.PollDevice(ctx, request.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" || pair.ClientID == "" {
		t.Fatalf("incomplete token pair: %+v", pair)
	}
	var rawRefreshMatches int
	if err := test.db.SQL().QueryRow(`SELECT COUNT(*) FROM websocket_clients WHERE CAST(refresh_token_hash AS TEXT) = ?`, pair.RefreshToken).Scan(&rawRefreshMatches); err != nil || rawRefreshMatches != 0 {
		t.Fatalf("raw refresh token persisted: count=%d err=%v", rawRefreshMatches, err)
	}

	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(pair.AccessToken, claims); err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != identifier || claims["cid"] != pair.ClientID || claims["ver"] != float64(1) {
		t.Fatalf("unexpected access claims: %#v", claims)
	}
	httpRequest, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/ws", nil)
	httpRequest.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	identity, err := test.store.Authenticate(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	if identity.UserID != "user-a" || identity.Subject != identifier || identity.ClientID != pair.ClientID || identity.TokenVersion != 1 {
		t.Fatalf("unexpected authenticated client: %+v", identity)
	}
	if _, err := test.store.PollDevice(ctx, request.DeviceCode); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("consumed poll error = %v", err)
	}
}

func TestRefreshRotationGraceExpiryAndRevocation(t *testing.T) {
	test := newTestStore(t)
	ctx := context.Background()
	addUser(t, test.db.SQL(), "user-a")
	request, _ := test.store.RequestDevice(ctx, "Phone")
	if _, err := test.store.ApproveForUser(ctx, "user-a", "Alice", request.UserCode); err != nil {
		t.Fatal(err)
	}
	original, err := test.store.PollDevice(ctx, request.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := test.store.Refresh(ctx, original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := test.store.VerifyAccess(ctx, original.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old access token error = %v", err)
	}
	graceRotation, err := test.store.Refresh(ctx, original.RefreshToken)
	if err != nil {
		t.Fatalf("previous token in grace: %v", err)
	}
	if graceRotation.RefreshToken == rotated.RefreshToken {
		t.Fatal("refresh rotation reused a token")
	}
	if _, err := test.store.Refresh(ctx, original.RefreshToken); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("superseded previous token error = %v", err)
	}

	*test.now = test.now.Add(31 * time.Second)
	if _, err := test.store.Refresh(ctx, rotated.RefreshToken); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("expired grace token error = %v", err)
	}
	latest, err := test.store.Refresh(ctx, graceRotation.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := test.store.ListClients(ctx, "user-a")
	if err != nil || len(clients) != 1 || clients[0].TokenVersion != 4 {
		t.Fatalf("clients=%+v err=%v", clients, err)
	}
	if err := test.store.RevokeRefresh(ctx, latest.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := test.store.VerifyAccess(ctx, latest.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked access error = %v", err)
	}
	if err := test.store.RevokeClient(ctx, "other-user", latest.ClientID); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("cross-user revoke error = %v", err)
	}

	secondRequest, _ := test.store.RequestDevice(ctx, "Tablet")
	if _, err := test.store.ApproveForUser(ctx, "user-a", "Alice", secondRequest.UserCode); err != nil {
		t.Fatal(err)
	}
	second, err := test.store.PollDevice(ctx, secondRequest.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	*test.now = test.now.Add(refreshTTL + time.Second)
	if _, err := test.store.Refresh(ctx, second.RefreshToken); !errors.Is(err, ErrExpired) {
		t.Fatalf("sliding expiry error = %v", err)
	}
	if err := test.store.RevokeClient(ctx, "user-a", second.ClientID); err != nil {
		t.Fatalf("revoke client: %v", err)
	}
	if _, err := test.store.VerifyAccess(ctx, second.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("client-revoked access error = %v", err)
	}
}

func TestApprovalIsolationAndNewUser(t *testing.T) {
	test := newTestStore(t)
	ctx := context.Background()
	one, _ := test.store.RequestDevice(ctx, "One")
	two, _ := test.store.RequestDevice(ctx, "Two")
	userID, err := test.store.ApproveNewUser(ctx, one.UserCode, "New User", false)
	if err != nil {
		t.Fatal(err)
	}
	var targetOne, stateOne, stateTwo string
	if err := test.db.SQL().QueryRow(`SELECT target_user_id, state FROM websocket_device_authorizations WHERE device_code_hash = ?`, hashOpaque(one.DeviceCode)).Scan(&targetOne, &stateOne); err != nil {
		t.Fatal(err)
	}
	if err := test.db.SQL().QueryRow(`SELECT state FROM websocket_device_authorizations WHERE device_code_hash = ?`, hashOpaque(two.DeviceCode)).Scan(&stateTwo); err != nil {
		t.Fatal(err)
	}
	if targetOne != userID || stateOne != "approved" || stateTwo != "pending" {
		t.Fatalf("approval isolation failed: target=%q states=%q/%q", targetOne, stateOne, stateTwo)
	}
	var admin, verified int
	if err := test.db.SQL().QueryRow(`SELECT u.is_admin, l.verified FROM account_users u JOIN linked_accounts l ON l.canonical_user_id = u.canonical_user_id WHERE u.canonical_user_id = ? AND l.gateway = 'websocket'`, userID).Scan(&admin, &verified); err != nil || admin != 0 || verified != 1 {
		t.Fatalf("new user state admin=%d verified=%d err=%v", admin, verified, err)
	}
}

func TestDeviceCodeExpiresDurably(t *testing.T) {
	test := newTestStore(t)
	request, err := test.store.RequestDevice(context.Background(), "Expired Device")
	if err != nil {
		t.Fatal(err)
	}
	*test.now = test.now.Add(defaultDeviceTTL)
	if _, err := test.store.PollDevice(context.Background(), request.DeviceCode); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired device poll error = %v", err)
	}
	var state string
	var expiredAt sql.NullString
	if err := test.db.SQL().QueryRow(`SELECT state, expired_at FROM websocket_device_authorizations WHERE device_code_hash = ?`, hashOpaque(request.DeviceCode)).Scan(&state, &expiredAt); err != nil {
		t.Fatal(err)
	}
	if state != "expired" || !expiredAt.Valid {
		t.Fatalf("expired device state=%q expired_at=%v", state, expiredAt)
	}
}
