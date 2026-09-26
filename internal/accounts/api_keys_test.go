package accounts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestAPIKeyLifecycle(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	owner, err := s.EnsureAccount(ctx, "discord", "123", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAPIKey(ctx, "missing"); err == nil {
		t.Fatal("created a new account for a key")
	}
	id, token, err := s.CreateAPIKey(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	otherID, otherToken, err := s.CreateAPIKey(ctx, owner)
	if err != nil || otherID == id || otherToken == token {
		t.Fatalf("second independent key id=%q err=%v", otherID, err)
	}
	if !regexp.MustCompile(`^osk_[0-9a-f]{32}_[0-9a-f]{64}$`).MatchString(token) || !strings.HasPrefix(token, "osk_"+id+"_") {
		t.Fatal("unexpected token format")
	}
	var hash []byte
	if err := s.db.SQL().QueryRow(`SELECT secret_hash FROM api_keys WHERE key_id = ?`, id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(strings.Split(token, "_")[2]))
	if string(hash) != string(want[:]) {
		t.Fatal("stored hash differs from token secret")
	}
	principal, err := s.AuthenticateAPIKey(ctx, token)
	if err != nil || !principal.Valid() || !principal.Authenticated() || principal.CanonicalUserID != owner || principal.Gateway != "openai" || principal.ExternalID != id || principal.Assurance != identity.AssuranceAPIKey {
		t.Fatalf("authenticate principal=%+v err=%v", principal, err)
	}
	for _, invalid := range []string{"", "osk_" + id + "_" + strings.Repeat("0", 64), token + "_extra", "osk_bad_abc", "osk_" + strings.ToUpper(id) + "_" + strings.Split(token, "_")[2]} {
		if _, err := s.AuthenticateAPIKey(ctx, invalid); !errors.Is(err, ErrInvalidAPIKey) {
			t.Fatalf("invalid key accepted: %v", err)
		}
	}
	if err := s.RevokeAPIKey(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateAPIKey(ctx, token); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("revoked key accepted: %v", err)
	}
	if _, err := s.ResolvePrincipal(principal); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("revoked principal resolved: %v", err)
	}
	if err := s.RunAuthenticatedCanonicalMutation(principal, func(string) error { return nil }); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("revoked principal mutated account: %v", err)
	}
	if err := s.RevokeAPIKey(ctx, id); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("second revoke: %v", err)
	}
	if p, err := s.AuthenticateAPIKey(ctx, otherToken); err != nil || p.CanonicalUserID != owner {
		t.Fatalf("other key lost: %+v %v", p, err)
	}
	if resolved, found, err := s.ResolveAccount("discord", "123"); err != nil || !found || resolved != owner {
		t.Fatalf("canonical account removed: %q %t %v", resolved, found, err)
	}
}

func TestAPIKeyRequiresLinkedIdentity(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	owner, _ := s.EnsureAccount(ctx, "discord", "123", "Alice")
	id, token, err := s.CreateAPIKey(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.SQL().Exec(`DELETE FROM linked_accounts WHERE gateway = 'openai' AND identifier = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateAPIKey(ctx, token); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("orphan key authenticated: %v", err)
	}
	if _, err := s.db.SQL().Exec(`DELETE FROM account_users WHERE canonical_user_id = ?`, owner); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SQL().QueryRow(`SELECT key_id FROM api_keys WHERE key_id = ?`, id).Scan(new(string)); err != sql.ErrNoRows {
		t.Fatalf("deleted owner retained key: %v", err)
	}
}

func TestAPIKeyOrphanLinkCannotAuthorizeQueuedPrincipal(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	owner, err := s.EnsureAccount(ctx, "discord", "123", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	id, token, err := s.CreateAPIKey(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := s.AuthenticateAPIKey(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an interrupted or inconsistent revocation leaving the link behind.
	if _, err := s.db.SQL().Exec(`DELETE FROM api_keys WHERE key_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ResolveAccount("openai", id); err != nil || found {
		t.Fatalf("orphan link resolved: found=%t err=%v", found, err)
	}
	if _, err := s.ResolvePrincipal(principal); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("orphan principal resolved: %v", err)
	}
	called := false
	if err := s.RunAuthenticatedCanonicalMutation(principal, func(string) error { called = true; return nil }); !errors.Is(err, ErrPrincipalMismatch) || called {
		t.Fatalf("orphan principal mutated account: called=%t err=%v", called, err)
	}
	if _, err := s.CreateChallenge(ctx, principal, "test"); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("orphan principal created challenge: %v", err)
	}
	if _, err := s.EnsureAccount(ctx, "openai", id, "forged"); err == nil {
		t.Fatal("created an API key identity without a credential")
	}
}

func TestAPIKeysFollowAccountMerge(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	winner, err := s.EnsureAccount(ctx, "discord", "123", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	loser, err := s.EnsureAccount(ctx, "imessage", "+15551234567", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	_, winnerToken, err := s.CreateAPIKey(ctx, winner)
	if err != nil {
		t.Fatal(err)
	}
	_, loserToken, err := s.CreateAPIKey(ctx, loser)
	if err != nil {
		t.Fatal(err)
	}
	connectTestAccounts(t, s,
		identity.Principal{CanonicalUserID: winner, Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway},
		identity.Principal{CanonicalUserID: loser, Gateway: "imessage", ExternalID: "+15551234567", Assurance: identity.AssuranceBlueBubblesWebhook})
	for _, token := range []string{winnerToken, loserToken} {
		p, err := s.AuthenticateAPIKey(ctx, token)
		if err != nil || p.CanonicalUserID != winner {
			t.Fatalf("merged key owner=%q err=%v", p.CanonicalUserID, err)
		}
	}
}

func TestAPIKeyMergeRollbackKeepsOwnersAndChallenge(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	winner, err := s.EnsureAccount(ctx, "discord", "123", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	loser, err := s.EnsureAccount(ctx, "imessage", "+15551234567", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateAPIKey(ctx, loser)
	if err != nil {
		t.Fatal(err)
	}
	winnerPrincipal := identity.Principal{CanonicalUserID: winner, Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
	loserPrincipal := identity.Principal{CanonicalUserID: loser, Gateway: "imessage", ExternalID: "+15551234567", Assurance: identity.AssuranceBlueBubblesWebhook}
	challenge, err := s.CreateChallenge(ctx, winnerPrincipal, "merge-key-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.SQL().Exec(`CREATE TRIGGER fail_key_merge BEFORE DELETE ON account_users BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmChallenge(ctx, loserPrincipal, challenge.Code, "merge-key-test"); err == nil {
		t.Fatal("merge should fail at final deletion")
	}
	if p, err := s.AuthenticateAPIKey(ctx, token); err != nil || p.CanonicalUserID != loser {
		t.Fatalf("failed merge moved key: owner=%q err=%v", p.CanonicalUserID, err)
	}
	if _, err := s.db.SQL().Exec(`DROP TRIGGER fail_key_merge`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmChallenge(ctx, loserPrincipal, challenge.Code, "merge-key-test"); err != nil {
		t.Fatalf("failed merge consumed challenge: %v", err)
	}
	if p, err := s.AuthenticateAPIKey(ctx, token); err != nil || p.CanonicalUserID != winner {
		t.Fatalf("committed merge did not move key: owner=%q err=%v", p.CanonicalUserID, err)
	}
}
