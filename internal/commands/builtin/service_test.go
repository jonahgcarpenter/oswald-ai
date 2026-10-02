package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type profileResetter struct{ owner, key string }

func (r *profileResetter) ResetSessionContext(_ context.Context, owner, key string) error {
	r.owner, r.key = owner, key
	return nil
}
func TestProfileCommandsContainNoAdministrationOrMCP(t *testing.T) {
	resetter := &profileResetter{}
	service, err := NewProfileService(resetter, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions := service.Definitions()
	if len(definitions) != 3 {
		t.Fatal("unexpected command inventory")
	}
	principal := identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
	for _, raw := range []string{"/help", "/help reset"} {
		result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Raw: raw})
		if err != nil || !strings.Contains(result.Text, "/reset") {
			t.Fatal("help omitted reset", err)
		}
		for _, removed := range []string{"/connect", "/bootstrap", "/admin", "/mcp"} {
			if strings.Contains(result.Text, removed) {
				t.Fatal("obsolete command exposed", removed)
			}
		}
	}
	if _, err := service.Execute(context.Background(), commands.Request{Principal: principal, SessionKey: "discord:dm:123", Raw: "/reset"}); err != nil || resetter.owner != "alice" || resetter.key != "discord:dm:123" {
		t.Fatal("reset used wrong scope", err)
	}
	for _, raw := range []string{"/help unknown", "/admin", "/mcp global"} {
		result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Raw: raw})
		if err != nil || !strings.Contains(result.Text, "Unknown command") {
			t.Fatal("removed command admitted", err)
		}
	}
}
