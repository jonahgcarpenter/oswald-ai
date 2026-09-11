package documents

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type heldCommandDelivery struct {
	gatewayruntime.Responder
	entered chan commands.Result
	release <-chan struct{}
}

func (r heldCommandDelivery) SendCommandResponse(result commands.Result) error {
	r.entered <- result
	<-r.release
	return nil
}

func TestGlobalConfirmationFencesOwnersThroughRuntimeDelivery(t *testing.T) {
	f := newFixture(t)
	f.admin(t)
	f.upload(t, f.actor, 1)
	f.upload(t, f.other, 1)
	code := confirmationCode(t, f.run(t, f.actor, "forget all"))
	id, err := f.h.accounts.EnsureAccount(context.Background(), "homeassistant", "unaffected", "Synthetic")
	if err != nil {
		t.Fatal(err)
	}
	unaffected := identity.Principal{CanonicalUserID: id, Gateway: "homeassistant", ExternalID: "unaffected", Assurance: identity.AssuranceHomeAssistantToken}
	log := config.NewLogger(config.LevelError)
	b := broker.NewBroker(nil, 4, log)
	b.Start()
	t.Cleanup(b.Shutdown)
	release := make(chan struct{})
	var released atomic.Bool
	var once sync.Once
	finishDelivery := func() { once.Do(func() { released.Store(true); close(release) }) }
	t.Cleanup(finishDelivery)
	responder := heldCommandDelivery{entered: make(chan commands.Result, 1), release: release}
	done := make(chan gatewayruntime.Outcome, 1)
	go func() {
		done <- gatewayruntime.Execute(gatewayruntime.Request{
			RequestID: "document-confirm-fence", Principal: f.actor, IsDirect: true,
			SessionKey: "confirm-session", Text: "/documents confirm " + code,
		}, gatewayruntime.Dependencies{Broker: b, Commands: f.service, Access: f.h.accounts, Log: log}, responder)
	}()
	select {
	case result := <-responder.entered:
		if result.Outcome.AffectedCount != 2 || result.Outcome.Status != "ok" {
			t.Fatal(result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("confirmation did not reach delivery")
	}
	started := make(chan struct{}, 2)
	finished := make(chan error, 2)
	var overlapped atomic.Bool
	for _, owner := range []identity.Principal{f.actor, f.other} {
		go func() {
			started <- struct{}{}
			finished <- b.RunInLane(context.Background(), owner, "another-session", func() error {
				if !released.Load() {
					overlapped.Store(true)
				}
				return nil
			})
		}()
	}
	for range 2 {
		<-started
	}
	unaffectedDone := make(chan error, 1)
	go func() {
		unaffectedDone <- b.RunInLane(context.Background(), unaffected, "unrelated", func() error { return nil })
	}()
	select {
	case err := <-unaffectedDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("global confirmation blocked an owner outside its snapshot")
	}
	select {
	case err := <-finished:
		t.Fatalf("owner lane completed before delivery returned: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	finishDelivery()
	select {
	case outcome := <-done:
		if outcome.Err != nil {
			t.Fatal(outcome.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not finish after delivery")
	}
	for range 2 {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("owner fence was not released")
		}
	}
	if overlapped.Load() {
		t.Fatal("owner work overlapped command delivery")
	}
}
