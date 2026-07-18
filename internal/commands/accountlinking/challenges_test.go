package accountlinking

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestChallengeLifecycleRejectsExpiryCancellationAndSameAccount(t *testing.T) {
	links := newTestService(t)
	discordID, _ := links.EnsureAccount("discord", "101", "Discord User")
	websocketID, _ := links.EnsureAccount("websocket", "ws-user", "Web User")
	discord := identity.Principal{CanonicalUserID: discordID, Gateway: "discord", ExternalID: "101", Assurance: identity.AssuranceDiscordGateway}
	websocket := identity.Principal{CanonicalUserID: websocketID, Gateway: "websocket", ExternalID: "ws-user", Assurance: identity.AssuranceWebSocketSignedToken}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	links.now = func() time.Time { return now }

	challenge, err := links.CreateChallenge(context.Background(), discord, "req")
	if err != nil {
		t.Fatalf("create challenge: %v", err)
	}
	if _, err := links.ConfirmChallenge(context.Background(), discord, challenge.Code, "req"); !errors.Is(err, ErrChallengeSameActor) {
		t.Fatalf("same-account confirmation error = %v", err)
	}
	links.now = func() time.Time { return now.Add(challengeTTL + time.Second) }
	if _, err := links.ConfirmChallenge(context.Background(), websocket, challenge.Code, "req"); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("expired confirmation error = %v", err)
	}

	links.now = func() time.Time { return now }
	challenge, err = links.CreateChallenge(context.Background(), discord, "req")
	if err != nil {
		t.Fatalf("create replacement challenge: %v", err)
	}
	cancelled, err := links.CancelChallenge(context.Background(), discord, "req")
	if err != nil || !cancelled {
		t.Fatalf("cancel challenge cancelled=%v err=%v", cancelled, err)
	}
	if _, err := links.ConfirmChallenge(context.Background(), websocket, challenge.Code, "req"); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("cancelled confirmation error = %v", err)
	}
}

func TestConnectCommandRequiresAuthenticatedDirectPrincipal(t *testing.T) {
	links := newTestService(t)
	userID, _ := links.EnsureAccount("websocket", "local", "Local")
	service, err := commands.NewService(New(links)...)
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	selfAsserted := identity.Principal{CanonicalUserID: userID, Gateway: "websocket", ExternalID: "local", Assurance: identity.AssuranceSelfAsserted}
	result, err := service.Execute(context.Background(), commands.Request{Principal: selfAsserted, IsDirect: true, Raw: "/connect"})
	if err != nil || result.Text != "Account changes require an authenticated identity." {
		t.Fatalf("self-asserted result=%q err=%v", result.Text, err)
	}
	authenticated := identity.Principal{CanonicalUserID: userID, Gateway: "websocket", ExternalID: "local", Assurance: identity.AssuranceWebSocketSignedToken}
	result, err = service.Execute(context.Background(), commands.Request{Principal: authenticated, IsGroup: true, Raw: "/connect"})
	if err != nil || result.Text != "Use this account command in a direct conversation with Oswald." {
		t.Fatalf("group result=%q err=%v", result.Text, err)
	}
	result, err = service.Execute(context.Background(), commands.Request{Principal: authenticated, IsDirect: true, IsGroup: true, Raw: "/connect"})
	if err != nil || result.Text != "Use this account command in a direct conversation with Oswald." {
		t.Fatalf("contradictory scope result=%q err=%v", result.Text, err)
	}
}

func TestChallengeCreationPrunesExpiredHistoryAfterRetention(t *testing.T) {
	links := newTestService(t)
	userID, _ := links.EnsureAccount("discord", "601", "Retention User")
	principal := identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: "601", Assurance: identity.AssuranceDiscordGateway}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	links.now = func() time.Time { return now }
	first, err := links.CreateChallenge(context.Background(), principal, "req")
	if err != nil {
		t.Fatalf("create first challenge: %v", err)
	}

	links.now = func() time.Time { return now.Add(challengeRetention + challengeTTL + time.Second) }
	if _, err := links.CreateChallenge(context.Background(), principal, "req"); err != nil {
		t.Fatalf("create challenge after retention: %v", err)
	}
	var count int
	if err := links.db.SQL().QueryRow(`SELECT COUNT(*) FROM account_link_challenges WHERE id = ?`, first.ID).Scan(&count); err != nil {
		t.Fatalf("count retained challenge: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired challenge retained after retention window")
	}
}

