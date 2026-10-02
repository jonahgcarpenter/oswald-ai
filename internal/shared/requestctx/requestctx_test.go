package requestctx

import (
	"context"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"testing"
)

type testExposer struct{ names []string }

func (e *testExposer) ExposeTools(names []string) { e.names = append(e.names, names...) }
func TestPrincipalAndMetadataRoundTrip(t *testing.T) {
	p := identity.Principal{CanonicalUserID: "sender-1", Gateway: "homeassistant", ExternalID: "external-1", Assurance: identity.AssuranceHomeAssistantToken}
	ctx := WithMetadata(WithPrincipal(context.Background(), p), Metadata{RequestID: "req-1", SessionID: "session-1", SessionGeneration: 3})
	meta := MetadataFromContext(ctx)
	got, ok := PrincipalFromContext(ctx)
	if !ok || got != p || meta.RequestID != "req-1" || meta.SessionID != "session-1" || meta.SessionGeneration != 3 {
		t.Fatal("request metadata changed")
	}
}
func TestToolExposerRoundTrip(t *testing.T) {
	e := &testExposer{}
	got := ToolExposerFromContext(WithToolExposer(context.Background(), e))
	if got == nil {
		t.Fatal("missing exposer")
	}
	got.ExposeTools([]string{"tool"})
	if len(e.names) != 1 || e.names[0] != "tool" {
		t.Fatal("discovery collector changed")
	}
}
func TestInputImagesUseDefensiveCopies(t *testing.T) {
	images := []InputImage{{MIMEType: "image/png", Data: "encoded", Source: "source"}}
	ctx := WithInputImages(context.Background(), images)
	images[0].Data = "mutated-input"
	got := InputImagesFromContext(ctx)
	if len(got) != 1 || got[0].Data != "encoded" {
		t.Fatal("caller mutated context images")
	}
	got[0].Data = "mutated-output"
	if InputImagesFromContext(ctx)[0].Data != "encoded" {
		t.Fatal("returned catalog shares context storage")
	}
}
