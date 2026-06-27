package usermanagement

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
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
	service := newAdminCommandService(t, links)

	for _, input := range []string{"/users", "/user " + userID} {
		result, err := service.Execute(context.Background(), commands.Request{UserID: userID, Raw: input})
		if err != nil {
			t.Fatalf("execute %s err=%v", input, err)
		}
		if result.Text != "You are not allowed to use admin commands." {
			t.Fatalf("unexpected response for %s: %q", input, result.Text)
		}
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
	service := newAdminCommandService(t, links)

	response, err := executeCommand(t, service, adminID, "/users")
	if err != nil {
		t.Fatalf("users err=%v", err)
	}
	if !strings.Contains(response, targetID) || !strings.Contains(response, "You are speaking with Target.") {
		t.Fatalf("unexpected users response: %q", response)
	}

	response, err = executeCommand(t, service, adminID, "/user "+targetID)
	if err != nil {
		t.Fatalf("user err=%v", err)
	}
	if !strings.Contains(response, targetID) || !strings.Contains(response, "You are speaking with Target.") || !strings.Contains(response, "discord:300 (Target)") {
		t.Fatalf("unexpected user response: %q", response)
	}

	response, err = executeCommand(t, service, adminID, "/user")
	if err != nil || response != "Show one canonical user.\nUse: /user <canonical_id>" {
		t.Fatalf("usage response=%q err=%v", response, err)
	}

	response, err = executeCommand(t, service, adminID, "/user usr_missing")
	if err != nil || response != "User usr_missing not found." {
		t.Fatalf("missing response=%q err=%v", response, err)
	}

	response, err = executeCommand(t, service, adminID, "/admin "+targetID)
	if err != nil || !strings.Contains(response, "Marked "+targetID+" as admin.") {
		t.Fatalf("admin response=%q err=%v", response, err)
	}
	isAdmin, err := links.IsAdmin(targetID)
	if err != nil || !isAdmin {
		t.Fatalf("expected target admin, got %v err=%v", isAdmin, err)
	}

	response, err = executeCommand(t, service, adminID, "/unadmin "+adminID)
	if err != nil || !strings.Contains(response, "cannot remove admin from yourself") {
		t.Fatalf("self unadmin response=%q err=%v", response, err)
	}

	response, err = executeCommand(t, service, adminID, "/ban "+targetID+" spam")
	if err != nil || !strings.Contains(response, "Banned "+targetID+".") {
		t.Fatalf("ban response=%q err=%v", response, err)
	}
	isBanned, err := links.IsBanned(targetID)
	if err != nil || !isBanned {
		t.Fatalf("expected target banned, got %v err=%v", isBanned, err)
	}

	response, err = executeCommand(t, service, adminID, "/unban "+targetID)
	if err != nil || !strings.Contains(response, "Unbanned "+targetID+".") {
		t.Fatalf("unban response=%q err=%v", response, err)
	}
	isBanned, err = links.IsBanned(targetID)
	if err != nil || isBanned {
		t.Fatalf("expected target unbanned, got %v err=%v", isBanned, err)
	}
}

func newAdminCommandService(t *testing.T, links *accountlinking.Service) *commands.Service {
	t.Helper()
	registrations := make([]commands.Command, 0)
	for _, handler := range New(links) {
		registrations = append(registrations, commands.Command{Handler: handler, Middleware: []commands.Middleware{commands.RequireAdmin(links)}})
	}
	service, err := commands.NewServiceWithCommands(registrations...)
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	return service
}

func executeCommand(t *testing.T, service *commands.Service, userID, raw string) (string, error) {
	t.Helper()
	result, err := service.Execute(context.Background(), commands.Request{UserID: userID, Raw: raw})
	return result.Text, err
}

func newTestService(t *testing.T) *accountlinking.Service {
	t.Helper()
	dir := t.TempDir()
	log := config.NewLogger(config.LevelError)
	memories := usermemory.NewStore(filepath.Join(dir, "users"), log)
	return accountlinking.NewService(filepath.Join(dir, "oswald.db"), memories, log)
}
