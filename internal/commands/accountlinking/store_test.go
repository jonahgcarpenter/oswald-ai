package accountlinking

import (
	"path/filepath"
	"strings"
	"testing"

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

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	return NewService(filepath.Join(dir, "links.json"), memories, log)
}
