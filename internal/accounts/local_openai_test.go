package accounts

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestLocalOpenAIAdminPersistsAndRechecksOwnership(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := NewService(path, nil, nil, config.NewLogger(config.LevelError))
	if _, err := s.LocalOpenAIPrincipal(ctx); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("unprovisioned local principal: %v", err)
	}
	if err := s.EnsureLocalOpenAIAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := s.LocalOpenAIPrincipal(ctx)
	if err != nil || !p.Authenticated() || p.Assurance != identity.AssuranceLocalLoopback {
		t.Fatalf("local principal=%+v err=%v", p, err)
	}
	if admin, err := s.IsAdminPrincipal(p); err != nil || !admin {
		t.Fatalf("local admin=%t err=%v", admin, err)
	}
	stale := p
	stale.CanonicalUserID = "stale-owner"
	if owner, err := s.ResolvePrincipal(stale); err != nil || owner != p.CanonicalUserID {
		t.Fatalf("stale principal owner=%q err=%v", owner, err)
	}
	if err := s.EnsureLocalOpenAIAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = NewService(path, nil, nil, config.NewLogger(config.LevelError))
	t.Cleanup(func() { _ = s.Close() })
	if err := s.EnsureLocalOpenAIAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := s.LocalOpenAIPrincipal(ctx)
	if err != nil || again != p {
		t.Fatalf("reopened principal=%+v err=%v", again, err)
	}
	if err := s.RunAuthenticatedCanonicalMutation(p, func(owner string) error {
		if owner != p.CanonicalUserID {
			t.Fatalf("mutated wrong user: %s", owner)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.SQL().Exec(`UPDATE account_users SET is_admin = 0, is_banned = 1 WHERE canonical_user_id = ?`, p.CanonicalUserID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureLocalOpenAIAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	if admin, err := s.IsAdminPrincipal(p); err != nil || admin {
		t.Fatalf("demotion overwritten: admin=%t err=%v", admin, err)
	}
	if banned, _, err := s.BanStatus(p.CanonicalUserID); err != nil || !banned {
		t.Fatalf("ban overwritten: banned=%t err=%v", banned, err)
	}
	if _, err := s.db.SQL().Exec(`UPDATE account_users SET lifecycle_state = 'erasing' WHERE canonical_user_id = ?`, p.CanonicalUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LocalOpenAIPrincipal(ctx); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("inactive local principal still resolves: %v", err)
	}
	if err := s.EnsureLocalOpenAIAdmin(ctx); err == nil {
		t.Fatal("inactive account was silently reprovisioned")
	}
	if _, err := s.db.SQL().Exec(`UPDATE account_users SET lifecycle_state = 'active' WHERE canonical_user_id = ?`, p.CanonicalUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.SQL().Exec(`DELETE FROM linked_accounts WHERE gateway = 'openai' AND identifier = 'local'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolvePrincipal(p); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("deleted identity still resolves: %v", err)
	}
	if _, err := s.LocalOpenAIPrincipal(ctx); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("deleted local principal still resolves: %v", err)
	}
	if err := s.EnsureLocalOpenAIAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.LocalOpenAIPrincipal(ctx)
	if err != nil || replacement.CanonicalUserID == p.CanonicalUserID {
		t.Fatalf("replacement principal=%+v err=%v", replacement, err)
	}
	if admin, err := s.IsAdminPrincipal(replacement); err != nil || !admin {
		t.Fatalf("replacement admin=%t err=%v", admin, err)
	}
}

func TestLegacyKeyRowsSurviveAccountMergeWithoutAuthorizing(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	winner, err := s.EnsureAccount(ctx, "discord", "101", "Winner")
	if err != nil {
		t.Fatal(err)
	}
	loser, err := s.EnsureAccount(ctx, "homeassistant", "legacy-user", "Loser")
	if err != nil {
		t.Fatal(err)
	}
	keyID := "0123456789abcdef0123456789abcdef"
	if _, err := s.db.SQL().Exec(`INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, verified) VALUES ('openai', ?, ?, 1)`, keyID, loser); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.SQL().Exec(`INSERT INTO api_keys(key_id, canonical_user_id, secret_hash) VALUES (?, ?, zeroblob(32))`, keyID, loser); err != nil {
		t.Fatal(err)
	}
	result := connectTestAccounts(t, s,
		identity.Principal{CanonicalUserID: winner, Gateway: "discord", ExternalID: "101", Assurance: identity.AssuranceDiscordGateway},
		identity.Principal{CanonicalUserID: loser, Gateway: "homeassistant", ExternalID: "legacy-user", Assurance: identity.AssuranceHomeAssistantToken})
	if !result.Merged || result.CanonicalUserID != winner {
		t.Fatalf("merge result: %+v", result)
	}
	var owner string
	if err := s.db.SQL().QueryRow(`SELECT canonical_user_id FROM api_keys WHERE key_id = ?`, keyID).Scan(&owner); err != nil || owner != winner {
		t.Fatalf("retained key owner=%q err=%v", owner, err)
	}
	if _, _, err := s.ResolveAccount("openai", keyID); err == nil {
		t.Fatal("retired key identifier resolved as active account")
	}
}
