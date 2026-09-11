package documents

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

type pausedDocumentResolver struct {
	*handler
	resolved chan []string
	release  <-chan struct{}
}

func (h pausedDocumentResolver) ResolveFenceTargets(ctx context.Context, req commands.Request) ([]string, error) {
	owners, err := h.handler.ResolveFenceTargets(ctx, req)
	h.resolved <- append([]string(nil), owners...)
	<-h.release
	return owners, err
}

func TestRuntimeDocumentDeletionCannotBroadenResolvedFences(t *testing.T) {
	for _, scenario := range []string{"promotion", "confirmation_repromotion", "new_owner"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			doc := f.upload(t, f.other, 1)[0]
			raw := "/documents forget " + doc.ID
			if scenario == "promotion" {
				if _, ok, err := f.h.accounts.ClaimBootstrapAdmin(ctx, f.other); err != nil || !ok {
					t.Fatalf("bootstrap=%t err=%v", ok, err)
				}
			} else {
				f.admin(t)
			}
			if scenario == "confirmation_repromotion" {
				f.upload(t, f.actor, 1)
				raw = "/documents confirm " + confirmationCode(t, f.run(t, f.actor, "forget all"))
				if _, err := f.h.accounts.SetAdminAs(ctx, f.actor, f.other.CanonicalUserID, true); err != nil {
					t.Fatal(err)
				}
				if _, err := f.h.accounts.SetAdminAs(ctx, f.other, f.actor.CanonicalUserID, false); err != nil {
					t.Fatal(err)
				}
			}
			before, err := f.h.store.UserDocumentUsage(ctx, memory.DocumentScope{Global: true})
			if err != nil {
				t.Fatal(err)
			}
			log := config.NewLogger(config.LevelError)
			b := broker.NewBroker(nil, 2, log)
			b.Start()
			t.Cleanup(b.Shutdown)
			release := make(chan struct{})
			var once sync.Once
			resume := func() { once.Do(func() { close(release) }) }
			t.Cleanup(resume)
			paused := pausedDocumentResolver{handler: f.h, resolved: make(chan []string, 1), release: release}
			service, err := commands.NewServiceWithCommands(commands.Command{Handler: paused})
			if err != nil {
				t.Fatal(err)
			}
			deliveryRelease := make(chan struct{})
			close(deliveryRelease)
			responder := heldCommandDelivery{entered: make(chan commands.Result, 1), release: deliveryRelease}
			done := make(chan gatewayruntime.Outcome, 1)
			go func() {
				done <- gatewayruntime.Execute(gatewayruntime.Request{
					RequestID: "document-authorization-race", Principal: f.actor, IsDirect: true,
					SessionKey: "race-session", Text: raw,
				}, gatewayruntime.Dependencies{Broker: b, Commands: service, Access: f.h.accounts, Log: log}, responder)
			}()
			select {
			case owners := <-paused.resolved:
				if scenario != "new_owner" && len(owners) != 0 {
					t.Fatalf("private resolution unexpectedly fenced other users: %v", owners)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("resolver did not pause")
			}
			if scenario == "new_owner" {
				owner, err := f.h.accounts.EnsureAccount(ctx, "homeassistant", "new-owner", "Synthetic")
				if err != nil {
					t.Fatal(err)
				}
				db, err := database.Open(f.path, log)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				// Deliberately simulate an ownership move to an owner created
				// after resolution; the deletion transaction must reject it.
				if _, err := db.SQL().Exec(`UPDATE user_documents SET canonical_user_id=? WHERE id=?`, owner, doc.ID); err != nil {
					t.Fatal(err)
				}
			} else if _, err := f.h.accounts.SetAdminAs(ctx, f.other, f.actor.CanonicalUserID, true); err != nil {
				t.Fatal(err)
			}
			resume()
			select {
			case outcome := <-done:
				if outcome.Err != nil {
					t.Fatal(outcome.Err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("command failed to complete")
			}
			result := <-responder.entered
			if result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "fence_required" || result.Outcome.IsChanged || result.Outcome.AffectedCount != 0 {
				t.Fatalf("unfenced result=%+v", result)
			}
			after, err := f.h.store.UserDocumentUsage(ctx, memory.DocumentScope{Global: true})
			if err != nil || after.DocumentCount != before.DocumentCount {
				t.Fatalf("before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

func TestUnscheduledRuntimeDoesNotClaimResolverFences(t *testing.T) {
	f := newFixture(t)
	f.admin(t)
	doc := f.upload(t, f.other, 1)[0]
	release := make(chan struct{})
	close(release)
	responder := heldCommandDelivery{entered: make(chan commands.Result, 1), release: release}
	outcome := gatewayruntime.Execute(gatewayruntime.Request{Principal: f.actor, Text: "/documents forget " + doc.ID, IsDirect: true}, gatewayruntime.Dependencies{Commands: f.service, Access: f.h.accounts, Log: config.NewLogger(config.LevelError)}, responder)
	result := <-responder.entered
	if outcome.Err != nil || result.Outcome.ReasonCode != "fence_required" || result.Outcome.IsChanged {
		t.Fatalf("outcome=%+v result=%+v", outcome, result)
	}
}

func TestFencedDocumentDeletionRollsBackMixedOwnerBatch(t *testing.T) {
	f := newFixture(t)
	one := f.upload(t, f.actor, 1)[0]
	two := f.upload(t, f.other, 1)[0]
	ctx := context.Background()
	scope := memory.DocumentScope{Global: true}
	for _, owners := range [][]string{nil, {}, {f.actor.CanonicalUserID}} {
		out, err := f.h.store.DeleteUserDocumentsFenced(ctx, scope, []string{one.ID, two.ID}, owners)
		if !errors.Is(err, memory.ErrDocumentUnfenced) || out.DeletedCount != 0 || out.SourceBytes != 0 || out.TextBytes != 0 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		u, err := f.h.store.UserDocumentUsage(ctx, scope)
		if err != nil || u.DocumentCount != 2 {
			t.Fatalf("usage=%+v err=%v", u, err)
		}
	}
	out, err := f.h.store.DeleteUserDocumentsFenced(ctx, scope, []string{one.ID, two.ID}, []string{f.actor.CanonicalUserID, f.other.CanonicalUserID})
	if err != nil || out.DeletedCount != 2 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}
