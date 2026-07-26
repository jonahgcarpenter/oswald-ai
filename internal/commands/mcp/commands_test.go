package mcp

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	mcpmanager "github.com/jonahgcarpenter/oswald-ai/internal/mcp"
)

type fakeAuth struct{ admin bool }

func (a fakeAuth) IsAdmin(canonicalUserID string) (bool, error) {
	return a.admin, nil
}

type fullFakeAuth struct {
	fakeAuth
	principal identity.Principal
}

func (a *fullFakeAuth) IsAdminPrincipal(principal identity.Principal) (bool, error) {
	a.principal = principal
	return a.admin, nil
}

func TestGlobalCommandsRequireAdmin(t *testing.T) {
	h := New(nil, nil, fakeAuth{admin: false})
	result, err := h.Execute(context.Background(), commands.Request{Principal: identity.Principal{CanonicalUserID: "user_1"}, Args: []string{"global", "servers"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Text != "You are not allowed to use admin commands." {
		t.Fatalf("unexpected result: %q", result.Text)
	}
}

func TestGlobalCommandsUsePrincipalAuthorization(t *testing.T) {
	auth := &fullFakeAuth{fakeAuth: fakeAuth{admin: false}}
	principal := identity.Principal{CanonicalUserID: "stale", Gateway: "discord", ExternalID: "current", Assurance: identity.AssuranceDiscordGateway}
	result, err := New(nil, nil, auth).Execute(context.Background(), commands.Request{Principal: principal, Args: []string{"global", "servers"}})
	if err != nil || result.Text != "You are not allowed to use admin commands." || auth.principal != principal {
		t.Fatalf("result=%+v err=%v principal=%+v", result, err, auth.principal)
	}
}

func TestParseHeadersSupportsBearerAndHeader(t *testing.T) {
	headers, err := parseHeaders([]string{"auth-bearer=abc123", "header:X-Test=value"})
	if err != nil {
		t.Fatalf("parse headers: %v", err)
	}
	if headers["Authorization"] != "Bearer abc123" || headers["X-Test"] != "value" {
		t.Fatalf("unexpected headers: %+v", headers)
	}
}

func TestMCPCommandDefinition(t *testing.T) {
	h := New((*mcpmanager.Store)(nil), (*mcpmanager.Manager)(nil), fakeAuth{})
	if h.Definition().Name != "mcp" {
		t.Fatalf("unexpected definition: %+v", h.Definition())
	}
}
