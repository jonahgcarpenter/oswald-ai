package accountlinking

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestServiceEnsureLinkDisconnectAndSpeakerLine(t *testing.T) {
	links := newTestService(t)

	userID, err := links.EnsureAccount("discord", "123", "Alice")
	if err != nil {
		t.Fatalf("ensure discord: %v", err)
	}
	again, err := links.EnsureAccount("discord", "123", "Alice Updated")
	if err != nil {
		t.Fatalf("ensure existing discord: %v", err)
	}
	if again != userID {
		t.Fatalf("expected same canonical user, got %q then %q", userID, again)
	}

	linked, err := links.LinkAccount(userID, "websocket", "alice-local", "")
	if err != nil {
		t.Fatalf("link websocket: %v", err)
	}
	if linked.CanonicalUserID != userID || linked.LinkedAccount.Identifier != "alice-local" {
		t.Fatalf("unexpected link result: %+v", linked)
	}

	accounts, err := links.AccountsForUser(userID)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if len(accounts) != 2 || accounts[0].Gateway != "discord" || accounts[1].Gateway != "websocket" {
		t.Fatalf("unexpected sorted accounts: %+v", accounts)
	}

	line, err := links.SpeakerLine(userID)
	if err != nil {
		t.Fatalf("speaker line: %v", err)
	}
	if line != "You are speaking with Alice Updated." {
		t.Fatalf("unexpected speaker line %q", line)
	}

	if err := links.DisconnectAccount(userID, "websocket", "alice-local"); err != nil {
		t.Fatalf("disconnect websocket: %v", err)
	}
	if err := links.DisconnectAccount(userID, "discord", "123"); err == nil {
		t.Fatal("expected error disconnecting last account")
	}
}

func TestCommandHandlerConnectAndDisconnect(t *testing.T) {
	links := newTestService(t)
	userID, err := links.EnsureAccount("discord", "123", "Alice")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	service, err := commands.NewService(New(links)...)
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}

	response, err := executeAccountCommand(service, userID, "/connect")
	if err != nil {
		t.Fatalf("start connect err=%v", err)
	}
	if !strings.Contains(response, "Connect an account.") || !strings.Contains(response, "Discord (connected)") {
		t.Fatalf("unexpected connect menu: %q", response)
	}

	response, err = executeAccountCommand(service, userID, "/connect 2 alice-local")
	if err != nil {
		t.Fatalf("connect err=%v", err)
	}
	if !strings.Contains(response, "Linked WebSocket as alice-local.") {
		t.Fatalf("unexpected connect response: %q", response)
	}

	response, err = executeAccountCommand(service, userID, "/disconnect")
	if err != nil {
		t.Fatalf("start disconnect err=%v", err)
	}
	if !strings.Contains(response, "Disconnect an account.") {
		t.Fatalf("unexpected disconnect menu: %q", response)
	}

	response, err = executeAccountCommand(service, userID, "/disconnect 2")
	if err != nil {
		t.Fatalf("disconnect err=%v", err)
	}
	if !strings.Contains(response, "Disconnected WebSocket: alice-local.") {
		t.Fatalf("unexpected disconnect response: %q", response)
	}
}

func executeAccountCommand(service *commands.Service, userID, raw string) (string, error) {
	result, err := service.Execute(context.Background(), commands.Request{Principal: identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: "100", Assurance: identity.AssuranceDiscordGateway}, Raw: raw})
	return result.Text, err
}

func TestServicePersistsSQLiteAccounts(t *testing.T) {
	dir := t.TempDir()
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	dbPath := filepath.Join(dir, "oswald.db")
	legacyPath := filepath.Join(dir, "links.json")

	links := NewService(dbPath, memories, log)
	links.legacyPath = legacyPath
	userID, err := links.EnsureAccount("discord", "123", "Alice")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	if _, err := links.LinkAccount(userID, "websocket", "alice-local", ""); err != nil {
		t.Fatalf("link websocket: %v", err)
	}

	reopened := NewService(dbPath, memories, log)
	reopened.legacyPath = legacyPath
	accounts, err := reopened.AccountsForUser(userID)
	if err != nil {
		t.Fatalf("accounts after reopen: %v", err)
	}
	if len(accounts) != 2 || accounts[0].Gateway != "discord" || accounts[1].Gateway != "websocket" {
		t.Fatalf("unexpected persisted accounts: %+v", accounts)
	}
}

func TestServiceMigratesLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	dbPath := filepath.Join(dir, "oswald.db")
	legacyPath := filepath.Join(dir, "links.json")
	now := time.Now().UTC().Truncate(time.Second)

	legacy := fileData{
		Version: 1,
		Users: map[string]UserRecord{
			"usr_legacy": {
				CreatedAt: now,
				UpdatedAt: now,
				Accounts: []LinkedAccount{{
					Gateway:     "discord",
					Identifier:  "123",
					DisplayName: "Alice",
					LinkedAt:    now,
					Verified:    true,
				}},
			},
		},
		AccountIndex: map[string]string{"discord:123": "usr_legacy"},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(legacyPath, raw, 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	links := NewService(dbPath, memories, log)
	links.legacyPath = legacyPath
	if err := links.Initialize(); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	userID, err := links.EnsureAccount("discord", "123", "Alice Updated")
	if err != nil {
		t.Fatalf("ensure migrated account: %v", err)
	}
	if userID != "usr_legacy" {
		t.Fatalf("got user %q, want legacy user", userID)
	}
	accounts, err := links.AccountsForUser(userID)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].DisplayName != "Alice Updated" || !accounts[0].Verified {
		t.Fatalf("unexpected migrated account: %+v", accounts)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy file should remain as backup: %v", err)
	}
}

func TestServiceAdminBanAndListUsers(t *testing.T) {
	links := newTestService(t)
	adminID, err := links.EnsureAccount("discord", "100", "Admin")
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	targetID, err := links.EnsureAccount("discord", "200", "Target")
	if err != nil {
		t.Fatalf("ensure target: %v", err)
	}

	if err := links.SetAdmin(adminID, adminID, true); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	if err := links.SetAdmin(adminID, adminID, false); err == nil || !strings.Contains(err.Error(), "cannot remove admin from yourself") {
		t.Fatalf("expected self unadmin error, got %v", err)
	}
	if err := links.BanUser(adminID, adminID, "bad"); err == nil || !strings.Contains(err.Error(), "cannot ban yourself") {
		t.Fatalf("expected self ban error, got %v", err)
	}
	if err := links.BanUser(adminID, targetID, "spam"); err != nil {
		t.Fatalf("ban target: %v", err)
	}

	isAdmin, err := links.IsAdmin(adminID)
	if err != nil || !isAdmin {
		t.Fatalf("expected admin true, got %v err=%v", isAdmin, err)
	}
	isBanned, err := links.IsBanned(targetID)
	if err != nil || !isBanned {
		t.Fatalf("expected banned true, got %v err=%v", isBanned, err)
	}

	users, err := links.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %+v", users)
	}
	foundTarget := false
	for _, user := range users {
		if user.CanonicalUserID == targetID {
			foundTarget = true
			if !user.IsBanned || user.BanReason != "spam" || !strings.Contains(user.Intro, "Target") {
				t.Fatalf("unexpected target summary: %+v", user)
			}
		}
	}
	if !foundTarget {
		t.Fatalf("target not found in users: %+v", users)
	}

	if err := links.UnbanUser(adminID, targetID); err != nil {
		t.Fatalf("unban target: %v", err)
	}
	isBanned, err = links.IsBanned(targetID)
	if err != nil || isBanned {
		t.Fatalf("expected banned false, got %v err=%v", isBanned, err)
	}
	users, err = links.ListUsers()
	if err != nil {
		t.Fatalf("list users after unban: %v", err)
	}
	foundTarget = false
	for _, user := range users {
		if user.CanonicalUserID == targetID {
			foundTarget = true
			if user.IsBanned || user.BanReason != "" {
				t.Fatalf("expected cleared ban fields after unban, got %+v", user)
			}
		}
	}
	if !foundTarget {
		t.Fatalf("target not found after unban: %+v", users)
	}
}

func TestServiceMergePreservesAdminAndBanState(t *testing.T) {
	links := newTestService(t)
	targetID, err := links.EnsureAccount("discord", "300", "Target")
	if err != nil {
		t.Fatalf("ensure target: %v", err)
	}
	sourceID, err := links.EnsureAccount("websocket", "source", "Source")
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	if err := links.SetAdmin(sourceID, sourceID, true); err != nil {
		t.Fatalf("set source admin: %v", err)
	}
	if err := links.BanUser(targetID, sourceID, "merged ban"); err != nil {
		t.Fatalf("ban source: %v", err)
	}

	result, err := links.LinkAccount(targetID, "websocket", "source", "")
	if err != nil {
		t.Fatalf("merge link: %v", err)
	}
	if !result.Merged {
		t.Fatalf("expected merge result: %+v", result)
	}
	isAdmin, err := links.IsAdmin(targetID)
	if err != nil || !isAdmin {
		t.Fatalf("expected merged admin true, got %v err=%v", isAdmin, err)
	}
	isBanned, err := links.IsBanned(targetID)
	if err != nil || !isBanned {
		t.Fatalf("expected merged banned true, got %v err=%v", isBanned, err)
	}
	user, ok, err := links.User(targetID)
	if err != nil || !ok {
		t.Fatalf("merged user lookup ok=%v err=%v", ok, err)
	}
	if user.BanReason != "merged ban" {
		t.Fatalf("expected ban metadata preserved, got %+v", user)
	}
}

