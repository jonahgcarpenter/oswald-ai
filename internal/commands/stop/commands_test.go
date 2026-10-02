package stop

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type fakeCanceler struct {
	owner, key string
	count      int
}

func (c *fakeCanceler) CancelActiveAgentWork(owner, key string) broker.CancelReport {
	c.owner, c.key = owner, key
	return broker.CancelReport{ActiveSignaled: c.count}
}
func TestStopIsConversationScopedAndRejectsAll(t *testing.T) {
	for _, count := range []int{0, 1} {
		c := &fakeCanceler{count: count}
		h := New(c)
		principal := identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
		result, err := h.Execute(context.Background(), commands.Request{Principal: principal, SessionKey: "discord:dm:123"})
		if err != nil || c.owner != "alice" || c.key != "discord:dm:123" || result.Outcome.ActiveCanceledCount != count {
			t.Fatal("incorrect scoped cancellation", err)
		}
		c.owner = ""
		result, err = h.Execute(context.Background(), commands.Request{Principal: principal, Args: []string{"all"}})
		if err != nil || result.Outcome.Status != "rejected" || c.owner != "" {
			t.Fatal("global cancellation admitted", err)
		}
	}
}
func TestStopRejectsMissingPrincipalAndMissingCanceler(t *testing.T) {
	c := &fakeCanceler{}
	result, err := New(c).Execute(context.Background(), commands.Request{})
	if err != nil || result.Outcome.ReasonCode != "authentication_required" || c.owner != "" {
		t.Fatal("unauthenticated cancellation", err)
	}
	principal := identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
	if _, err := New(nil).Execute(context.Background(), commands.Request{Principal: principal}); err == nil {
		t.Fatal("missing canceler ignored")
	}
}
