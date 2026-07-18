package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestServiceExecutesKnownUnknownAndAdminCommands(t *testing.T) {
	auth := fakeAuthorizer{admins: map[string]bool{"admin": true}}
	service, err := NewServiceWithCommands(
		Command{Handler: fakeCommand{name: "ping"}},
		Command{Handler: fakeCommand{name: "secret", admin: true}, Middleware: []Middleware{RequireAdmin(auth)}},
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	result, err := service.Execute(context.Background(), Request{Principal: testPrincipal("user"), Raw: "/ping one two"})
	if err != nil || result.Text != "ran ping:one two" {
		t.Fatalf("known command result=%+v err=%v", result, err)
	}

	result, err = service.Execute(context.Background(), Request{Principal: testPrincipal("user"), Raw: "/missing"})
	if err != nil || result.Text != "Unknown command: /missing" {
		t.Fatalf("unknown command result=%+v err=%v", result, err)
	}

	result, err = service.Execute(context.Background(), Request{Principal: testPrincipal("user"), Raw: "/secret"})
	if err != nil || result.Text != "You are not allowed to use admin commands." {
		t.Fatalf("non-admin result=%+v err=%v", result, err)
	}

	result, err = service.Execute(context.Background(), Request{Principal: testPrincipal("admin"), Raw: "/secret"})
	if err != nil || result.Text != "ran secret:" {
		t.Fatalf("admin result=%+v err=%v", result, err)
	}
}

func TestServiceRejectsInvalidPrincipal(t *testing.T) {
	service, err := NewService(fakeCommand{name: "ping"})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := service.Execute(context.Background(), Request{Raw: "/ping"}); err == nil {
		t.Fatal("expected invalid principal error")
	}
}

func TestServicePropagatesPrincipalToHandler(t *testing.T) {
	want := identity.Principal{
		CanonicalUserID: "user",
		Gateway:         "discord",
		ExternalID:      "123",
		Assurance:       identity.AssuranceDiscordGateway,
	}
	var got identity.Principal
	service, err := NewService(HandlerFunc{
		DefinitionValue: Definition{Name: "principal"},
		ExecuteFunc: func(_ context.Context, req Request) (Result, error) {
			got = req.Principal
			return Result{Text: "ok"}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := service.Execute(context.Background(), Request{Principal: want, Raw: "/principal"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != want {
		t.Fatalf("principal = %+v, want %+v", got, want)
	}
}

func TestServiceRejectsDuplicateNamesAndAliases(t *testing.T) {
	_, err := NewService(fakeCommand{name: "ping"}, fakeCommand{name: "ping"})
	if !errors.Is(err, ErrDuplicateCommand) {
		t.Fatalf("expected duplicate command error, got %v", err)
	}

	_, err = NewService(fakeCommand{name: "ping", aliases: []string{"p"}}, fakeCommand{name: "pong", aliases: []string{"p"}})
	if !errors.Is(err, ErrDuplicateAlias) {
		t.Fatalf("expected duplicate alias error, got %v", err)
	}
}

type fakeCommand struct {
	name    string
	aliases []string
	admin   bool
}

func (c fakeCommand) Definition() Definition {
	return Definition{Name: c.name, Aliases: c.aliases, AdminOnly: c.admin}
}

func (c fakeCommand) Execute(_ context.Context, req Request) (Result, error) {
	return Result{Text: "ran " + req.Name + ":" + req.ArgsText}, nil
}

type fakeAuthorizer struct {
	admins map[string]bool
}

func testPrincipal(userID string) identity.Principal {
	return identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: "external-" + userID, Assurance: identity.AssuranceDiscordGateway}
}

func (a fakeAuthorizer) IsAdmin(userID string) (bool, error) {
	return a.admins[userID], nil
}
