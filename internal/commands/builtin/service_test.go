package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type profileStarter struct{ owner, key string }

func (r *profileStarter) NewSessionContext(_ context.Context, owner, key string) error {
	r.owner, r.key = owner, key
	return nil
}
func TestProfileCommandsContainNoAdministrationOrMCP(t *testing.T) {
	starter := &profileStarter{}
	service, err := NewProfileService(starter, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions := service.Definitions()
	if len(definitions) != 3 {
		t.Fatal("unexpected command inventory")
	}
	principal := identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
	for _, raw := range []string{"/help", "/help new"} {
		result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Raw: raw})
		if err != nil || !strings.Contains(result.Text, "/new") {
			t.Fatal("help omitted new", err)
		}
		for _, removed := range []string{"/connect", "/bootstrap", "/admin", "/mcp", "/reset"} {
			if strings.Contains(result.Text, removed) {
				t.Fatal("obsolete command exposed", removed)
			}
		}
	}
	if _, err := service.Execute(context.Background(), commands.Request{Principal: principal, SessionKey: "discord:dm:123", Raw: "/new"}); err != nil || starter.owner != "alice" || starter.key != "discord:dm:123" {
		t.Fatal("new used wrong scope", err)
	}
	for _, raw := range []string{"/help unknown", "/admin", "/mcp global", "/reset"} {
		result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Raw: raw})
		if err != nil || !strings.Contains(result.Text, "Unknown command") {
			t.Fatal("removed command admitted", err)
		}
	}
}