func TestChallengeConfirmationIsReplaySafeAndVerifiesParticipants(t *testing.T) {
	links := newTestService(t)
	discordID, _ := links.EnsureAccount("discord", "201", "Discord User")
	websocketID, _ := links.EnsureAccount("websocket", "ws-201", "Web User")
	discord := identity.Principal{CanonicalUserID: discordID, Gateway: "discord", ExternalID: "201", Assurance: identity.AssuranceDiscordGateway}
	websocket := identity.Principal{CanonicalUserID: websocketID, Gateway: "websocket", ExternalID: "ws-201", Assurance: identity.AssuranceWebSocketSignedToken}

	challenge, err := links.CreateChallenge(context.Background(), discord, "req")
	if err != nil {
		t.Fatalf("create challenge: %v", err)
	}
	result, err := links.ConfirmChallenge(context.Background(), websocket, challenge.Code, "req")
	if err != nil || !result.Merged || result.CanonicalUserID != discordID {
		t.Fatalf("confirm result=%+v err=%v", result, err)
	}
	accounts, err := links.AccountsForUser(discordID)
	if err != nil || len(accounts) != 2 || !accounts[0].Verified || !accounts[1].Verified {
		t.Fatalf("verified accounts=%+v err=%v", accounts, err)
	}
	websocket.CanonicalUserID = discordID
	replay, err := links.ConfirmChallenge(context.Background(), websocket, challenge.Code, "req")
	if err != nil || !replay.Replayed || replay.CanonicalUserID != discordID {
		t.Fatalf("replay result=%+v err=%v", replay, err)
	}
}

func TestChallengeReplayFollowsLaterCanonicalMerge(t *testing.T) {
	links := newTestService(t)
	firstID, _ := links.EnsureAccount("discord", "211", "First")
	secondID, _ := links.EnsureAccount("websocket", "ws-211", "Second")
	thirdID, _ := links.EnsureAccount("imessage", "+15550000211", "Third")
	first := identity.Principal{CanonicalUserID: firstID, Gateway: "discord", ExternalID: "211", Assurance: identity.AssuranceDiscordGateway}
	second := identity.Principal{CanonicalUserID: secondID, Gateway: "websocket", ExternalID: "ws-211", Assurance: identity.AssuranceWebSocketSignedToken}
	third := identity.Principal{CanonicalUserID: thirdID, Gateway: "imessage", ExternalID: "+15550000211", Assurance: identity.AssuranceBlueBubblesWebhook}

	challenge, err := links.CreateChallenge(context.Background(), first, "req")
	if err != nil {
		t.Fatalf("create first challenge: %v", err)
	}
	if _, err := links.ConfirmChallenge(context.Background(), second, challenge.Code, "req"); err != nil {
		t.Fatalf("confirm first challenge: %v", err)
	}
	if result := connectTestAccounts(t, links, third, first); result.CanonicalUserID != thirdID {
		t.Fatalf("later merge result=%+v", result)
	}
	second.CanonicalUserID = thirdID
	replay, err := links.ConfirmChallenge(context.Background(), second, challenge.Code, "req")
	if err != nil || !replay.Replayed || replay.CanonicalUserID != thirdID {
		t.Fatalf("chained replay result=%+v err=%v", replay, err)
	}
}

func TestAdminMutationReResolvesMergedActor(t *testing.T) {
	links := newTestService(t)
	winnerID, _ := links.EnsureAccount("discord", "221", "Winner")
	loserID, _ := links.EnsureAccount("websocket", "ws-221", "Admin")
	if err := links.SetAdmin(winnerID, loserID, true); err != nil {
		t.Fatalf("grant loser admin: %v", err)
	}
	winner := identity.Principal{CanonicalUserID: winnerID, Gateway: "discord", ExternalID: "221", Assurance: identity.AssuranceDiscordGateway}
	staleAdmin := identity.Principal{CanonicalUserID: loserID, Gateway: "websocket", ExternalID: "ws-221", Assurance: identity.AssuranceWebSocketSignedToken}
	connectTestAccounts(t, links, winner, staleAdmin)

	if err := links.DeleteUserAs(staleAdmin, winnerID); err == nil || !strings.Contains(err.Error(), "cannot delete yourself") {
		t.Fatalf("stale merged actor delete error=%v", err)
	}
	if _, ok, err := links.User(winnerID); err != nil || !ok {
		t.Fatalf("winner deleted by stale actor: ok=%v err=%v", ok, err)
	}
}

