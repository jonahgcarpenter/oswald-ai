package requestctx

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type testExposer struct{ names []string }

func (e *testExposer) ExposeTools(names []string) { e.names = append(e.names, names...) }

func TestPrincipalAndMetadataRoundTrip(t *testing.T) {
	principal := identity.Principal{CanonicalUserID: "sender-1", Gateway: "websocket", ExternalID: "external-1", Assurance: identity.AssuranceSelfAsserted}
	ctx := WithPrincipal(context.Background(), principal)
	ctx = WithMetadata(ctx, Metadata{RequestID: "req-1", SessionID: "session-1"})

	meta := MetadataFromContext(ctx)
	gotPrincipal, ok := PrincipalFromContext(ctx)
	if !ok || gotPrincipal != principal || meta.RequestID != "req-1" || meta.SessionID != "session-1" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
}

func TestToolExposerRoundTrip(t *testing.T) {
	exposer := &testExposer{}
	ctx := WithToolExposer(context.Background(), exposer)
	got := ToolExposerFromContext(ctx)
	if got == nil {
		t.Fatal("expected exposer")
	}
	got.ExposeTools([]string{"tool"})
	if len(exposer.names) != 1 || exposer.names[0] != "tool" {
		t.Fatalf("unexpected exposer calls: %+v", exposer.names)
	}
}
