package accountlinking

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
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
	handler := NewCommandHandler(links)

	if !handler.CanHandle(" /connect ") || handler.CanHandle("hello") {
		t.Fatal("unexpected CanHandle result")
	}

	response, handled, err := handler.Handle(userID, "/connect")
	if err != nil || !handled {
		t.Fatalf("start connect handled=%v err=%v", handled, err)
	}
	if !strings.Contains(response, "Connect an account.") || !strings.Contains(response, "Discord (connected)") {
		t.Fatalf("unexpected connect menu: %q", response)
	}

	response, handled, err = handler.Handle(userID, "/connect 2 alice-local")
	if err != nil || !handled {
		t.Fatalf("connect handled=%v err=%v", handled, err)
	}
	if !strings.Contains(response, "Linked WebSocket as alice-local.") {
		t.Fatalf("unexpected connect response: %q", response)
	}

	response, handled, err = handler.Handle(userID, "/disconnect")
	if err != nil || !handled {
		t.Fatalf("start disconnect handled=%v err=%v", handled, err)
	}
	if !strings.Contains(response, "Disconnect an account.") {
		t.Fatalf("unexpected disconnect menu: %q", response)
	}

	response, handled, err = handler.Handle(userID, "/disconnect 2")
	if err != nil || !handled {
		t.Fatalf("disconnect handled=%v err=%v", handled, err)
	}
	if !strings.Contains(response, "Disconnected WebSocket: alice-local.") {
		t.Fatalf("unexpected disconnect response: %q", response)
	}
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

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	links := NewService(filepath.Join(dir, "oswald.db"), memories, log)
	links.legacyPath = filepath.Join(dir, "links.json")
	return links
}