func TestChallengeRejectsGatewayConflictAndBanWithoutConsumption(t *testing.T) {
	links := newTestService(t)
	firstID, _ := links.EnsureAccount("discord", "301", "First")
	secondID, _ := links.EnsureAccount("discord", "302", "Second")
	first := identity.Principal{CanonicalUserID: firstID, Gateway: "discord", ExternalID: "301", Assurance: identity.AssuranceDiscordGateway}
	second := identity.Principal{CanonicalUserID: secondID, Gateway: "discord", ExternalID: "302", Assurance: identity.AssuranceDiscordGateway}
	challenge, _ := links.CreateChallenge(context.Background(), first, "req")
	if _, err := links.ConfirmChallenge(context.Background(), second, challenge.Code, "req"); !errors.Is(err, ErrGatewayConflict) {
		t.Fatalf("gateway conflict error = %v", err)
	}

	websocketID, _ := links.EnsureAccount("websocket", "ws-301", "Web")
	websocket := identity.Principal{CanonicalUserID: websocketID, Gateway: "websocket", ExternalID: "ws-301", Assurance: identity.AssuranceWebSocketSignedToken}
	challenge, _ = links.CreateChallenge(context.Background(), first, "req")
	if err := links.BanUser(firstID, websocketID, "blocked"); err != nil {
		t.Fatalf("ban confirmer: %v", err)
	}
	if _, err := links.ConfirmChallenge(context.Background(), websocket, challenge.Code, "req"); !errors.Is(err, ErrLinkBanned) {
		t.Fatalf("banned confirmation error = %v", err)
	}
	if err := links.UnbanUser(firstID, websocketID); err != nil {
		t.Fatalf("unban confirmer: %v", err)
	}
	if _, err := links.ConfirmChallenge(context.Background(), websocket, challenge.Code, "req"); err != nil {
		t.Fatalf("challenge was consumed on rejected ban: %v", err)
	}
}

