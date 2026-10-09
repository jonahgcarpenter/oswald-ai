package session

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type fakeStarter struct {
	userID    string
	sessionID string
}

func (s *fakeStarter) NewSessionContext(_ context.Context, userID, sessionID string) error {
	s.userID, s.sessionID = userID, sessionID
	return nil
}

func TestNewSessionCommandStartsFreshSession(t *testing.T) {
	starter := &fakeStarter{}
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: New(starter)})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "discord", ExternalID: "user", Assurance: identity.AssuranceDiscordGateway}
	result, err := service.Execute(context.Background(), commands.Request{Principal: principal, SessionKey: "session", Raw: "/new"})
	if err != nil {
		t.Fatal(err)
	}
	if starter.userID != "user" || starter.sessionID != "session" {
		t.Fatalf("new scope user=%q session=%q", starter.userID, starter.sessionID)
	}
	if result.Text != "Started a new session. Past conversations remain searchable." {
		t.Fatalf("unexpected text: %q", result.Text)
	}
	if result.Outcome.Operation != "session.new" {
		t.Fatalf("unexpected operation: %q", result.Outcome.Operation)
	}
}

func TestNewSessionCommandRejectsUnauthenticatedIdentity(t *testing.T) {
	starter := &fakeStarter{}
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: New(starter)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(context.Background(), commands.Request{Principal: identity.Principal{}, SessionKey: "session", Raw: "/new"})
	if err == nil || starter.userID != "" || result.Text != "" {
		t.Fatalf("result=%q starter=%+v err=%v", result.Text, starter, err)
	}
}
