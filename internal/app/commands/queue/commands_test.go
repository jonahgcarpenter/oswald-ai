package queue

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
)

type fakeSubmitter struct {
	got  *broker.Request
	resp broker.Result
	err  error
}

func (f *fakeSubmitter) Submit(req *broker.Request) error {
	f.got = req
	if f.err != nil {
		return f.err
	}
	select {
	case req.ResponseChan <- f.resp:
	default:
	}
	return nil
}

func testPrincipal() identity.Principal {
	return identity.Principal{CanonicalUserID: "user", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
}

func TestQueueDefinition(t *testing.T) {
	def := New(nil).Definition()
	if def.Name != "queue" || !strings.Contains(def.Usage, "/queue") {
		t.Fatalf("definition = %+v", def)
	}
}

func TestQueueRejectsEmptyPrompt(t *testing.T) {
	h := New(&fakeSubmitter{})
	res, err := h.Execute(context.Background(), commands.Request{Principal: testPrincipal(), Raw: "/queue", ArgsText: "   "})
	if err != nil || !strings.Contains(res.Text, "/queue") || res.Outcome.Status != "rejected" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}

func TestQueueForcesFIFOFlag(t *testing.T) {
	sub := &fakeSubmitter{resp: broker.Result{Response: &agent.Response{Response: "queued answer"}}}
	h := New(sub)
	res, err := h.Execute(context.Background(), commands.Request{RequestID: "cmd-1", Principal: testPrincipal(), SessionKey: "s", Raw: "/queue do next", ArgsText: "do next"})
	if err != nil || res.Outcome.Status != "ok" || res.Text != "queued answer" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if sub.got == nil || !sub.got.ForceQueue || sub.got.Prompt != "do next" {
		t.Fatalf("broker request = %+v", sub.got)
	}
}

func TestQueueRejectsQueueFull(t *testing.T) {
	sub := &fakeSubmitter{err: broker.ErrQueueFull}
	h := New(sub)
	res, err := h.Execute(context.Background(), commands.Request{RequestID: "cmd-1", Principal: testPrincipal(), SessionKey: "s", Raw: "/queue hi", ArgsText: "hi"})
	if err != nil || res.Outcome.ReasonCode != "queue_full" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}

func TestQueueRequiresAuthentication(t *testing.T) {
	h := New(&fakeSubmitter{})
	res, err := h.Execute(context.Background(), commands.Request{Raw: "/queue hi", ArgsText: "hi"})
	if err != nil || res.Outcome.ReasonCode != "authentication_required" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	_ = config.SafeText
}