func TestServiceDeleteUserRemovesAccountsMemoryAndSessions(t *testing.T) {
	links := newTestService(t)
	adminID, err := links.EnsureAccount("discord", "400", "Admin")
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	targetID, err := links.EnsureAccount("discord", "500", "Target")
	if err != nil {
		t.Fatalf("ensure target: %v", err)
	}
	if _, err := links.LinkAccount(targetID, "websocket", "target-local", "Target Local"); err != nil {
		t.Fatalf("link websocket: %v", err)
	}
	if err := links.SetAdmin(adminID, adminID, true); err != nil {
		t.Fatalf("set admin: %v", err)
	}

	db := links.db.SQL()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT OR REPLACE INTO user_memory_profiles (canonical_user_id, intro, created_at, updated_at) VALUES (?, ?, ?, ?)`, targetID, "You are speaking with Target.", now, now); err != nil {
		t.Fatalf("insert profile: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memory_entries (canonical_user_id, scope, category, statement, statement_key, evidence, confidence, importance, status, source_session_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, targetID, "long_term", "durable_preferences", "The user likes green.", "the user likes green.", "test", 0.9, 3, "active", "session-target", now, now); err != nil {
		t.Fatalf("insert memory entry: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO session_turns (session_id, canonical_user_id, user_text, assistant_text, created_at) VALUES (?, ?, ?, ?, ?)`, "session-target", targetID, "hello", "hi", now); err != nil {
		t.Fatalf("insert session turn: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO mcp_servers (id, scope, owner_user_id, name, type, transport, url_ciphertext, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, "mcp-target", "user", targetID, "target-tools", "generic", "streamable_http", "ciphertext", now, now); err != nil {
		t.Fatalf("insert mcp server: %v", err)
	}

	if err := links.DeleteUser(adminID, adminID); err == nil || !strings.Contains(err.Error(), "cannot delete yourself") {
		t.Fatalf("expected self delete error, got %v", err)
	}
	if err := links.DeleteUser(adminID, targetID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	if _, ok, err := links.User(targetID); err != nil || ok {
		t.Fatalf("expected deleted user missing, ok=%v err=%v", ok, err)
	}
	accounts, err := links.AccountsForUser(targetID)
	if err != nil {
		t.Fatalf("accounts after delete: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("expected no accounts after delete, got %+v", accounts)
	}

	assertRowCount(t, db, `SELECT COUNT(*) FROM account_users WHERE canonical_user_id = ?`, targetID, 0)
	assertRowCount(t, db, `SELECT COUNT(*) FROM linked_accounts WHERE canonical_user_id = ?`, targetID, 0)
	assertRowCount(t, db, `SELECT COUNT(*) FROM user_memory_profiles WHERE canonical_user_id = ?`, targetID, 0)
	assertRowCount(t, db, `SELECT COUNT(*) FROM memory_entries WHERE canonical_user_id = ?`, targetID, 0)
	assertRowCount(t, db, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = ?`, targetID, 0)
	assertRowCount(t, db, `SELECT COUNT(*) FROM mcp_servers WHERE owner_user_id = ?`, targetID, 0)

	recreatedID, err := links.EnsureAccount("discord", "500", "Target Recreated")
	if err != nil {
		t.Fatalf("recreate deleted account: %v", err)
	}
	if recreatedID == targetID {
		t.Fatalf("expected deleted external account to create a new canonical user, got original %s", recreatedID)
	}
}

func assertRowCount(t *testing.T, db interface {
	QueryRow(string, ...interface{}) *sql.Row
}, query, userID string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(query, userID).Scan(&got); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("count query %q got %d, want %d", query, got, want)
	}
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	links := NewService(filepath.Join(dir, "oswald.db"), memories, log)
	links.legacyPath = filepath.Join(dir, "links.json")
	return links
}