func TestChallengeMergeTransfersMemoryAndEncryptedMCP(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oswald.db")
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(dbPath, log)
	mcpStore, err := mcp.NewStore(dbPath, "0123456789abcdef0123456789abcdef", log.Server("mcp"))
	if err != nil {
		t.Fatalf("new MCP store: %v", err)
	}
	t.Cleanup(func() { mcpStore.Close() })
	mcpStore.SetResolverForTest(testPublicResolver{})
	manager := mcp.NewManagerFromStore(mcpStore, log)
	links := NewService(dbPath, memories, manager, log)
	winnerID, _ := links.EnsureAccount("discord", "401", "Winner")
	loserID, _ := links.EnsureAccount("websocket", "ws-401", "Loser")
	if _, err := memories.SaveMemory(ctx, loserID, usermemory.SaveRequest{Scope: "long_term", Category: "notes", Statement: "Loser fact", Evidence: "test"}); err != nil {
		t.Fatalf("save loser memory: %v", err)
	}
	if _, err := mcpStore.Save(ctx, mcp.ServerConfig{Scope: mcp.ScopeUser, OwnerUserID: loserID, Name: "home", Transport: mcp.TransportStreamableHTTP, URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer secret"}, Enabled: true}); err != nil {
		t.Fatalf("save loser MCP: %v", err)
	}
	winner := identity.Principal{CanonicalUserID: winnerID, Gateway: "discord", ExternalID: "401", Assurance: identity.AssuranceDiscordGateway}
	loser := identity.Principal{CanonicalUserID: loserID, Gateway: "websocket", ExternalID: "ws-401", Assurance: identity.AssuranceWebSocketSignedToken}
	connectTestAccounts(t, links, winner, loser)

	entries, err := memories.ListMemories(winnerID, "", "", 10)
	if err != nil || len(entries) != 1 || entries[0].Statement != "Loser fact" {
		t.Fatalf("merged memories=%+v err=%v", entries, err)
	}
	cfg, ok, err := mcpStore.Get(ctx, mcp.ScopeUser, winnerID, "home")
	if err != nil || !ok || cfg.URL != "https://example.com/mcp" || cfg.Headers["Authorization"] != "Bearer secret" {
		t.Fatalf("merged MCP=%+v ok=%v err=%v", cfg, ok, err)
	}
	if _, ok, err := mcpStore.Get(ctx, mcp.ScopeUser, loserID, "home"); err != nil || ok {
		t.Fatalf("loser MCP remains ok=%v err=%v", ok, err)
	}
}

func TestChallengeConfirmationRollsBackConsumptionOnMergeFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oswald.db")
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(dbPath, log)
	mcpMerger := &failingMCPMerger{fail: true}
	links := NewService(dbPath, memories, mcpMerger, log)
	winnerID, _ := links.EnsureAccount("discord", "501", "Winner")
	loserID, _ := links.EnsureAccount("websocket", "ws-501", "Loser")
	winner := identity.Principal{CanonicalUserID: winnerID, Gateway: "discord", ExternalID: "501", Assurance: identity.AssuranceDiscordGateway}
	loser := identity.Principal{CanonicalUserID: loserID, Gateway: "websocket", ExternalID: "ws-501", Assurance: identity.AssuranceWebSocketSignedToken}
	challenge, _ := links.CreateChallenge(context.Background(), winner, "req")
	if _, err := links.ConfirmChallenge(context.Background(), loser, challenge.Code, "req"); err == nil {
		t.Fatal("expected injected MCP merge failure")
	}
	if owner, ok, _ := links.ResolveAccount("websocket", "ws-501"); !ok || owner != loserID {
		t.Fatalf("failed merge changed owner: owner=%q ok=%v", owner, ok)
	}
	mcpMerger.fail = false
	if _, err := links.ConfirmChallenge(context.Background(), loser, challenge.Code, "req"); err != nil {
		t.Fatalf("challenge was consumed by rolled-back merge: %v", err)
	}
}

func TestDeleteUserRollsBackWhenMCPDeletionFails(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oswald.db")
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(dbPath, log)
	t.Cleanup(func() { memories.Close() })
	mcpMerger := &failingMCPMerger{deleteFail: true}
	links := NewService(dbPath, memories, mcpMerger, log)
	t.Cleanup(func() { links.Close() })
	actorID, _ := links.EnsureAccount("discord", "511", "Actor")
	targetID, _ := links.EnsureAccount("websocket", "ws-511", "Target")

	if err := links.DeleteUser(actorID, targetID); err == nil {
		t.Fatal("expected injected MCP deletion failure")
	}
	if _, ok, err := links.User(targetID); err != nil || !ok {
		t.Fatalf("failed deletion removed user: ok=%v err=%v", ok, err)
	}
	mcpMerger.deleteFail = false
	if err := links.DeleteUser(actorID, targetID); err != nil {
		t.Fatalf("delete after recovery: %v", err)
	}
	if mcpMerger.deletedUser != targetID {
		t.Fatalf("committed deletion notification=%q", mcpMerger.deletedUser)
	}
}

type failingMCPMerger struct {
	fail        bool
	deleteFail  bool
	deletedUser string
}

func (m *failingMCPMerger) MergeUsersTx(context.Context, *sql.Tx, string, string) error {
	if m.fail {
		return errors.New("injected MCP merge failure")
	}
	return nil
}

func (*failingMCPMerger) UserMergeCommitted(string, string) {}
func (m *failingMCPMerger) DeleteUserTx(context.Context, *sql.Tx, string) error {
	if m.deleteFail {
		return errors.New("injected MCP deletion failure")
	}
	return nil
}
func (m *failingMCPMerger) UserDeleteCommitted(userID string) { m.deletedUser = userID }

type testPublicResolver struct{}

func (testPublicResolver) LookupHost(context.Context, string) ([]string, error) {
	return []string{"93.184.216.34"}, nil
}
