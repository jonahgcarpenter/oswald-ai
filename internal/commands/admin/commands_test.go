package admin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestCommandHandlerRequiresAdmin(t *testing.T) {
	links := newTestService(t)
	userID, err := links.EnsureAccount("discord", "100", "User")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	handler := NewCommandHandler(links)

	response, handled, err := handler.Handle(userID, "/users")
	if err != nil || !handled {
		t.Fatalf("handle users err=%v handled=%v", err, handled)
	}
	if response != "You are not allowed to use admin commands." {
		t.Fatalf("unexpected response: %q", response)
	}
}

func TestCommandHandlerUsersAdminBanAndUnban(t *testing.T) {
	links := newTestService(t)
	adminID, err := links.EnsureAccount("discord", "200", "Admin")
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	targetID, err := links.EnsureAccount("discord", "300", "Target")
	if err != nil {
		t.Fatalf("ensure target: %v", err)
	}
	if err := links.SetAdmin(adminID, adminID, true); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	handler := NewCommandHandler(links)

	response, handled, err := handler.Handle(adminID, "/users")
	if err != nil || !handled {
		t.Fatalf("users err=%v handled=%v", err, handled)
	}
	if !strings.Contains(response, targetID) || !strings.Contains(response, "You are speaking with Target.") {
		t.Fatalf("unexpected users response: %q", response)
	}

	response, handled, err = handler.Handle(adminID, "/admin "+targetID)
	if err != nil || !handled || !strings.Contains(response, "Marked "+targetID+" as admin.") {
		t.Fatalf("admin response=%q handled=%v err=%v", response, handled, err)
	}
	isAdmin, err := links.IsAdmin(targetID)
	if err != nil || !isAdmin {
		t.Fatalf("expected target admin, got %v err=%v", isAdmin, err)
	}

	response, handled, err = handler.Handle(adminID, "/unadmin "+adminID)
	if err != nil || !handled || !strings.Contains(response, "cannot remove admin from yourself") {
		t.Fatalf("self unadmin response=%q handled=%v err=%v", response, handled, err)
	}

	response, handled, err = handler.Handle(adminID, "/ban "+targetID+" spam")
	if err != nil || !handled || !strings.Contains(response, "Banned "+targetID+".") {
		t.Fatalf("ban response=%q handled=%v err=%v", response, handled, err)
	}
	isBanned, err := links.IsBanned(targetID)
	if err != nil || !isBanned {
		t.Fatalf("expected target banned, got %v err=%v", isBanned, err)
	}

	response, handled, err = handler.Handle(adminID, "/unban "+targetID)
	if err != nil || !handled || !strings.Contains(response, "Unbanned "+targetID+".") {
		t.Fatalf("unban response=%q handled=%v err=%v", response, handled, err)
	}
	isBanned, err = links.IsBanned(targetID)
	if err != nil || isBanned {
		t.Fatalf("expected target unbanned, got %v err=%v", isBanned, err)
	}
}

func newTestService(t *testing.T) *accountlinking.Service {
	t.Helper()
	dir := t.TempDir()
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	return accountlinking.NewService(filepath.Join(dir, "oswald.db"), memories, log)
}
