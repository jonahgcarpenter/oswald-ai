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
	fenceTargets, err := links.ResolveChallengeFenceTargets(context.Background(), websocket, challenge.Code)
	if err != nil {
		t.Fatalf("resolve challenge fence targets: %v", err)
	}
	targetSet := make(map[string]bool, len(fenceTargets))
	for _, target := range fenceTargets {
		targetSet[target] = true
	}
	if !targetSet[discordID] || !targetSet[websocketID] {
		t.Fatalf("challenge fence targets=%v", fenceTargets)
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
	loserProfile, err := memories.ResolveSessionProfile(ctx, loserID, "linked-session", time.Hour)
	if err != nil {
		t.Fatalf("resolve loser session: %v", err)
	}
	if err := memories.AppendSessionTurnForGeneration(ctx, "linked-session", loserID, loserProfile.Generation, "before link", "still here", nil, time.Hour); err != nil {
		t.Fatalf("append loser session: %v", err)
	}
	if _, err := mcpStore.Save(ctx, mcp.ServerConfig{Scope: mcp.ScopeUser, OwnerUserID: loserID, Name: "home", Transport: mcp.TransportStreamableHTTP, URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer secret"}, Enabled: true}); err != nil {
		t.Fatalf("save loser MCP: %v", err)
	}
	if _, err := links.db.SQL().Exec(`
INSERT INTO privacy_operations(operation_id, idempotency_key, actor_hash, target_user_id, target_hash, operation_type, target_digest, status, created_at, updated_at, started_at, completed_at)
VALUES ('completed-before-merge', 'completed-before-merge', ?, ?, ?, 'export_user', ?, 'completed', datetime('now'), datetime('now'), datetime('now'), datetime('now'));
INSERT INTO privacy_operations(operation_id, idempotency_key, actor_hash, target_user_id, target_hash, operation_type, target_digest, challenge_hash, challenge_expires_at, status, created_at, updated_at)
VALUES ('pending-before-merge', 'pending-before-merge', ?, ?, ?, 'delete_user', ?, ?, datetime('now', '+10 minutes'), 'pending', datetime('now'), datetime('now'))`,
		strings.Repeat("a", 64), loserID, strings.Repeat("b", 64), strings.Repeat("c", 64),
		strings.Repeat("d", 64), loserID, strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("1", 64)); err != nil {
		t.Fatalf("save loser privacy operations: %v", err)
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
	contextResult, err := memories.BuildContext(ctx, winnerID, "linked-session", "before", usermemory.ContextOptions{Generation: loserProfile.Generation, RecentTurns: 10, ContextBudgetChars: 4000})
	if err != nil || !strings.Contains(contextResult.Block, "before link") {
		t.Fatalf("merged linked-session context=%q err=%v", contextResult.Block, err)
	}
	rows, err := links.db.SQL().Query(`SELECT target_user_id, status, challenge_hash FROM privacy_operations WHERE operation_id IN ('completed-before-merge', 'pending-before-merge') ORDER BY operation_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var privacyRows int
	for rows.Next() {
		var target, status, challengeHash string
		if err := rows.Scan(&target, &status, &challengeHash); err != nil {
			t.Fatal(err)
		}
		privacyRows++
		if target != winnerID {
			t.Fatalf("merged privacy target = %q, want %q", target, winnerID)
		}
		if status == "pending" || status == "running" || challengeHash != "" {
			t.Fatalf("unsafe merged privacy operation status=%q challenge=%q", status, challengeHash)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if privacyRows != 2 {
		t.Fatalf("merged privacy row count = %d, want 2", privacyRows)
	}
	var loserChallengeReferences int
	if err := links.db.SQL().QueryRow(`SELECT COUNT(*) FROM account_link_challenges WHERE initiator_user_id = ? OR consumed_by_user_id = ? OR result_user_id = ? OR invalidated_by_user_id = ?`, loserID, loserID, loserID, loserID).Scan(&loserChallengeReferences); err != nil {
		t.Fatal(err)
	}
	if loserChallengeReferences != 0 {
		t.Fatalf("loser account-link audit references = %d", loserChallengeReferences)
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

func TestChallengeMergeRollsBackPreservedStateWhenFinalDeleteFails(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(dbPath, log)
	t.Cleanup(func() { memories.Close() })
	links := NewService(dbPath, memories, nil, log)
	t.Cleanup(func() { links.Close() })
	winnerID, _ := links.EnsureAccount("discord", "591", "Winner")
	loserID, _ := links.EnsureAccount("websocket", "merge-fail-loser", "Loser")
	if _, err := memories.SaveMemory(ctx, loserID, usermemory.SaveRequest{Scope: "long_term", Category: "notes", Statement: "rollback fact", Evidence: "test"}); err != nil {
		t.Fatal(err)
	}
	profile, err := memories.ResolveSessionProfile(ctx, loserID, "rollback-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := memories.AppendSessionTurnForGeneration(ctx, "rollback-session", loserID, profile.Generation, "rollback turn", "answer", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	winner := identity.Principal{CanonicalUserID: winnerID, Gateway: "discord", ExternalID: "591", Assurance: identity.AssuranceDiscordGateway}
	loser := identity.Principal{CanonicalUserID: loserID, Gateway: "websocket", ExternalID: "merge-fail-loser", Assurance: identity.AssuranceWebSocketSignedToken}
	challenge, err := links.CreateChallenge(ctx, winner, "req")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := links.db.SQL().Exec(`CREATE TRIGGER fail_final_account_merge BEFORE DELETE ON account_users BEGIN SELECT RAISE(ABORT, 'injected final merge failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := links.ConfirmChallenge(ctx, loser, challenge.Code, "req"); err == nil {
		t.Fatal("expected final account deletion failure")
	}
	var loserMemories, loserTurns, loserActive int
	if err := links.db.SQL().QueryRow(`SELECT COUNT(*) FROM memory_entries WHERE canonical_user_id = ?`, loserID).Scan(&loserMemories); err != nil {
		t.Fatal(err)
	}
	if err := links.db.SQL().QueryRow(`SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = ?`, loserID).Scan(&loserTurns); err != nil {
		t.Fatal(err)
	}
	if err := links.db.SQL().QueryRow(`SELECT COUNT(*) FROM account_users WHERE canonical_user_id = ? AND lifecycle_state = 'active'`, loserID).Scan(&loserActive); err != nil {
		t.Fatal(err)
	}
	if loserMemories != 1 || loserTurns != 1 || loserActive != 1 {
		t.Fatalf("rolled-back loser memories=%d turns=%d active=%d", loserMemories, loserTurns, loserActive)
	}
	if _, err := links.db.SQL().Exec(`DROP TRIGGER fail_final_account_merge`); err != nil {
		t.Fatal(err)
	}
	if _, err := links.ConfirmChallenge(ctx, loser, challenge.Code, "req"); err != nil {
		t.Fatalf("challenge consumed by rolled-back preserved-state merge: %v", err)
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
