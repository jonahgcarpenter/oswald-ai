package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

type documentPrincipalRefresh struct{}

func (documentPrincipalRefresh) BanStatus(string) (bool, string, error) { return false, "", nil }
func (documentPrincipalRefresh) ResolvePrincipal(identity.Principal) (string, error) {
	return "refreshed-owner", nil
}

type documentLoaderProcessor struct{ owner chan string }

func (p documentLoaderProcessor) Process(ctx context.Context, req agent.Request) (*agent.Response, error) {
	if req.Prompt != "" {
		return nil, fmt.Errorf("file-only prompt was rewritten")
	}
	loader := requestctx.MetadataFromContext(ctx).DocumentLoader
	if loader == nil {
		return nil, fmt.Errorf("document loader lost through broker")
	}
	_, err := loader.Load(ctx)
	p.owner <- req.Principal.CanonicalUserID
	return &agent.Response{Response: "processing"}, err
}

func TestDocumentLoaderSurvivesBrokerPrincipalRefresh(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	p := documentLoaderProcessor{owner: make(chan string, 1)}
	b := broker.NewBroker(p, 1, log)
	b.Start()
	defer b.Shutdown()
	downloaded := make(chan string, 1)
	req := Request{Principal: testPrincipal("old-owner"), SessionKey: "session", DocumentLoader: &requestctx.DocumentLoader{FileCount: 1, SourceBytes: 20 << 20, Load: func(ctx context.Context) ([]requestctx.DocumentUpload, error) {
		principal, _ := requestctx.PrincipalFromContext(ctx)
		downloaded <- principal.CanonicalUserID
		return nil, nil
	}}}
	out := Execute(req, Dependencies{Broker: b, Access: documentPrincipalRefresh{}, Log: log}, &fakeResponder{})
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	if len(p.owner) != 1 || len(downloaded) != 1 {
		t.Fatal("file-only request did not reach loader")
	}
	if <-p.owner != "refreshed-owner" || <-downloaded != "refreshed-owner" {
		t.Fatal("stale ownership reached document download")
	}
}

func TestDocumentLoaderNotInvokedBeforeAccessChecks(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	deps, shutdown := testDependencies(t, log)
	defer shutdown()
	called := 0
	req := Request{DocumentLoader: &requestctx.DocumentLoader{FileCount: 1, SourceBytes: 20 << 20, Load: func(context.Context) ([]requestctx.DocumentUpload, error) { called++; return nil, nil }}}
	if out := Execute(req, deps, &fakeResponder{}); out.Reason != "invalid_principal" {
		t.Fatalf("unauthenticated=%+v", out)
	}
	req.Principal = testPrincipal("user")
	deps.Access = &fakeAccess{banned: true}
	if out := Execute(req, deps, &fakeResponder{}); out.Reason != "user_banned" {
		t.Fatalf("banned=%+v", out)
	}
	req.IsGroup = true
	req.Principal = identity.Principal{}
	Execute(req, deps, &fakeResponder{})
	if called != 0 {
		t.Fatal("document download before admission")
	}
}
