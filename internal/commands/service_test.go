package commands

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func testPrincipal(owner string) identity.Principal {
	return identity.Principal{CanonicalUserID: owner, Gateway: "discord", ExternalID: "synthetic", Assurance: identity.AssuranceDiscordGateway}
}

type fakeCommand struct {
	name    string
	aliases []string
}

func (c fakeCommand) Definition() Definition { return Definition{Name: c.name, Aliases: c.aliases} }
func (c fakeCommand) Execute(_ context.Context, request Request) (Result, error) {
	return Result{Text: "ran " + request.Name + ":" + request.ArgsText}, nil
}

func TestServiceKnownUnknownAndPrincipalValidation(t *testing.T) {
	service, err := NewServiceWithCommands(Command{Handler: fakeCommand{name: "ping"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(context.Background(), Request{Principal: testPrincipal("alice"), Raw: "/ping one two"})
	if err != nil || result.Text != "ran ping:one two" {
		t.Fatal("known command failed", err)
	}
	result, err = service.Execute(context.Background(), Request{Principal: testPrincipal("alice"), Raw: "/missing"})
	if err != nil || result.Outcome.Status != "rejected" || result.Text != "Unknown command: /missing" {
		t.Fatal("unknown command admitted", err)
	}
	for _, principal := range []identity.Principal{{}, {CanonicalUserID: "alice", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceSelfAsserted}} {
		if _, err := service.Execute(context.Background(), Request{Principal: principal, Raw: "/ping"}); err == nil {
			t.Fatal("invalid principal admitted")
		}
	}
}

func TestServicePropagatesPrincipalAndRejectsDuplicateRegistrations(t *testing.T) {
	want := testPrincipal("alice")
	var got identity.Principal
	service, err := NewServiceWithCommands(Command{Handler: HandlerFunc{DefinitionValue: Definition{Name: "principal"}, ExecuteFunc: func(_ context.Context, req Request) (Result, error) { got = req.Principal; return Result{}, nil }}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), Request{Principal: want, Raw: "/principal"}); err != nil || got != want {
		t.Fatal("principal changed", err)
	}
	if _, err := NewServiceWithCommands(Command{Handler: fakeCommand{name: "ping"}}, Command{Handler: fakeCommand{name: "ping"}}); !errors.Is(err, ErrDuplicateCommand) {
		t.Fatal("duplicate command accepted", err)
	}
	if _, err := NewServiceWithCommands(Command{Handler: fakeCommand{name: "ping", aliases: []string{"p"}}}, Command{Handler: fakeCommand{name: "pong", aliases: []string{"p"}}}); !errors.Is(err, ErrDuplicateAlias) {
		t.Fatal("duplicate alias accepted", err)
	}
}

func TestServiceAliasesExclusivityAndPreMiddlewareFenceResolution(t *testing.T) {
	middlewareCalled := false
	service, err := NewServiceWithCommands(Command{Handler: HandlerFunc{DefinitionValue: Definition{Name: "refresh", Aliases: []string{"reload"}, UserExclusive: true}, ExecuteFunc: func(context.Context, Request) (Result, error) { return Result{}, nil }, ResolveFenceTargetsFunc: func(_ context.Context, req Request) ([]string, error) {
		if middlewareCalled || req.Name != "refresh" || req.ArgsText != "target" || len(req.Args) != 1 {
			t.Fatal("fence resolution ordering changed")
		}
		return req.Args, nil
	}}, Middleware: []Middleware{func(next Handler) Handler {
		return HandlerFunc{DefinitionValue: next.Definition(), ExecuteFunc: func(ctx context.Context, req Request) (Result, error) {
			middlewareCalled = true
			return next.Execute(ctx, req)
		}}
	}}})
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := service.Definition("/reload")
	if !ok || definition.Name != "refresh" || !definition.UserExclusive {
		t.Fatal("alias metadata lost")
	}
	if _, ok := service.Definition("missing"); ok {
		t.Fatal("unknown definition returned")
	}
	targets, err := service.ResolveFenceTargets(context.Background(), Request{Principal: testPrincipal("alice"), Raw: "/reload target"})
	if err != nil || len(targets) != 1 || targets[0] != "target" {
		t.Fatal("fence target parsing failed", err)
	}
	if _, err := service.Execute(context.Background(), Request{Principal: testPrincipal("alice"), Raw: "/reload target"}); err != nil || !middlewareCalled {
		t.Fatal("middleware omitted", err)
	}
}

func TestServiceOutcomesAndCanonicalTelemetryNames(t *testing.T) {
	failure := errors.New("synthetic storage failure")
	service, err := NewServiceWithCommands(Command{Handler: HandlerFunc{DefinitionValue: Definition{Name: "reset", Aliases: []string{"clear"}}, ExecuteFunc: func(context.Context, Request) (Result, error) { return Result{}, failure }}})
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{"clear": "reset", "/reset": "reset", "private-input": "unknown"} {
		if got := service.CanonicalName(input); got != want {
			t.Fatal("unsafe telemetry name", got)
		}
	}
	result, err := service.Execute(context.Background(), Request{Principal: testPrincipal("alice"), Raw: "/clear"})
	if !errors.Is(err, failure) || result.Outcome.Status != "error" || result.Outcome.IsChanged {
		t.Fatal("operation failure lost", err)
	}
	encoded, err := json.Marshal(Result{Text: "response", Outcome: Outcome{Operation: "private-operation", IsChanged: true}})
	if err != nil || strings.Contains(string(encoded), "private-operation") || strings.Contains(string(encoded), "Outcome") {
		t.Fatal("internal outcome exposed", err)
	}
	service, err = NewServiceWithCommands(Command{Handler: HandlerFunc{DefinitionValue: Definition{Name: "stop"}, ExecuteFunc: func(context.Context, Request) (Result, error) {
		return Result{Outcome: Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err = service.Execute(context.Background(), Request{Principal: testPrincipal("alice"), Raw: "/stop all"})
	if err != nil || result.Outcome.Status != "rejected" || result.Outcome.IsChanged {
		t.Fatal("expected rejection became failure", err)
	}
}
